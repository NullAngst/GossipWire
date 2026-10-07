package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/google/uuid"

	"github.com/NullAngst/GossipWire/internal/config"
	"github.com/NullAngst/GossipWire/internal/fed"
	"github.com/NullAngst/GossipWire/internal/store"
)

const draftLifetime = 15 * time.Minute

// MaxReason is the reason length cap.
const MaxReason = 500

// Draft is an alert waiting for the sender to confirm it.
type Draft struct {
	ID           string
	Actor        Actor
	TargetID     string
	TargetName   string
	TargetAvatar string
	Reason       string
	Link         string
	EvidenceName string
	EvidenceType string
	Evidence     []byte
	Created      time.Time
	OverLimit    string // set when sending now would put the server over its rate limit
}

// Drafts live in memory until confirmed, so they are capped. A mod running
// /alert over and over with a large attachment could otherwise fill RAM.
const (
	maxDrafts        = 200
	maxDraftEvidence = 256 << 20
)

// AttachmentIn is the evidence attachment from the slash command.
type AttachmentIn struct {
	Name        string
	ContentType string
	Size        int
	URL         string
}

var messageLinkRe = regexp.MustCompile(`^https://(?:(?:ptb|canary)\.)?discord(?:app)?\.com/channels/(?:[0-9]{17,20}|@me)/[0-9]{17,20}/[0-9]{17,20}$`)

// PrepareAlert checks an /alert, resolves the target, copies the evidence and
// returns a draft for the confirm card.
func (a *App) PrepareAlert(ctx context.Context, act Actor, channelID, userID, reason, link string, att *AttachmentIn) (*Draft, error) {
	g, err := a.requireSetup(ctx, act.GuildID)
	if err != nil {
		return nil, err
	}
	if g.ChannelID != channelID {
		return nil, userErr("Run /alert in this server's alert channel, <#%s>. That is the channel alerts go out from and come in to.", g.ChannelID)
	}
	userID = strings.TrimSpace(userID)
	if !config.IsSnowflake(userID) {
		return nil, userErr("`%s` is not a Discord user ID. It should be a long number. Turn on Developer Mode, right-click the user and pick Copy User ID.", truncate(userID, 40))
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil, userErr("Give a reason, so receiving mods know what this is about.")
	}
	if len([]rune(reason)) > MaxReason {
		return nil, userErr("The reason is over %d characters. Shorten it and put the detail in the evidence.", MaxReason)
	}
	link = strings.TrimSpace(link)
	if link != "" && !messageLinkRe.MatchString(link) {
		return nil, userErr("`link` should be a Discord message link (right-click the message, Copy Message Link).")
	}
	overLimit := ""
	if err := a.checkRate(ctx, act.GuildID); err != nil {
		var ue *UserError
		if !errors.As(err, &ue) {
			return nil, err
		}
		// Warn on the confirm card. Sending still works; the alert is held.
		overLimit = ue.Msg
	}
	u, err := a.DC.User(ctx, userID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, userErr("No Discord account has the ID `%s`. Check the number and try again.", userID)
		}
		return nil, err
	}
	d := &Draft{
		ID: uuid.NewString(), Actor: act, TargetID: userID, TargetName: cleanName(u.Username),
		TargetAvatar: u.EffectiveAvatarURL(), Reason: reason, Link: link, Created: a.Now(), OverLimit: overLimit,
	}
	if att != nil {
		limit := int64(a.St.GetLimits(ctx).EvidenceMB) << 20
		if int64(att.Size) > limit {
			return nil, userErr("The evidence file is over this instance's %d MB cap. Crop the screenshot or link the message instead.", limit>>20)
		}
		data, err := a.FetchEvidence(ctx, att.URL, limit)
		if err != nil {
			a.Log.Warn("evidence download failed", "err", err)
			return nil, userErr("I could not copy the evidence file from Discord. Try attaching it again.")
		}
		kind := evidenceKind(data)
		if kind == "" {
			return nil, userErr("Evidence has to be an image (PNG, JPEG, GIF, WebP), a video (MP4, WebM) or a plain text file. " +
				"The bot re-posts it in other servers, so it does not pass on anything else.")
		}
		d.Evidence = data
		d.EvidenceName = evidenceName(att.Name, kind)
		d.EvidenceType = kind
	}
	a.draftsMu.Lock()
	defer a.draftsMu.Unlock()
	held := 0
	for k, x := range a.drafts {
		// Expired drafts go, and so does this person's previous draft:
		// one pending confirm card per mod is plenty.
		if a.Now().Sub(x.Created) > draftLifetime || (x.Actor.UserID == act.UserID && x.Actor.GuildID == act.GuildID) {
			delete(a.drafts, k)
			continue
		}
		held += len(x.Evidence)
	}
	if len(a.drafts) >= maxDrafts || held+len(d.Evidence) > maxDraftEvidence {
		return nil, userErr("The bot is holding too many unconfirmed alerts right now. Try again in a few minutes.")
	}
	a.drafts[d.ID] = d
	return d, nil
}

// evidenceTypes are the files GossipWire will re-post in other servers,
// keyed by sniffed content type. The type comes from the bytes, never from
// the uploader's file name or content type.
var evidenceTypes = map[string]string{
	"image/png":                 "png",
	"image/jpeg":                "jpg",
	"image/gif":                 "gif",
	"image/webp":                "webp",
	"video/mp4":                 "mp4",
	"video/webm":                "webm",
	"text/plain; charset=utf-8": "txt",
}

// evidenceKind returns the sniffed content type if it is allowed, or "".
func evidenceKind(data []byte) string {
	ct := http.DetectContentType(data)
	if _, ok := evidenceTypes[ct]; ok {
		return ct
	}
	return ""
}

// evidenceName keeps the uploader's base name but forces the extension to
// match the sniffed type, so "shot.png.exe" cannot be passed around.
func evidenceName(name, kind string) string {
	base := safeFileName(name)
	if i := strings.Index(base, "."); i > 0 {
		base = base[:i]
	}
	if base == "" || base == "." {
		base = "evidence"
	}
	return truncate(base, 60) + "." + evidenceTypes[kind]
}

// discordCDN reports whether an attachment URL points at Discord's file servers.
func discordCDN(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && (u.Host == "cdn.discordapp.com" || u.Host == "media.discordapp.net")
}

// downloadFromCDN fetches an attachment, and only from Discord's file servers.
func downloadFromCDN(ctx context.Context, url string, limit int64) ([]byte, error) {
	if !discordCDN(url) {
		return nil, fmt.Errorf("attachment URL is not on Discord's CDN")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := cdnClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("evidence over the size cap")
	}
	return b, nil
}

var cdnClient = &http.Client{Timeout: 60 * time.Second,
	CheckRedirect: func(r *http.Request, _ []*http.Request) error {
		if !discordCDN(r.URL.String()) {
			return errors.New("redirect off Discord's CDN")
		}
		return nil
	}}

// DraftCard is the ephemeral confirm card.
func (d *Draft) Card() (discord.Embed, []discord.LayoutComponent) {
	acct := accountCreated(d.TargetID)
	e := discord.Embed{
		Title:       "Send this alert?",
		Color:       colorInfo,
		Description: "Check the username and avatar. This goes to every server that follows yours.",
		Fields: []discord.EmbedField{
			{Name: "Target", Value: fmt.Sprintf("%s\n`%s`", md(d.TargetName), d.TargetID), Inline: boolPtr(true)},
			{Name: "Account created", Value: ts(acct.Unix(), "R"), Inline: boolPtr(true)},
			{Name: "Reason", Value: truncate(md(d.Reason), 1024)},
		},
		Thumbnail: &discord.EmbedResource{URL: d.TargetAvatar},
	}
	var ev []string
	if d.EvidenceName != "" {
		ev = append(ev, fmt.Sprintf("`%s` (%d KB)", d.EvidenceName, (len(d.Evidence)+1023)/1024))
	}
	if d.Link != "" {
		ev = append(ev, d.Link)
	}
	if len(ev) > 0 {
		e.Fields = append(e.Fields, discord.EmbedField{Name: "Evidence", Value: strings.Join(ev, "\n")})
	}
	if d.OverLimit != "" {
		e.Fields = append(e.Fields, discord.EmbedField{Name: "Over the limit",
			Value: d.OverLimit + " If you send this, it is held until the server owner releases it."})
	}
	return e, []discord.LayoutComponent{discord.NewActionRow(
		discord.NewDangerButton("Send alert", "gw:send:"+d.ID),
		discord.NewSecondaryButton("Cancel", "gw:cancel:"+d.ID),
	)}
}

// CancelDraft drops a draft, if it is this person's.
func (a *App) CancelDraft(id string, act Actor) {
	a.draftsMu.Lock()
	if d := a.drafts[id]; d != nil && d.Actor.UserID == act.UserID && d.Actor.GuildID == act.GuildID {
		delete(a.drafts, id)
	}
	a.draftsMu.Unlock()
}

// checkRate returns a UserError when a server is over its limits.
func (a *App) checkRate(ctx context.Context, guild string) error {
	l := a.St.GetLimits(ctx)
	now := a.Now()
	hour, err := a.St.CountSends(ctx, guild, a.Self(), now.Add(-time.Hour).Unix())
	if err != nil {
		return err
	}
	if hour >= l.PerHour {
		return userErr("This server has sent or forwarded %d alerts in the last hour. The limit is %d per hour.", hour, l.PerHour)
	}
	day, err := a.St.CountSends(ctx, guild, a.Self(), now.Add(-24*time.Hour).Unix())
	if err != nil {
		return err
	}
	if day >= l.PerDay {
		return userErr("This server has sent or forwarded %d alerts in the last 24 hours. The limit is %d per day.", day, l.PerDay)
	}
	return nil
}

// SendDraft turns a confirmed draft into an alert and fans it out.
func (a *App) SendDraft(ctx context.Context, draftID string, act Actor) (string, error) {
	a.draftsMu.Lock()
	d := a.drafts[draftID]
	switch {
	case d == nil || a.Now().Sub(d.Created) > draftLifetime:
		delete(a.drafts, draftID)
		a.draftsMu.Unlock()
		return "", userErr("This draft expired. Run /alert again.")
	case d.Actor.GuildID != act.GuildID || d.Actor.UserID != act.UserID:
		a.draftsMu.Unlock() // not theirs to send, and not theirs to throw away either
		return "", userErr("This draft belongs to someone else.")
	}
	delete(a.drafts, draftID) // taken under the lock, so a double click sends once
	a.draftsMu.Unlock()
	if _, err := a.requireSetup(ctx, act.GuildID); err != nil {
		return "", err
	}
	rateErr := a.checkRate(ctx, act.GuildID)
	var ue *UserError
	if rateErr != nil && !errors.As(rateErr, &ue) {
		return "", rateErr
	}

	now := a.Now()
	al := &fed.Alert{
		AlertID: uuid.NewString(), OriginGuild: act.GuildID, OriginGuildName: cleanName(a.guildName(act.GuildID, act.GuildName)),
		OriginHost: a.Self(), OriginKey: a.ID.PubB64(), SenderID: act.UserID, SenderName: cleanName(act.UserName),
		TargetID: d.TargetID, TargetName: d.TargetName, TargetAvatar: d.TargetAvatar, Reason: d.Reason, Link: d.Link,
		CreatedAt: now.Unix(),
	}
	if d.EvidenceName != "" {
		sum := sha256.Sum256(d.Evidence)
		al.Evidence = &fed.EvidenceMeta{Name: d.EvidenceName, ContentType: d.EvidenceType, Size: int64(len(d.Evidence)), SHA256: hex.EncodeToString(sum[:])}
	}
	signed, err := a.ID.SignAlert(al)
	if err != nil {
		return "", err
	}
	row := alertRow(al, signed, true)
	if rateErr != nil {
		row.Status = store.AlertHeld
	}
	if err := a.St.InsertAlert(ctx, row); err != nil {
		return "", err
	}
	if len(d.Evidence) > 0 {
		if err := a.saveEvidence(ctx, &row, d.Evidence); err != nil {
			return "", err
		}
	}
	a.Log.Info("alert created", "alert", al.AlertID, "guild", act.GuildID, "sender", act.UserID, "target", al.TargetID, "held", rateErr != nil)

	if rateErr != nil {
		a.notifyHeld(ctx, row, act)
		return fmt.Sprintf("%s\nThis alert (`%s`) is held, not sent. The server owner got a DM to release or discard it.", ue.Msg, al.AlertID), nil
	}
	res := a.fanout(ctx, row, act.GuildID, nil)
	return res.Summary(al.AlertID), nil
}

func alertRow(al *fed.Alert, signed *fed.SignedAlert, verified bool) store.Alert {
	// The signed alert is kept byte for byte so it can be forwarded, but what
	// gets shown is checked here: a peer can put anything in these fields.
	r := store.Alert{
		ID: al.AlertID, OriginGuild: al.OriginGuild, OriginHost: al.OriginHost, OriginGuildName: cleanName(al.OriginGuildName),
		SenderID: al.SenderID, SenderName: cleanName(al.SenderName), TargetID: al.TargetID, TargetName: cleanName(al.TargetName),
		Reason: truncate(strings.TrimSpace(al.Reason), MaxReason), CreatedAt: al.CreatedAt,
		Status: store.AlertSent, OriginVerified: verified, Signed: signed.Alert, Sig: signed.Sig,
	}
	if discordCDN(al.TargetAvatar) {
		r.TargetAvatar = al.TargetAvatar
	}
	if messageLinkRe.MatchString(al.Link) {
		r.EvidenceLink = al.Link
	}
	if al.Evidence != nil {
		r.EvidenceName = safeFileName(al.Evidence.Name)
		r.EvidenceSize = al.Evidence.Size
		r.EvidenceSHA = al.Evidence.SHA256
	}
	return r
}

func (a *App) saveEvidence(ctx context.Context, row *store.Alert, data []byte) error {
	kind := evidenceKind(data)
	if kind == "" {
		return errors.New("evidence type not allowed")
	}
	name := evidenceName(row.EvidenceName, kind)
	dir := a.evidenceDir(row.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		return err
	}
	row.EvidencePath, row.EvidenceName, row.EvidenceType = p, name, kind
	return a.St.SetEvidenceFile(ctx, row.ID, p, name, kind)
}

func (a *App) notifyHeld(ctx context.Context, row store.Alert, act Actor) {
	name, owner, ok := a.DC.Guild(act.GuildID)
	if !ok {
		name = row.OriginGuildName
	}
	msg := discord.MessageCreate{
		Content: fmt.Sprintf("A GossipWire alert from **%s** was held because the server is over its rate limit.\n"+
			"Sent by <@%s> about `%s` (%s). Reason: %s\n"+
			"If this is a burst you expected, release it. If not, a mod account may be compromised: discard it and check who has Manage Server and Ban Members.",
			name, row.SenderID, row.TargetID, row.TargetName, truncate(row.Reason, 300)),
		Components: []discord.LayoutComponent{discord.NewActionRow(
			discord.NewDangerButton("Release", "gw:release:"+row.ID),
			discord.NewSecondaryButton("Discard", "gw:discard:"+row.ID),
		)},
		AllowedMentions: noMentions(),
	}
	if ok && owner != "" {
		if err := a.DC.DM(ctx, owner, msg); err != nil {
			a.Log.Warn("could not DM the server owner about a held alert", "guild", act.GuildID, "err", err)
		}
	}
	if a.Cfg.OwnerID != owner {
		if err := a.DC.DM(ctx, a.Cfg.OwnerID, plain(fmt.Sprintf("FYI: an alert from %s (`%s`) was held for going over the rate limit. Its server owner decides whether to release it.", md(name), act.GuildID))); err != nil {
			a.Log.Warn("could not DM the instance owner about a held alert", "err", err)
		}
	}
}

// ReleaseHeld sends or discards a held alert. Only the origin server's owner may.
func (a *App) ReleaseHeld(ctx context.Context, alertID, userID string, release bool) (string, error) {
	al, err := a.St.GetAlert(ctx, alertID)
	if err != nil {
		return "", err
	}
	if al == nil || al.Status != store.AlertHeld {
		return "This alert is no longer held. It was already released, discarded or expired.", nil
	}
	_, owner, ok := a.DC.Guild(al.OriginGuild)
	if !ok || owner != userID {
		return "", userErr("Only the owner of the server that sent this alert can release or discard it.")
	}
	if !release {
		if err := a.St.DeleteAlert(ctx, alertID); err != nil {
			return "", err
		}
		os.RemoveAll(a.evidenceDir(alertID))
		a.Log.Info("held alert discarded", "alert", alertID, "by", userID)
		return "Discarded. Nothing was sent.", nil
	}
	if fed.Expired(al.CreatedAt, a.Now()) {
		return "This alert is past its 14 days and can no longer be sent.", nil
	}
	if err := a.St.SetAlertStatus(ctx, alertID, store.AlertSent); err != nil {
		return "", err
	}
	al.Status = store.AlertSent
	a.Log.Info("held alert released", "alert", alertID, "by", userID)
	res := a.fanout(ctx, *al, al.OriginGuild, nil)
	return "Released. " + res.Summary(alertID), nil
}

// ---------------------------------------------------------------- fan-out

// FanoutResult summarizes one send or forward.
type FanoutResult struct {
	Local    int
	Failed   map[string]int // reason -> count
	Remote   int
	Hosts    map[string]bool
	Skipped  int
	NoTarget bool
}

// Summary is the line the sender sees.
func (r *FanoutResult) Summary(alertID string) string {
	if r.NoTarget {
		return fmt.Sprintf("Alert `%s` saved, but no server follows yours yet, so it went nowhere. Others subscribe with your server's address.", alertID)
	}
	failed := 0
	var reasons []string
	for k, n := range r.Failed {
		failed += n
		reasons = append(reasons, fmt.Sprintf("%d %s", n, k))
	}
	sort.Strings(reasons)
	var parts []string
	total := r.Local + failed
	if total > 0 {
		s := fmt.Sprintf("Sent to %d of %d servers on this instance", r.Local, total)
		if failed > 0 {
			s += fmt.Sprintf(", %d failed: %s", failed, strings.Join(reasons, ", "))
		}
		parts = append(parts, s+".")
	}
	if r.Remote > 0 {
		parts = append(parts, fmt.Sprintf("%d queued for %d other instance(s).", r.Remote, len(r.Hosts)))
	}
	if r.Skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped (already had it, paused, or not set up).", r.Skipped))
	}
	return fmt.Sprintf("Alert `%s`: %s", alertID, strings.Join(parts, " "))
}

// fanout sends an alert from fromGuild (this instance) to its subscribers.
// path is the forward chain up to and including fromGuild when this is a
// forward, nil for an original send.
func (a *App) fanout(ctx context.Context, al store.Alert, fromGuild string, path []string) *FanoutResult {
	res := &FanoutResult{Failed: map[string]int{}, Hosts: map[string]bool{}}
	subs, err := a.St.ActiveSubscribers(ctx, fromGuild, a.Self())
	if err != nil {
		a.Log.Warn("fan-out: listing subscribers failed", "err", err)
		return res
	}
	if len(subs) == 0 {
		res.NoTarget = true
		return res
	}
	now := a.Now().Unix()
	remote := map[string][]string{}
	for _, s := range subs {
		if s.SubGuild == al.OriginGuild && s.SubHost == al.OriginHost {
			res.Skipped++ // the origin already has it
			continue
		}
		fresh, err := a.St.AddRecipient(ctx, al.ID, s.SubGuild, s.SubHost, fromGuild, a.Self(), now)
		if err != nil {
			res.Failed["database error"]++
			continue
		}
		if !fresh {
			res.Skipped++
			continue
		}
		if s.SubHost == a.Self() {
			status, reason := a.deliverLocal(ctx, &al, s.SubGuild, path, a.Self())
			switch status {
			case deliverOK:
				res.Local++
			case deliverSkipped:
				res.Skipped++
			default:
				res.Failed[reason]++
			}
			continue
		}
		if !a.peerUsable(ctx, s.SubHost) {
			res.Failed["peer instance not allowed"]++
			continue
		}
		remote[s.SubHost] = append(remote[s.SubHost], s.SubGuild)
	}
	if len(remote) > 0 {
		msg, typ, err := a.alertMessage(ctx, &al, path)
		if err != nil {
			a.Log.Warn("fan-out: building federation message failed", "alert", al.ID, "err", err)
			for _, gs := range remote {
				res.Failed["could not build message"] += len(gs)
			}
			return res
		}
		for host, guilds := range remote {
			m := *msg
			m.Targets = guilds
			if err := a.enqueue(ctx, host, typ, &m); err != nil {
				res.Failed["could not queue"] += len(guilds)
				continue
			}
			res.Remote += len(guilds)
			res.Hosts[host] = true
		}
	}
	return res
}

// alertMessage builds the federation body for an alert along a path.
func (a *App) alertMessage(ctx context.Context, al *store.Alert, path []string) (*fed.AlertMessage, string, error) {
	m := &fed.AlertMessage{Alert: fed.SignedAlert{Alert: al.Signed, Sig: al.Sig}}
	typ := fed.TypeAlert
	if len(path) > 0 {
		typ = fed.TypeAlertForward
		hops, err := a.St.HopsByKey(ctx, al.ID)
		if err != nil {
			return nil, "", err
		}
		for _, k := range path {
			h, ok := hops[k]
			if !ok {
				return nil, "", fmt.Errorf("hop %s missing", k)
			}
			m.Hops = append(m.Hops, fed.Hop{AlertID: h.AlertID, N: h.N, GuildID: h.GuildID, GuildName: h.GuildName, Host: h.Host,
				ByID: h.ByID, ByName: h.ByName, At: h.At, Sig: h.Sig})
		}
	}
	if al.EvidencePath != "" {
		b, err := os.ReadFile(al.EvidencePath)
		if err == nil {
			m.Evidence = b
		}
	}
	return m, typ, nil
}

const (
	deliverOK = iota
	deliverSkipped
	deliverFailed
)

// deliverLocal posts an alert card into a server on this instance.
func (a *App) deliverLocal(ctx context.Context, al *store.Alert, guild string, path []string, viaHost string) (int, string) {
	g, err := a.St.GetGuild(ctx, guild)
	if err != nil {
		return deliverFailed, "database error"
	}
	if g == nil || g.Paused || al.RetractedAt > 0 || fed.Expired(al.CreatedAt, a.Now()) {
		return deliverSkipped, ""
	}
	d := store.Delivery{AlertID: al.ID, GuildID: guild, ChannelID: g.ChannelID, Status: store.DeliverySending, Path: path, ViaHost: viaHost, At: a.Now().Unix()}
	if err := a.St.ClaimDelivery(ctx, d); err != nil {
		if errors.Is(err, store.ErrDuplicate) {
			return deliverSkipped, ""
		}
		return deliverFailed, "database error"
	}
	in, err := a.DC.IsMember(ctx, guild, al.TargetID)
	if err != nil {
		a.Log.Debug("member check failed", "guild", guild, "err", err)
	}
	d.InServer = in
	if err := a.St.FinishDelivery(ctx, d); err != nil {
		return deliverFailed, "database error"
	}
	cs, err := a.loadCard(ctx, al.ID, guild)
	if err != nil || cs == nil {
		return deliverFailed, "database error"
	}
	e, comps := cs.build()
	mc := discord.MessageCreate{Embeds: []discord.Embed{e}, Components: comps, AllowedMentions: noMentions()}
	file, closeFile := a.evidenceFile(al)
	defer closeFile()
	if file != nil {
		mc.Files = []*discord.File{file}
	}
	msgID, err := a.DC.Send(ctx, g.ChannelID, mc)
	if err != nil && file != nil && !errors.Is(err, ErrForbidden) {
		// A file over the server's upload cap fails the whole message.
		// Send the card without it rather than lose the alert.
		mc.Files = nil
		msgID, err = a.DC.Send(ctx, g.ChannelID, mc)
	}
	if err != nil {
		d.Status = store.DeliveryFailed
		reason := "could not post"
		if errors.Is(err, ErrForbidden) {
			reason = "missing permissions"
		} else if errors.Is(err, ErrNotFound) {
			reason = "alert channel deleted"
		}
		d.Error = truncate(err.Error(), 300)
		a.St.FinishDelivery(ctx, d)
		a.Log.Info("delivery failed", "alert", al.ID, "guild", guild, "err", err)
		return deliverFailed, reason
	}
	d.Status = store.DeliverySent
	d.MessageID = msgID
	if err := a.St.FinishDelivery(ctx, d); err != nil {
		return deliverFailed, "database error"
	}
	return deliverOK, ""
}

// ---------------------------------------------------------------- buttons

// ErrExpired means the alert behind a card was purged.
var ErrExpired = errors.New("alert expired")

// Press handles Ban, Ignore and Forward on a card. The caller has already
// checked the presser holds Manage Server and Ban Members in this server.
// The returned text, if any, goes back to the presser privately.
func (a *App) Press(ctx context.Context, action, alertID string, act Actor) (string, error) {
	al, err := a.St.GetAlert(ctx, alertID)
	if err != nil {
		return "", err
	}
	if al == nil || fed.Expired(al.CreatedAt, a.Now()) {
		return "", ErrExpired
	}
	d, err := a.St.GetDelivery(ctx, alertID, act.GuildID)
	if err != nil {
		return "", err
	}
	if d == nil {
		return "", userErr("This alert was not delivered to this server.")
	}
	if al.RetractedAt > 0 {
		a.refreshCard(ctx, alertID, act.GuildID)
		return "", userErr("The sending server retracted this alert.")
	}
	switch action {
	case store.ActBan, store.ActIgnore:
		return a.decide(ctx, al, act, action)
	case store.ActForward:
		return a.forward(ctx, al, d, act)
	}
	return "", fmt.Errorf("unknown action %q", action)
}

func (a *App) decide(ctx context.Context, al *store.Alert, act Actor, action string) (string, error) {
	now := a.Now().Unix()
	err := a.St.InsertAction(ctx, store.Action{AlertID: al.ID, GuildID: act.GuildID, ModeratorID: act.UserID, ModeratorName: act.UserName, Action: action, At: now})
	if errors.Is(err, store.ErrDuplicate) {
		acts, _ := a.St.ActionsFor(ctx, al.ID, act.GuildID)
		for _, x := range acts {
			if x.Action == store.ActBan || x.Action == store.ActIgnore {
				return "", userErr("Already handled by <@%s> (%s).", x.ModeratorID, x.Action)
			}
		}
		return "", userErr("Already handled.")
	}
	if err != nil {
		return "", err
	}
	if action == store.ActBan {
		reason := truncate(fmt.Sprintf("GossipWire from %s via %s: %s", al.OriginGuildName, al.OriginHost, al.Reason), 512)
		if err := a.DC.Ban(ctx, act.GuildID, al.TargetID, reason); err != nil {
			a.St.DeleteAction(ctx, al.ID, act.GuildID, store.ActBan)
			if errors.Is(err, ErrForbidden) {
				return "", userErr("I could not ban `%s`. My role needs Ban Members and has to sit above the user's highest role (Server Settings > Roles). Fix that and press Ban again.", al.TargetID)
			}
			a.Log.Warn("ban failed", "alert", al.ID, "guild", act.GuildID, "err", err)
			return "", userErr("Discord did not accept the ban. Try again in a moment.")
		}
		a.Log.Info("banned from alert", "alert", al.ID, "guild", act.GuildID, "target", al.TargetID, "by", act.UserID)
	} else {
		a.Log.Info("alert ignored", "alert", al.ID, "guild", act.GuildID, "by", act.UserID)
	}
	a.refreshCard(ctx, al.ID, act.GuildID)
	return "", nil
}

func (a *App) forward(ctx context.Context, al *store.Alert, d *store.Delivery, act Actor) (string, error) {
	if err := a.checkRate(ctx, act.GuildID); err != nil {
		var ue *UserError
		if errors.As(err, &ue) {
			return "", userErr("%s Forwards count toward it. Try again later.", ue.Msg)
		}
		return "", err
	}
	now := a.Now()
	err := a.St.InsertAction(ctx, store.Action{AlertID: al.ID, GuildID: act.GuildID, ModeratorID: act.UserID, ModeratorName: act.UserName, Action: store.ActForward, At: now.Unix()})
	if errors.Is(err, store.ErrDuplicate) {
		return "", userErr("This server already forwarded this alert.")
	}
	if err != nil {
		return "", err
	}
	h := fed.Hop{AlertID: al.ID, N: len(d.Path) + 1, GuildID: act.GuildID, GuildName: cleanName(a.guildName(act.GuildID, act.GuildName)),
		Host: a.Self(), ByID: act.UserID, ByName: cleanName(act.UserName), At: now.Unix()}
	a.ID.SignHop(&h)
	if err := a.St.InsertHop(ctx, store.Hop{AlertID: h.AlertID, N: h.N, GuildID: h.GuildID, Host: h.Host, GuildName: h.GuildName,
		ByID: h.ByID, ByName: h.ByName, Sig: h.Sig, At: h.At}); err != nil {
		return "", err
	}
	path := append(append([]string{}, d.Path...), h.Key())
	res := a.fanout(ctx, *al, act.GuildID, path)
	a.refreshCard(ctx, al.ID, act.GuildID)
	a.Log.Info("alert forwarded", "alert", al.ID, "guild", act.GuildID, "by", act.UserID)
	return "Forwarded. " + strings.TrimPrefix(res.Summary(al.ID), "Alert `"+al.ID+"`: "), nil
}

// ---------------------------------------------------------------- retract

// Retract marks an alert this server sent as retracted, everywhere.
func (a *App) Retract(ctx context.Context, act Actor, alertID string) (string, error) {
	alertID = strings.ToLower(strings.TrimSpace(alertID))
	if !fed.CanonicalUUID(alertID) {
		return "", userErr("`%s` is not an alert ID. It is in the footer of the alert card.", truncate(alertID, 40))
	}
	al, err := a.St.GetAlert(ctx, alertID)
	if err != nil {
		return "", err
	}
	if al == nil {
		return "", userErr("No alert with that ID in the last 14 days.")
	}
	if al.OriginGuild != act.GuildID || al.OriginHost != a.Self() {
		return "", userErr("Only the server that sent an alert can retract it.")
	}
	if al.Status == store.AlertHeld {
		return "", userErr("That alert is held and was never sent. The server owner can discard it from the DM.")
	}
	if al.RetractedAt > 0 {
		return "That alert is already retracted.", nil
	}
	r := fed.Retraction{AlertID: al.ID, OriginHost: a.Self(), OriginKey: a.ID.PubB64(), RetractedAt: a.Now().Unix()}
	a.ID.SignRetraction(&r)
	if _, err := a.St.SetRetracted(ctx, al.ID, r.RetractedAt, r.Sig); err != nil {
		return "", err
	}
	a.refreshAllCards(ctx, al.ID)
	n := a.relayRetraction(ctx, &r, "")
	a.Log.Info("alert retracted", "alert", al.ID, "by", act.UserID)
	msg := "Retracted. Every copy on this instance now shows RETRACTED with no Ban or Forward button."
	if n > 0 {
		msg += fmt.Sprintf(" The retraction is on its way to %d other instance(s), which pass it along the same paths the alert took.", n)
	}
	return msg + " Bans already made stay; each server decides whether to unban.", nil
}

func (a *App) relayRetraction(ctx context.Context, r *fed.Retraction, except string) int {
	hosts, err := a.St.RecipientHosts(ctx, r.AlertID)
	if err != nil {
		return 0
	}
	n := 0
	for _, h := range hosts {
		if h == a.Self() || h == except || !a.peerUsable(ctx, h) {
			continue
		}
		if err := a.enqueue(ctx, h, fed.TypeAlertRetract, r); err == nil {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------- history

// History renders /history for a server.
func (a *App) History(ctx context.Context, act Actor, userID string) (discord.Embed, error) {
	userID = strings.TrimSpace(userID)
	if userID != "" && !config.IsSnowflake(userID) {
		return discord.Embed{}, userErr("`%s` is not a Discord user ID.", truncate(userID, 40))
	}
	since := a.Now().Add(-fed.AlertLifetime).Unix()
	items, err := a.St.History(ctx, act.GuildID, a.Self(), userID, since, 25)
	if err != nil {
		return discord.Embed{}, err
	}
	e := discord.Embed{Color: colorInfo, Title: "Alert history, last 14 days"}
	if userID != "" {
		e.Title = fmt.Sprintf("Alerts about %s, last 14 days", userID)
	}
	if len(items) == 0 {
		e.Description = "Nothing in the last 14 days."
		return e, nil
	}
	var b strings.Builder
	for _, it := range items {
		al := it.Alert
		dir := "Received"
		if it.Sent {
			dir = "Sent"
		}
		line := fmt.Sprintf("%s **%s** `%s` %s: %s", ts(al.CreatedAt, "R"), dir, al.TargetID, md(al.TargetName), md(truncate(al.Reason, 80)))
		var tags []string
		if al.Status == store.AlertHeld {
			tags = append(tags, "held")
		}
		if al.RetractedAt > 0 {
			tags = append(tags, "retracted")
		}
		if !it.Sent {
			tags = append(tags, fmt.Sprintf("from @%s in %s", md(al.SenderName), md(al.OriginGuildName)))
			if al.OriginHost != a.Self() {
				tags = append(tags, "via "+al.OriginHost)
			}
		}
		acts, _ := a.St.ActionsFor(ctx, al.ID, act.GuildID)
		for _, x := range acts {
			tags = append(tags, fmt.Sprintf("%s by <@%s>", x.Action, x.ModeratorID))
		}
		if userID != "" && it.Delivery != nil && len(it.Delivery.Path) > 0 {
			hops, _ := a.St.HopsByKey(ctx, al.ID)
			var chain []string
			for _, k := range it.Delivery.Path {
				if h, ok := hops[k]; ok {
					chain = append(chain, fmt.Sprintf("@%s in %s", h.ByName, h.GuildName))
				}
			}
			tags = append(tags, "forward chain: "+strings.Join(chain, " > "))
		}
		if len(tags) > 0 {
			line += " (" + strings.Join(tags, ", ") + ")"
		}
		line += "\n`" + al.ID + "`\n"
		if b.Len()+len(line) > 3900 {
			b.WriteString("...more not shown. Filter with `userid` to narrow it down.")
			break
		}
		b.WriteString(line)
	}
	e.Description = b.String()
	return e, nil
}
