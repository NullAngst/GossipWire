package app

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/disgoorg/disgo/discord"

	"github.com/NullAngst/GossipWire/internal/config"
	"github.com/NullAngst/GossipWire/internal/fed"
	"github.com/NullAngst/GossipWire/internal/store"
)

// IsOwner reports whether a Discord user may run /instance.
func (a *App) IsOwner(userID string) bool { return userID == a.Cfg.OwnerID }

// Status renders /instance status, in the same A/B/C words as the README.
func (a *App) Status(ctx context.Context) discord.Embed {
	l := a.St.GetLimits(ctx)
	insts, _ := a.St.ListInstances(ctx)
	backlog, _ := a.St.OutboxBacklog(ctx)
	counts := map[string]int{}
	for _, i := range insts {
		if i.Host != a.Self() {
			counts[i.Status]++
		}
	}
	addr := "none (other instances cannot reach this one directly)"
	if a.Cfg.Reachable() {
		addr = a.Cfg.InstanceURL
	}
	mode := map[string]string{
		config.ModeOff:       "off: only servers that use this bot",
		config.ModeAllowlist: "allowlist: also instances you added with /instance peer add",
		config.ModeOpen:      "open: any GossipWire instance may ask to connect",
	}[a.Cfg.FederationMode]
	queued := 0
	for _, n := range backlog {
		queued += n
	}
	fields := []discord.EmbedField{
		{Name: "Setup", Value: a.Cfg.SetupDescription()},
		{Name: "FEDERATION_MODE", Value: mode},
		{Name: "Public address", Value: addr},
	}
	if a.Cfg.FederationMode != config.ModeOff {
		fields = append(fields,
			discord.EmbedField{Name: "This instance's name", Value: fmt.Sprintf("`%s`\nOther operators add you with `/instance peer add %s`.", a.Self(), a.Self())},
			discord.EmbedField{Name: "Key fingerprint", Value: "`" + fed.Fingerprint(a.ID.Pub) + "`"},
			discord.EmbedField{Name: "Peers", Value: fmt.Sprintf("%d allowed, %d blocked, %d waiting on a key decision. %d message(s) queued.",
				counts[store.PeerAllowed], counts[store.PeerBlocked], counts[store.PeerKeyChanged], queued)},
		)
	}
	fields = append(fields,
		discord.EmbedField{Name: "Limits", Value: fmt.Sprintf("%d alerts per hour, %d per day, per server. Evidence up to %d MB.", l.PerHour, l.PerDay, l.EvidenceMB)},
		discord.EmbedField{Name: "Servers", Value: fmt.Sprintf("In %d server(s). %d have run /setup.", a.DC.GuildCount(), a.St.CountGuilds(ctx))},
		discord.EmbedField{Name: "Install link", Value: a.InstallLink()},
	)
	return discord.Embed{Title: "GossipWire instance status", Color: colorInfo, Fields: fields}
}

// AddPeer allows another instance on the operator's say-so. A hostname must
// answer at its address so its key can be pinned now; a key- address pins
// itself on first contact.
func (a *App) AddPeer(ctx context.Context, raw string) (string, error) {
	return a.addPeer(ctx, raw, true)
}

// addPeer is AddPeer. operator is false when a server mod's /subscribe adds
// a peer in open mode; then internal addresses are refused.
func (a *App) addPeer(ctx context.Context, raw string, operator bool) (string, error) {
	if a.Cfg.FederationMode == config.ModeOff {
		return "", userErr("FEDERATION_MODE is off, so this instance does not talk to other instances. Change it to allowlist in the compose file first.")
	}
	host, err := fed.NormalizeHost(raw)
	if err != nil {
		return "", &UserError{Msg: err.Error()}
	}
	if host == a.Self() {
		return "", userErr("That is this instance.")
	}
	now := a.Now().Unix()
	cur, err := a.St.GetInstance(ctx, host)
	if err != nil {
		return "", err
	}
	if fed.IsKeyID(host) {
		if !a.Cfg.Reachable() {
			return "", userErr("`%s` has no public address and neither does this instance, so they cannot reach each other. One side needs INSTANCE_URL set (setup C).", host)
		}
		row := store.Instance{Host: host, Reach: store.ReachOutbound, Status: store.PeerAllowed, FirstSeen: now}
		if cur != nil {
			row.PublicKey, row.PollCursor, row.FirstSeen, row.LastSeen = cur.PublicKey, cur.PollCursor, cur.FirstSeen, cur.LastSeen
			if cur.Status == store.PeerKeyChanged {
				return "", userErr("`%s` is waiting on a key decision. Use /instance peers.", host)
			}
		}
		if err := a.St.UpsertInstance(ctx, row); err != nil {
			return "", err
		}
		return fmt.Sprintf("Added `%s`. It has no public address, so it polls this instance for its messages. Its key is checked against its name on first contact.", host), nil
	}
	fetch := a.Fed.FetchWellKnown
	if !operator {
		fetch = a.Fed.FetchWellKnownUntrusted
	}
	_, pub, err := fetch(ctx, host)
	if err != nil {
		return "", userErr("Could not add `%s`: %v", host, err)
	}
	fp := fed.Fingerprint(pub)
	row := store.Instance{Host: host, PublicKey: fed.EncodeKey(pub), Reach: store.ReachReachable, Status: store.PeerAllowed, FirstSeen: now, LastSeen: now}
	if cur != nil {
		if !operator && cur.Status != store.PeerAllowed {
			return "", userErr("`%s` is blocked on this instance.", host)
		}
		row.FirstSeen, row.PollCursor = cur.FirstSeen, cur.PollCursor
		if cur.PublicKey != "" && cur.PublicKey != row.PublicKey {
			return "", userErr("`%s` now publishes a different key than the one pinned here. Check with its operator, then decide in /instance peers.", host)
		}
	}
	if err := a.St.UpsertInstance(ctx, row); err != nil {
		return "", err
	}
	a.Log.Info("peer added", "host", host, "fingerprint", fp, "by_operator", operator)
	msg := fmt.Sprintf("Added `%s`. Key fingerprint `%s`; confirm it with that operator out of band if you can.", host, fp)
	if !a.Cfg.Reachable() {
		msg += " This instance has no public address, so it will push to that peer and check its outbox every 60 seconds."
	}
	if a.Cfg.FederationMode == config.ModeAllowlist {
		msg += fmt.Sprintf(" Their operator also has to add yours: `/instance peer add %s`.", a.Self())
	}
	return msg, nil
}

// RemovePeer cuts a peer off immediately. The row stays as blocked so an
// open-mode instance does not let it straight back in.
func (a *App) RemovePeer(ctx context.Context, raw string) (string, error) {
	host, err := fed.NormalizeHost(raw)
	if err != nil {
		return "", &UserError{Msg: err.Error()}
	}
	cur, err := a.St.GetInstance(ctx, host)
	if err != nil {
		return "", err
	}
	if cur == nil || host == a.Self() {
		return "", userErr("`%s` is not a peer of this instance.", host)
	}
	cur.Status = store.PeerBlocked
	if err := a.St.UpsertInstance(ctx, *cur); err != nil {
		return "", err
	}
	n, _ := a.St.DeleteSubsWithHost(ctx, host)
	a.St.DropOutboxFor(ctx, host)
	a.Log.Info("peer removed", "host", host, "subscriptions_dropped", n)
	return fmt.Sprintf("Removed `%s`. It is blocked, %d subscription(s) with its servers were dropped, and nothing more goes to or comes from it. Run peer add to allow it again.", host, n), nil
}

// Peers renders /instance peers, with key decision buttons where needed.
func (a *App) Peers(ctx context.Context) (discord.Embed, []discord.LayoutComponent) {
	insts, _ := a.St.ListInstances(ctx)
	backlog, _ := a.St.OutboxBacklog(ctx)
	e := discord.Embed{Title: "Peer instances", Color: colorInfo}
	var lines []string
	var rows []discord.LayoutComponent
	sort.Slice(insts, func(i, j int) bool { return insts[i].Host < insts[j].Host })
	for _, i := range insts {
		if i.Host == a.Self() {
			continue
		}
		fp := "not seen yet"
		if k, err := fed.DecodeKey(i.PublicKey); err == nil {
			fp = fed.Fingerprint(k)
		}
		seen := "never"
		if i.LastSeen > 0 {
			seen = ts(i.LastSeen, "R")
		}
		line := fmt.Sprintf("**%s**: %s, %s, last heard %s, %d queued\nkey `%s`", i.Host, i.Status, i.Reach, seen, backlog[i.Host], fp)
		if i.Status == store.PeerKeyChanged {
			nfp := "unknown"
			if k, err := fed.DecodeKey(i.PendingKey); err == nil {
				nfp = fed.Fingerprint(k)
			}
			line += fmt.Sprintf("\nnew key `%s`", nfp)
			if len(rows) < 5 && len(i.Host) < 80 {
				rows = append(rows, discord.NewActionRow(
					discord.NewSuccessButton("Approve key: "+truncate(i.Host, 40), "gw:keyok:"+i.Host),
					discord.NewDangerButton("Keep blocked", "gw:keyno:"+i.Host),
				))
			}
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		e.Description = "No peers. Add one with `/instance peer add <host>`."
	} else {
		e.Description = truncate(strings.Join(lines, "\n\n"), 4000)
	}
	return e, rows
}

// DecideKey approves a peer's new key or keeps it blocked.
func (a *App) DecideKey(ctx context.Context, host string, approve bool) (string, error) {
	cur, err := a.St.GetInstance(ctx, host)
	if err != nil {
		return "", err
	}
	if cur == nil || cur.Status != store.PeerKeyChanged {
		return "That peer is not waiting on a key decision anymore.", nil
	}
	if approve {
		if cur.PendingKey == "" {
			return "", userErr("No new key is recorded for `%s`.", host)
		}
		cur.PublicKey = cur.PendingKey
		cur.PendingKey = ""
		cur.Status = store.PeerAllowed
		a.Log.Info("peer key approved", "host", host)
	} else {
		cur.Status = store.PeerBlocked
		a.Log.Info("peer kept blocked after key change", "host", host)
	}
	if err := a.St.UpsertInstance(ctx, *cur); err != nil {
		return "", err
	}
	if approve {
		return fmt.Sprintf("Approved the new key for `%s`. It is allowed again.", host), nil
	}
	return fmt.Sprintf("`%s` stays blocked.", host), nil
}

// SetLimits shows or changes the limits. nil means leave as is.
func (a *App) SetLimits(ctx context.Context, perHour, perDay, evidenceMB *int) (string, error) {
	l := a.St.GetLimits(ctx)
	changed := false
	if perHour != nil {
		if *perHour < 1 || *perHour > 100 {
			return "", userErr("per_hour must be between 1 and 100.")
		}
		l.PerHour, changed = *perHour, true
	}
	if perDay != nil {
		if *perDay < 1 || *perDay > 1000 {
			return "", userErr("per_day must be between 1 and 1000.")
		}
		l.PerDay, changed = *perDay, true
	}
	if evidenceMB != nil {
		if *evidenceMB < 1 || *evidenceMB > 25 {
			return "", userErr("evidence_mb must be between 1 and 25.")
		}
		l.EvidenceMB, changed = *evidenceMB, true
	}
	if l.PerDay < l.PerHour {
		return "", userErr("per_day (%d) cannot be lower than per_hour (%d).", l.PerDay, l.PerHour)
	}
	if changed {
		if err := a.St.SetLimits(ctx, l); err != nil {
			return "", err
		}
		a.Log.Info("limits changed", "per_hour", l.PerHour, "per_day", l.PerDay, "evidence_mb", l.EvidenceMB)
	}
	prefix := "Current limits"
	if changed {
		prefix = "Saved. Limits are now"
	}
	return fmt.Sprintf("%s: %d alerts per hour and %d per day for each server (forwards included), evidence up to %d MB.", prefix, l.PerHour, l.PerDay, l.EvidenceMB), nil
}

// StartupSummary is the line logged on a clean start.
func (a *App) StartupSummary() string {
	return fmt.Sprintf("GossipWire is running. %s. Owner %s. In %d server(s). Install link: %s",
		a.Cfg.SetupDescription(), a.Cfg.OwnerID, a.DC.GuildCount(), a.InstallLink())
}
