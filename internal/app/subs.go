package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/disgoorg/disgo/discord"

	"github.com/NullAngst/GossipWire/internal/config"
	"github.com/NullAngst/GossipWire/internal/fed"
	"github.com/NullAngst/GossipWire/internal/store"
)

// Subscribe asks to receive another server's alerts.
func (a *App) Subscribe(ctx context.Context, act Actor, target string) (string, error) {
	if _, err := a.requireSetup(ctx, act.GuildID); err != nil {
		return "", err
	}
	guild, host, err := fed.ParseAddress(target)
	if err != nil {
		return "", &UserError{Msg: err.Error()}
	}
	if host == "" {
		host = a.Self()
	}
	if guild == act.GuildID && host == a.Self() {
		return "", userErr("That is this server.")
	}
	if host != a.Self() {
		if err := a.checkRemoteHost(ctx, host); err != nil {
			return "", err
		}
	}
	existing, err := a.St.GetSub(ctx, guild, host, act.GuildID, a.Self())
	if err != nil {
		return "", err
	}
	if existing != nil {
		switch existing.Status {
		case store.SubActive:
			return "", userErr("You already follow `%s`.", target)
		case store.SubPending:
			return "", userErr("A request to follow `%s` is already waiting for their mods.", target)
		}
	}
	myName := cleanName(a.guildName(act.GuildID, act.GuildName))
	sub := store.Subscription{PubGuild: guild, PubHost: host, SubGuild: act.GuildID, SubHost: a.Self(), SubName: myName,
		Status: store.SubPending, RequestedBy: act.UserID, CreatedAt: a.Now().Unix()}

	if host == a.Self() {
		pg, err := a.St.GetGuild(ctx, guild)
		if err != nil {
			return "", err
		}
		if pg == nil {
			return "", userErr("Server `%s` is not using GossipWire on this instance, or has not run /setup yet. If it is on another instance, use `%s@<their host>`.", guild, guild)
		}
		if a.St.CountPendingFor(ctx, guild, a.Self()) >= maxPendingRequests {
			return "", userErr("That server has too many unanswered requests. Ask its mods to clear them, then try again.")
		}
		sub.PubName = cleanName(a.guildName(guild, ""))
		id, err := a.St.PutSub(ctx, sub)
		if err != nil {
			return "", err
		}
		sub.ID = id
		if err := a.postRequestCard(ctx, &sub, pg.ChannelID, act.UserName); err != nil {
			a.St.DeleteSub(ctx, id)
			return "", userErr("I could not post your request in that server's alert channel. Their mods need to check my permissions there.")
		}
		return fmt.Sprintf("Request sent to %s. Their mods will see it in their alert channel.", md(sub.PubName)), nil
	}

	if _, err := a.St.PutSub(ctx, sub); err != nil {
		return "", err
	}
	if err := a.enqueue(ctx, host, fed.TypeSubscribeRequest, &fed.SubscribeRequest{
		PublisherGuild: guild, SubscriberGuild: act.GuildID, SubscriberGuildName: myName, ByName: cleanName(act.UserName),
	}); err != nil {
		return "", err
	}
	return fmt.Sprintf("Request sent to `%s` on `%s`. You will get a message here when their mods answer.", guild, host), nil
}

// checkRemoteHost decides whether this instance may talk to host at all.
func (a *App) checkRemoteHost(ctx context.Context, host string) error {
	if a.Cfg.FederationMode == config.ModeOff {
		return userErr("This instance does not federate (FEDERATION_MODE is off), so it can only reach servers that use this same bot. Use the bare server ID.")
	}
	inst, err := a.St.GetInstance(ctx, host)
	if err != nil {
		return err
	}
	if inst == nil {
		if a.Cfg.FederationMode == config.ModeOpen && !fed.IsKeyID(host) {
			// A server mod, not the operator, is naming this host, so it
			// is fetched with the client that refuses internal addresses.
			if _, err := a.addPeer(ctx, host, false); err != nil {
				return err
			}
			return nil
		}
		return userErr("`%s` is not one of this instance's peers. The instance operator has to run `/instance peer add %s` first.", host, host)
	}
	switch inst.Status {
	case store.PeerBlocked:
		return userErr("`%s` is blocked on this instance.", host)
	case store.PeerKeyChanged:
		return userErr("`%s` changed its key and is blocked until the instance operator checks it in /instance peers.", host)
	}
	if fed.IsKeyID(host) && !a.Cfg.Reachable() {
		return userErr("`%s` has no public address and neither does this instance, so they cannot reach each other. One side needs INSTANCE_URL set (setup C).", host)
	}
	return nil
}

// peerUsable reports whether messages to host may be queued.
func (a *App) peerUsable(ctx context.Context, host string) bool {
	if a.Cfg.FederationMode == config.ModeOff {
		return false
	}
	inst, err := a.St.GetInstance(ctx, host)
	return err == nil && inst != nil && inst.Status == store.PeerAllowed
}

func (a *App) postRequestCard(ctx context.Context, sub *store.Subscription, channelID, byName string) error {
	who := sub.SubName
	addr := a.addr(sub.SubGuild, sub.SubHost)
	e := discord.Embed{
		Title: "Subscription request",
		Color: colorInfo,
		Description: fmt.Sprintf("**%s** (`%s`) wants to receive this server's alerts. Requested by @%s.\n\n"+
			"**Approve** sends them your alerts. Their alerts do not come to you.\n"+
			"**Approve and follow back** also asks to receive theirs; they still get to approve that.\n"+
			"**Deny** sends nothing.", md(who), addr, md(cleanName(byName))),
	}
	if sub.SubHost != a.Self() {
		e.Description += fmt.Sprintf("\n\nThis server is on another instance, `%s`. Approving means trusting its operator too.", sub.SubHost)
	}
	id := fmt.Sprint(sub.ID)
	msgID, err := a.DC.Send(ctx, channelID, discord.MessageCreate{
		Embeds: []discord.Embed{e},
		Components: []discord.LayoutComponent{discord.NewActionRow(
			discord.NewSuccessButton("Approve", "gw:sa:"+id),
			discord.NewPrimaryButton("Approve and follow back", "gw:sf:"+id),
			discord.NewSecondaryButton("Deny", "gw:sd:"+id),
		)},
		AllowedMentions: noMentions(),
	})
	if err != nil {
		return err
	}
	sub.CardChannel = channelID
	sub.CardMessage = msgID
	return a.St.UpdateSub(ctx, *sub)
}

func (a *App) closeRequestCard(ctx context.Context, sub *store.Subscription, line string) {
	if sub.CardMessage == "" {
		return
	}
	e := discord.Embed{Title: "Subscription request", Color: colorRetracted,
		Description: fmt.Sprintf("**%s** (`%s`) asked to receive this server's alerts.\n%s", md(sub.SubName), a.addr(sub.SubGuild, sub.SubHost), line)}
	comps := []discord.LayoutComponent{}
	a.DC.Edit(ctx, sub.CardChannel, sub.CardMessage, discord.MessageUpdate{Embeds: &[]discord.Embed{e}, Components: &comps})
}

// maxPendingRequests caps unanswered subscribe requests per server, so
// nobody can bury a mod channel in request cards.
const maxPendingRequests = 10

// Subscription decisions.
const (
	DecideApprove = "approve"
	DecideFollow  = "follow"
	DecideDeny    = "deny"
)

// DecideSubscription handles the buttons on a request card. The caller has
// already checked the presser is a mod in this server.
func (a *App) DecideSubscription(ctx context.Context, subID int64, decision string, act Actor) (string, error) {
	sub, err := a.St.GetSubByID(ctx, subID)
	if err != nil {
		return "", err
	}
	if sub == nil {
		return "", userErr("This request no longer exists. The other server may have cancelled it.")
	}
	if sub.PubGuild != act.GuildID || sub.PubHost != a.Self() {
		return "", userErr("This request is for another server.")
	}
	if sub.Status != store.SubPending {
		return "", userErr("This request was already answered.")
	}
	pubName := cleanName(a.guildName(act.GuildID, act.GuildName))
	if decision == DecideDeny {
		if err := a.St.DeleteSub(ctx, sub.ID); err != nil {
			return "", err
		}
		a.closeRequestCard(ctx, sub, fmt.Sprintf("Denied by <@%s>.", act.UserID))
		if sub.SubHost == a.Self() {
			a.notifyGuild(ctx, sub.SubGuild, fmt.Sprintf("%s denied your request to receive their alerts.", md(pubName)))
		} else {
			a.enqueue(ctx, sub.SubHost, fed.TypeSubscribeDeny, &fed.SubscribeReply{PublisherGuild: sub.PubGuild, PublisherGuildName: pubName,
				SubscriberGuild: sub.SubGuild, ByName: cleanName(act.UserName)})
		}
		return "", nil
	}

	sub.Status = store.SubActive
	sub.ApprovedBy = act.UserID
	sub.ApprovedAt = a.Now().Unix()
	sub.PubName = pubName
	if err := a.St.UpdateSub(ctx, *sub); err != nil {
		return "", err
	}
	line := fmt.Sprintf("Approved by <@%s>. They now receive this server's alerts.", act.UserID)
	if sub.SubHost == a.Self() {
		a.notifyGuild(ctx, sub.SubGuild, fmt.Sprintf("%s approved your request. Their alerts will arrive here.", md(pubName)))
	} else {
		a.enqueue(ctx, sub.SubHost, fed.TypeSubscribeAccept, &fed.SubscribeReply{PublisherGuild: sub.PubGuild, PublisherGuildName: pubName,
			SubscriberGuild: sub.SubGuild, ByName: cleanName(act.UserName)})
	}
	reply := ""
	if decision == DecideFollow {
		target := a.addr(sub.SubGuild, sub.SubHost)
		msg, err := a.Subscribe(ctx, act, target)
		if err != nil {
			reply = "Approved. The follow-back request did not go out: " + Friendly(err)
			line += " Follow back failed."
		} else {
			reply = "Approved. " + msg
			line += " A follow-back request went to them."
		}
	}
	a.closeRequestCard(ctx, sub, line)
	return reply, nil
}

func (a *App) notifyGuild(ctx context.Context, guild, text string) {
	g, err := a.St.GetGuild(ctx, guild)
	if err != nil || g == nil {
		return
	}
	if _, err := a.DC.Send(ctx, g.ChannelID, plain(text)); err != nil {
		a.Log.Debug("could not post notice", "guild", guild, "err", err)
	}
}

// Unsubscribe stops receiving a server's alerts. Immediate.
func (a *App) Unsubscribe(ctx context.Context, act Actor, target string) (string, error) {
	guild, host, err := fed.ParseAddress(target)
	if err != nil {
		return "", &UserError{Msg: err.Error()}
	}
	if host == "" {
		host = a.Self()
	}
	sub, err := a.St.GetSub(ctx, guild, host, act.GuildID, a.Self())
	if err != nil {
		return "", err
	}
	if sub == nil {
		return "", userErr("This server does not follow `%s`.", target)
	}
	if err := a.St.DeleteSub(ctx, sub.ID); err != nil {
		return "", err
	}
	if host != a.Self() {
		a.enqueue(ctx, host, fed.TypeUnsubscribe, &fed.Unsubscribe{PublisherGuild: guild, SubscriberGuild: act.GuildID, Side: "subscriber"})
	} else if sub.Status == store.SubPending {
		a.closeRequestCard(ctx, sub, "Cancelled by the requesting server.")
	}
	return fmt.Sprintf("Done. Alerts from `%s` will no longer come here.", target), nil
}

// RemoveSubscriber stops sending to a server that follows this one. Immediate.
func (a *App) RemoveSubscriber(ctx context.Context, act Actor, target string) (string, error) {
	guild, host, err := fed.ParseAddress(target)
	if err != nil {
		return "", &UserError{Msg: err.Error()}
	}
	if host == "" {
		host = a.Self()
	}
	sub, err := a.St.GetSub(ctx, act.GuildID, a.Self(), guild, host)
	if err != nil {
		return "", err
	}
	if sub == nil {
		return "", userErr("`%s` does not follow this server.", target)
	}
	if err := a.St.DeleteSub(ctx, sub.ID); err != nil {
		return "", err
	}
	pubName := cleanName(a.guildName(act.GuildID, act.GuildName))
	if host != a.Self() {
		a.enqueue(ctx, host, fed.TypeUnsubscribe, &fed.Unsubscribe{PublisherGuild: act.GuildID, SubscriberGuild: guild, Side: "publisher"})
	} else if sub.Status == store.SubActive {
		a.notifyGuild(ctx, guild, fmt.Sprintf("%s stopped sending its alerts to this server.", md(pubName)))
	}
	if sub.Status == store.SubPending {
		a.closeRequestCard(ctx, sub, "Removed.")
	}
	return fmt.Sprintf("Done. `%s` no longer receives this server's alerts.", target), nil
}

// ListSubscriptions renders /subscriptions.
func (a *App) ListSubscriptions(ctx context.Context, act Actor) (discord.Embed, error) {
	subs, err := a.St.SubsForGuild(ctx, act.GuildID, a.Self())
	if err != nil {
		return discord.Embed{}, err
	}
	var following, followers, outPending, inPending []string
	for _, s := range subs {
		if s.SubGuild == act.GuildID && s.SubHost == a.Self() {
			line := fmt.Sprintf("%s `%s`", md(orID(s.PubName, s.PubGuild)), a.addr(s.PubGuild, s.PubHost))
			if s.Status == store.SubPending {
				outPending = append(outPending, line)
			} else if s.Status == store.SubActive {
				following = append(following, line)
			}
		} else {
			line := fmt.Sprintf("%s `%s`", md(orID(s.SubName, s.SubGuild)), a.addr(s.SubGuild, s.SubHost))
			if s.Status == store.SubPending {
				inPending = append(inPending, line)
			} else if s.Status == store.SubActive {
				followers = append(followers, line)
			}
		}
	}
	list := func(xs []string) string {
		if len(xs) == 0 {
			return "none"
		}
		return truncate(strings.Join(xs, "\n"), 1024)
	}
	return discord.Embed{
		Title:       "Subscriptions",
		Color:       colorInfo,
		Description: fmt.Sprintf("This server's address: `%s`", a.ServerAddress(act.GuildID)),
		Fields: []discord.EmbedField{
			{Name: "You follow (their alerts come here)", Value: list(following)},
			{Name: "Follow you (your alerts go there)", Value: list(followers)},
			{Name: "Waiting on them", Value: list(outPending)},
			{Name: "Waiting on you", Value: list(inPending)},
		},
	}, nil
}

func orID(name, id string) string {
	if name == "" {
		return id
	}
	return name
}
