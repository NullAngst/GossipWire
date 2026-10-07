package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/disgoorg/disgo/discord"

	"github.com/NullAngst/GossipWire/internal/fed"
	"github.com/NullAngst/GossipWire/internal/store"
)

// Card colors.
const (
	colorActive    = 0xD83C3E
	colorBanned    = 0x2D7D46
	colorIgnored   = 0xC27C0E
	colorRetracted = 0x747F8D
	colorInfo      = 0x5865F2
)

// noMentions keeps cards from pinging anyone.
func noMentions() *discord.AllowedMentions {
	return &discord.AllowedMentions{Parse: []discord.AllowedMentionType{}, Roles: []discordID{}, Users: []discordID{}}
}

func plain(content string) discord.MessageCreate {
	return discord.MessageCreate{Content: content, AllowedMentions: noMentions()}
}

func ts(unix int64, style string) string { return fmt.Sprintf("<t:%d:%s>", unix, style) }

// mdEscaper backslash-escapes Discord markdown. Server names, nicknames and
// reasons come from other servers and other instances; unescaped, a server
// named "[Discord Trust & Safety](https://...)" would render as a link on
// every card, and "<@&id>" or "<#id>" would render as role or channel tags.
// Plain URLs stay clickable, since they show where they go; masked links do
// not survive. Mentions never ping either way: every message the bot sends
// sets allowed_mentions to nothing.
var mdEscaper = strings.NewReplacer(
	`\`, `\\`, "*", `\*`, "_", `\_`, "~", `\~`, "`", "'", "|", `\|`, ">", `\>`, "<", `\<`,
	"#", `\#`, "-", `\-`, "[", `\[`, "]", `\]`, "(", `\(`, ")", `\)`,
)

// md makes untrusted text display literally.
func md(s string) string { return mdEscaper.Replace(s) }

func unverifiedTag(h store.Hop) string {
	if h.Verified {
		return ""
	}
	return " (not verified)"
}

func boolPtr(b bool) *bool { return &b }

// accountCreated reads the creation time out of a Discord ID.
func accountCreated(id string) time.Time {
	n, err := strconv.ParseUint(id, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.UnixMilli(int64(n>>22) + 1420070400000)
}

// cardState is what one server's copy of an alert should show.
type cardState struct {
	alert    *store.Alert
	delivery *store.Delivery
	chain    []store.Hop // forward chain on this server's path, in order
	actions  []store.Action
	self     string
	now      time.Time
}

func (c *cardState) decision() *store.Action {
	for i := range c.actions {
		if c.actions[i].Action == store.ActBan || c.actions[i].Action == store.ActIgnore {
			return &c.actions[i]
		}
	}
	return nil
}

func (c *cardState) forwarded() *store.Action {
	for i := range c.actions {
		if c.actions[i].Action == store.ActForward {
			return &c.actions[i]
		}
	}
	return nil
}

func (c *cardState) expired() bool { return fed.Expired(c.alert.CreatedAt, c.now) }

// build renders the embed and buttons.
func (c *cardState) build() (discord.Embed, []discord.LayoutComponent) {
	al := c.alert
	e := discord.Embed{Color: colorActive}
	e.Title = truncate(fmt.Sprintf("Alert from @%s in %s", md(al.SenderName), md(al.OriginGuildName)), 256)
	created := time.Unix(al.CreatedAt, 0)
	e.Timestamp = &created

	var lines []string
	if al.RetractedAt > 0 {
		e.Title = truncate("RETRACTED: "+e.Title, 256)
		e.Color = colorRetracted
		lines = append(lines, fmt.Sprintf("**Retracted by the sending server %s.** Bans already made stay; unban if you think it was wrong.", ts(al.RetractedAt, "R")))
	}
	if al.OriginHost != c.self {
		lines = append(lines, "via `"+al.OriginHost+"`")
	}
	if !al.OriginVerified {
		lines = append(lines, "**Origin not verified.** This instance could not check the sending instance's signature, so the header above is only as trustworthy as the instance that passed it on.")
	}
	if n := len(c.chain); n > 0 {
		show := c.chain
		if n > 2 {
			show = c.chain[n-2:]
			lines = append(lines, fmt.Sprintf("and %d more", n-2))
		}
		for _, h := range show {
			via := ""
			if h.Host != c.self {
				via = " via `" + h.Host + "`"
			}
			lines = append(lines, fmt.Sprintf("Forwarded by @%s in %s%s%s", md(h.ByName), md(h.GuildName), via, unverifiedTag(h)))
		}
	}
	e.Description = truncate(strings.Join(lines, "\n"), 4000)

	acct := accountCreated(al.TargetID)
	inServer := "No"
	if c.delivery != nil && c.delivery.InServer {
		inServer = "Yes"
	}
	e.Fields = append(e.Fields,
		discord.EmbedField{Name: "Target", Value: truncate(fmt.Sprintf("%s\n`%s`\n<@%s>", md(al.TargetName), al.TargetID, al.TargetID), 1024), Inline: boolPtr(true)},
		discord.EmbedField{Name: "Account created", Value: fmt.Sprintf("%s\n%s", ts(acct.Unix(), "D"), ts(acct.Unix(), "R")), Inline: boolPtr(true)},
		discord.EmbedField{Name: "In this server", Value: inServer, Inline: boolPtr(true)},
		discord.EmbedField{Name: "Reason", Value: truncate(md(al.Reason), 1024)},
	)
	var ev []string
	if al.EvidenceName != "" {
		if al.EvidencePath != "" {
			ev = append(ev, "Attached: `"+cleanName(al.EvidenceName)+"`")
		} else {
			ev = append(ev, "The sender attached `"+cleanName(al.EvidenceName)+"`, but it did not reach this instance.")
		}
	}
	if al.EvidenceLink != "" {
		ev = append(ev, "Message: "+al.EvidenceLink)
	}
	if len(ev) > 0 {
		e.Fields = append(e.Fields, discord.EmbedField{Name: "Evidence", Value: truncate(strings.Join(ev, "\n"), 1024)})
	}

	var status []string
	if d := c.decision(); d != nil {
		if d.Action == store.ActBan {
			status = append(status, fmt.Sprintf("Banned by <@%s> %s", d.ModeratorID, ts(d.At, "f")))
			if al.RetractedAt == 0 {
				e.Color = colorBanned
			}
		} else {
			status = append(status, fmt.Sprintf("Ignored by <@%s> %s", d.ModeratorID, ts(d.At, "f")))
			if al.RetractedAt == 0 {
				e.Color = colorIgnored
			}
		}
	}
	if f := c.forwarded(); f != nil {
		status = append(status, fmt.Sprintf("Forwarded by <@%s> %s", f.ModeratorID, ts(f.At, "f")))
	}
	if c.expired() && al.RetractedAt == 0 {
		status = append(status, "Expired, ban manually if needed")
		e.Color = colorRetracted
	}
	if len(status) > 0 {
		e.Fields = append(e.Fields, discord.EmbedField{Name: "Status", Value: strings.Join(status, "\n")})
	}
	if al.TargetAvatar != "" {
		e.Thumbnail = &discord.EmbedResource{URL: al.TargetAvatar}
	}
	e.Footer = &discord.EmbedFooter{Text: "Alert ID " + al.ID}

	var buttons []discord.InteractiveComponent
	if al.RetractedAt == 0 && !c.expired() {
		if c.decision() == nil {
			buttons = append(buttons,
				discord.NewDangerButton("Ban", "gw:ban:"+al.ID),
				discord.NewSecondaryButton("Ignore", "gw:ignore:"+al.ID))
		}
		if c.forwarded() == nil {
			buttons = append(buttons, discord.NewPrimaryButton("Forward", "gw:fwd:"+al.ID))
		}
	}
	comps := []discord.LayoutComponent{}
	if len(buttons) > 0 {
		comps = append(comps, discord.NewActionRow(buttons...))
	}
	return e, comps
}

// loadCard gathers everything one server's copy needs.
func (a *App) loadCard(ctx context.Context, alertID, guild string) (*cardState, error) {
	al, err := a.St.GetAlert(ctx, alertID)
	if err != nil || al == nil {
		return nil, err
	}
	d, err := a.St.GetDelivery(ctx, alertID, guild)
	if err != nil {
		return nil, err
	}
	acts, err := a.St.ActionsFor(ctx, alertID, guild)
	if err != nil {
		return nil, err
	}
	cs := &cardState{alert: al, delivery: d, actions: acts, self: a.Self(), now: a.Now()}
	if d != nil && len(d.Path) > 0 {
		hops, err := a.St.HopsByKey(ctx, alertID)
		if err != nil {
			return nil, err
		}
		for _, k := range d.Path {
			if h, ok := hops[k]; ok {
				cs.chain = append(cs.chain, h)
			}
		}
	}
	return cs, nil
}

// refreshCard re-renders one server's copy in place.
func (a *App) refreshCard(ctx context.Context, alertID, guild string) {
	cs, err := a.loadCard(ctx, alertID, guild)
	if err != nil || cs == nil || cs.delivery == nil || cs.delivery.MessageID == "" {
		return
	}
	e, comps := cs.build()
	if err := a.DC.Edit(ctx, cs.delivery.ChannelID, cs.delivery.MessageID, discord.MessageUpdate{Embeds: &[]discord.Embed{e}, Components: &comps}); err != nil {
		a.Log.Warn("could not update alert card", "alert", alertID, "guild", guild, "err", err)
	}
}

// refreshAllCards re-renders every local copy of an alert.
func (a *App) refreshAllCards(ctx context.Context, alertID string) {
	ds, err := a.St.DeliveriesForAlert(ctx, alertID)
	if err != nil {
		return
	}
	for _, d := range ds {
		if d.Status == store.DeliverySent {
			a.refreshCard(ctx, alertID, d.GuildID)
		}
	}
}

// ExpiredCardUpdate turns an old card whose alert was purged into a closed
// one, keeping whatever it already showed.
func ExpiredCardUpdate(old []discord.Embed) discord.MessageUpdate {
	var e discord.Embed
	if len(old) > 0 {
		e = old[0]
	}
	e.Color = colorRetracted
	fields := make([]discord.EmbedField, 0, len(e.Fields)+1)
	for _, f := range e.Fields {
		if f.Name != "Status" {
			fields = append(fields, f)
		}
	}
	e.Fields = append(fields, discord.EmbedField{Name: "Status", Value: "Expired, ban manually if needed"})
	comps := []discord.LayoutComponent{}
	return discord.MessageUpdate{Embeds: &[]discord.Embed{e}, Components: &comps}
}

// evidenceFile opens the stored evidence for upload, or returns nil.
func (a *App) evidenceFile(al *store.Alert) (*discord.File, func()) {
	if al.EvidencePath == "" {
		return nil, func() {}
	}
	f, err := os.Open(al.EvidencePath)
	if err != nil {
		a.Log.Warn("evidence file missing", "alert", al.ID, "err", err)
		return nil, func() {}
	}
	return discord.NewFile(safeFileName(al.EvidenceName), "Evidence for alert "+al.ID, f), func() { f.Close() }
}

// safeFileName keeps upload names to characters Discord and filesystems agree on.
func safeFileName(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	s := strings.TrimLeft(b.String(), ".")
	if s == "" {
		s = "evidence"
	}
	return truncate(s, 80)
}
