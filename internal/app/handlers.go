package app

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/omit"
)

// modPerms is what every state-changing action needs, in the server it
// happens in.
const modPerms = discord.PermissionManageGuild | discord.PermissionBanMembers

func intPtr(n int) *int { return &n }

// Commands are registered globally on startup.
func Commands() []discord.ApplicationCommandCreate {
	guildOnly := []discord.InteractionContextType{discord.InteractionContextTypeGuild}
	installs := []discord.ApplicationIntegrationType{discord.ApplicationIntegrationTypeGuildInstall}
	mod := omit.NewPtr(modPerms)
	server := discord.ApplicationCommandOptionString{Name: "server", Description: "Server ID, or id@host for a server on another instance", Required: true, MaxLength: intPtr(300)}
	cmd := func(name, desc string, opts ...discord.ApplicationCommandOption) discord.SlashCommandCreate {
		return discord.SlashCommandCreate{Name: name, Description: desc, Options: opts, DefaultMemberPermissions: mod, Contexts: guildOnly, IntegrationTypes: installs}
	}
	inst := cmd("instance", "Instance settings. Only the instance owner can use these.",
		discord.ApplicationCommandOptionSubCommand{Name: "status", Description: "Show which setup this instance runs as, peers, limits and the install link"},
		discord.ApplicationCommandOptionSubCommand{Name: "peers", Description: "List peer instances and their keys"},
		discord.ApplicationCommandOptionSubCommand{Name: "limits", Description: "Show or change rate limits and the evidence size cap", Options: []discord.ApplicationCommandOption{
			discord.ApplicationCommandOptionInt{Name: "per_hour", Description: "Alerts per hour, per server", MinValue: intPtr(1), MaxValue: intPtr(100)},
			discord.ApplicationCommandOptionInt{Name: "per_day", Description: "Alerts per day, per server", MinValue: intPtr(1), MaxValue: intPtr(1000)},
			discord.ApplicationCommandOptionInt{Name: "evidence_mb", Description: "Largest evidence file, in MB", MinValue: intPtr(1), MaxValue: intPtr(25)},
		}},
		discord.ApplicationCommandOptionSubCommandGroup{Name: "peer", Description: "Add or remove a peer instance", Options: []discord.ApplicationCommandOptionSubCommand{
			{Name: "add", Description: "Allow another GossipWire instance", Options: []discord.ApplicationCommandOption{
				discord.ApplicationCommandOptionString{Name: "host", Description: "Like alerts.example.org, or a key- address", Required: true, MaxLength: intPtr(253)}}},
			{Name: "remove", Description: "Cut a peer off immediately", Options: []discord.ApplicationCommandOption{
				discord.ApplicationCommandOptionString{Name: "host", Description: "The peer's address", Required: true, MaxLength: intPtr(253)}}},
		}},
	)
	// /instance is checked against OWNER_ID on every call. Manage Server
	// only keeps it out of regular members' command list.
	inst.DefaultMemberPermissions = omit.NewPtr(discord.PermissionManageGuild)

	return []discord.ApplicationCommandCreate{
		cmd("setup", "Pick the channel alerts go out from and come in to",
			discord.ApplicationCommandOptionChannel{Name: "channel", Description: "Your mod alert channel", Required: true,
				ChannelTypes: []discord.ChannelType{discord.ChannelTypeGuildText}}),
		cmd("pause", "Stop incoming alerts without touching subscriptions"),
		cmd("resume", "Start incoming alerts again"),
		cmd("alert", "Send a bad-actor alert to every server that follows yours",
			discord.ApplicationCommandOptionString{Name: "userid", Description: "The user's Discord ID (a long number)", Required: true, MinLength: intPtr(17), MaxLength: intPtr(20)},
			discord.ApplicationCommandOptionString{Name: "reason", Description: "What they did", Required: true, MaxLength: intPtr(MaxReason)},
			discord.ApplicationCommandOptionAttachment{Name: "evidence", Description: "A screenshot or file"},
			discord.ApplicationCommandOptionString{Name: "link", Description: "Link to the offending message", MaxLength: intPtr(200)},
		),
		cmd("retract", "Retract an alert this server sent, on every copy",
			discord.ApplicationCommandOptionString{Name: "alert_id", Description: "From the footer of the alert card", Required: true, MaxLength: intPtr(36)}),
		cmd("subscribe", "Ask to receive another server's alerts", server),
		cmd("unsubscribe", "Stop receiving a server's alerts", server),
		cmd("subscribers", "Manage servers that follow yours",
			discord.ApplicationCommandOptionSubCommand{Name: "remove", Description: "Stop sending your alerts to a server", Options: []discord.ApplicationCommandOption{server}}),
		cmd("subscriptions", "List who you follow, who follows you, and pending requests"),
		cmd("history", "Alerts sent and received in the last 14 days",
			discord.ApplicationCommandOptionString{Name: "userid", Description: "Only alerts about this user ID", MaxLength: intPtr(20)}),
		inst,
	}
}

// Handlers routes Discord interactions into the App.
type Handlers struct {
	A *App
}

func isMod(p discord.Permissions) bool {
	return p.Has(discord.PermissionAdministrator) || p.Has(modPerms)
}

func actorFromCommand(e *events.ApplicationCommandInteractionCreate) Actor {
	act := Actor{UserID: e.User().ID.String(), UserName: e.User().Username}
	if g := e.GuildID(); g != nil {
		act.GuildID = g.String()
	}
	if g, ok := e.Guild(); ok {
		act.GuildName = g.Name
	}
	return act
}

func actorFromComponent(e *events.ComponentInteractionCreate) Actor {
	act := Actor{UserID: e.User().ID.String(), UserName: e.User().Username}
	if g := e.GuildID(); g != nil {
		act.GuildID = g.String()
	}
	if g, ok := e.Guild(); ok {
		act.GuildName = g.Name
	}
	return act
}

func ephemeral(content string) discord.MessageCreate {
	return discord.MessageCreate{Content: content, Flags: discord.MessageFlagEphemeral, AllowedMentions: noMentions()}
}

func strPtr(s string) *string { return &s }

// OnCommand handles slash commands. Every reply is deferred first so slow
// work (fan-out, REST lookups) never trips Discord's 3 second window.
func (h *Handlers) OnCommand(e *events.ApplicationCommandInteractionCreate) {
	data := e.SlashCommandInteractionData()
	act := actorFromCommand(e)
	name := data.CommandName()

	if act.GuildID == "" {
		e.CreateMessage(ephemeral("Use GossipWire commands inside a server."))
		return
	}
	// The bot-side check. Discord's default_member_permissions can be
	// overridden by a server owner, so this is the one that counts.
	if name != "instance" {
		if m := e.Member(); m == nil || !isMod(m.Permissions) {
			e.CreateMessage(ephemeral("You need both Manage Server and Ban Members in this server to use GossipWire."))
			return
		}
	} else if !h.A.IsOwner(act.UserID) {
		e.CreateMessage(ephemeral("Only this instance's owner (the OWNER_ID in its compose file) can run /instance."))
		return
	}
	if err := e.DeferCreateMessage(true); err != nil {
		h.A.Log.Warn("could not acknowledge a command", "command", name, "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	reply := func(content string, embeds []discord.Embed, comps []discord.LayoutComponent) {
		u := discord.MessageUpdate{Content: strPtr(content), AllowedMentions: noMentions()}
		if embeds != nil {
			u.Embeds = &embeds
		}
		if comps != nil {
			u.Components = &comps
		}
		if _, err := e.Client().Rest.UpdateInteractionResponse(e.ApplicationID(), e.Token(), u, rest.WithCtx(ctx)); err != nil {
			h.A.Log.Warn("could not send a reply", "command", name, "err", err)
		}
	}
	text := func(msg string, err error) {
		if err != nil {
			h.logErr(name, err)
			reply(Friendly(err), nil, nil)
			return
		}
		reply(msg, nil, nil)
	}

	switch name {
	case "setup":
		ch, ok := data.OptChannel("channel")
		if !ok {
			text("", userErr("Pick a text channel."))
			return
		}
		text(h.A.Setup(ctx, act, ch.ID.String()))
	case "pause":
		text(h.A.SetPaused(ctx, act, true))
	case "resume":
		text(h.A.SetPaused(ctx, act, false))
	case "alert":
		var att *AttachmentIn
		if at, ok := data.OptAttachment("evidence"); ok {
			ct := ""
			if at.ContentType != nil {
				ct = *at.ContentType
			}
			att = &AttachmentIn{Name: at.Filename, ContentType: ct, Size: at.Size, URL: at.URL}
		}
		link, _ := data.OptString("link")
		d, err := h.A.PrepareAlert(ctx, act, e.Channel().ID().String(), data.String("userid"), data.String("reason"), link, att)
		if err != nil {
			text("", err)
			return
		}
		emb, comps := d.Card()
		reply("", []discord.Embed{emb}, comps)
	case "retract":
		text(h.A.Retract(ctx, act, data.String("alert_id")))
	case "subscribe":
		text(h.A.Subscribe(ctx, act, data.String("server")))
	case "unsubscribe":
		text(h.A.Unsubscribe(ctx, act, data.String("server")))
	case "subscribers":
		text(h.A.RemoveSubscriber(ctx, act, data.String("server")))
	case "subscriptions":
		emb, err := h.A.ListSubscriptions(ctx, act)
		if err != nil {
			text("", err)
			return
		}
		reply("", []discord.Embed{emb}, nil)
	case "history":
		uid, _ := data.OptString("userid")
		emb, err := h.A.History(ctx, act, uid)
		if err != nil {
			text("", err)
			return
		}
		reply("", []discord.Embed{emb}, nil)
	case "instance":
		h.instance(ctx, data, reply, text)
	default:
		text("", userErr("Unknown command."))
	}
}

func (h *Handlers) instance(ctx context.Context, data discord.SlashCommandInteractionData,
	reply func(string, []discord.Embed, []discord.LayoutComponent), text func(string, error)) {
	group := ""
	if data.SubCommandGroupName != nil {
		group = *data.SubCommandGroupName
	}
	sub := ""
	if data.SubCommandName != nil {
		sub = *data.SubCommandName
	}
	switch {
	case group == "peer" && sub == "add":
		text(h.A.AddPeer(ctx, data.String("host")))
	case group == "peer" && sub == "remove":
		text(h.A.RemovePeer(ctx, data.String("host")))
	case sub == "status":
		reply("", []discord.Embed{h.A.Status(ctx)}, nil)
	case sub == "peers":
		emb, comps := h.A.Peers(ctx)
		reply("", []discord.Embed{emb}, comps)
	case sub == "limits":
		var ph, pd, ev *int
		if v, ok := data.OptInt("per_hour"); ok {
			ph = &v
		}
		if v, ok := data.OptInt("per_day"); ok {
			pd = &v
		}
		if v, ok := data.OptInt("evidence_mb"); ok {
			ev = &v
		}
		text(h.A.SetLimits(ctx, ph, pd, ev))
	default:
		text("", userErr("Unknown /instance command."))
	}
}

func (h *Handlers) logErr(where string, err error) {
	var ue *UserError
	if !errors.As(err, &ue) {
		h.A.Log.Error("command failed", "where", where, "err", err)
	}
}

// OnComponent handles every button. custom_ids are gw:<action>:<id>; the
// handler looks everything else up in the database, so buttons keep working
// after a restart.
func (h *Handlers) OnComponent(e *events.ComponentInteractionCreate) {
	parts := strings.SplitN(e.Data.CustomID(), ":", 3)
	if len(parts) != 3 || parts[0] != "gw" {
		return
	}
	action, id := parts[1], parts[2]
	act := actorFromComponent(e)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	followup := func(msg string) {
		if msg == "" {
			return
		}
		if _, err := e.Client().Rest.CreateFollowupMessage(e.ApplicationID(), e.Token(), ephemeral(msg), rest.WithCtx(ctx)); err != nil {
			h.A.Log.Debug("followup failed", "err", err)
		}
	}
	editOriginal := func(content string, embeds *[]discord.Embed) {
		comps := []discord.LayoutComponent{}
		u := discord.MessageUpdate{Content: strPtr(content), Components: &comps, Embeds: embeds, AllowedMentions: noMentions()}
		if _, err := e.Client().Rest.UpdateInteractionResponse(e.ApplicationID(), e.Token(), u, rest.WithCtx(ctx)); err != nil {
			h.A.Log.Debug("could not edit the original message", "err", err)
		}
	}

	switch action {
	case "release", "discard":
		// From a DM to the server owner; ReleaseHeld checks ownership.
		if err := e.DeferUpdateMessage(); err != nil {
			return
		}
		msg, err := h.A.ReleaseHeld(ctx, id, act.UserID, action == "release")
		if err != nil {
			h.logErr("release", err)
			followup(Friendly(err))
			return
		}
		editOriginal(msg, &[]discord.Embed{})
		return
	case "keyok", "keyno":
		if !h.A.IsOwner(act.UserID) {
			e.CreateMessage(ephemeral("Only this instance's owner can decide on peer keys."))
			return
		}
		if err := e.DeferUpdateMessage(); err != nil {
			return
		}
		msg, err := h.A.DecideKey(ctx, id, action == "keyok")
		if err != nil {
			followup(Friendly(err))
			return
		}
		emb, comps := h.A.Peers(ctx)
		u := discord.MessageUpdate{Content: strPtr(msg), Embeds: &[]discord.Embed{emb}, Components: &comps, AllowedMentions: noMentions()}
		e.Client().Rest.UpdateInteractionResponse(e.ApplicationID(), e.Token(), u, rest.WithCtx(ctx))
		return
	}

	if act.GuildID == "" {
		e.CreateMessage(ephemeral("This button only works inside a server."))
		return
	}
	if m := e.Member(); m == nil || !isMod(m.Permissions) {
		e.CreateMessage(ephemeral("You need both Manage Server and Ban Members in this server to do that."))
		return
	}
	if err := e.DeferUpdateMessage(); err != nil {
		return
	}

	switch action {
	case "send":
		msg, err := h.A.SendDraft(ctx, id, act)
		if err != nil {
			h.logErr("send", err)
			msg = Friendly(err)
		}
		editOriginal(msg, &[]discord.Embed{})
	case "cancel":
		h.A.CancelDraft(id, act)
		editOriginal("Cancelled. Nothing was sent.", &[]discord.Embed{})
	case "ban", "ignore", "fwd":
		name := map[string]string{"ban": "ban", "ignore": "ignore", "fwd": "forward"}[action]
		msg, err := h.A.Press(ctx, name, id, act)
		if errors.Is(err, ErrExpired) {
			upd := ExpiredCardUpdate(e.Message.Embeds)
			if err := h.A.DC.Edit(ctx, e.Message.ChannelID.String(), e.Message.ID.String(), upd); err != nil {
				h.A.Log.Debug("could not close expired card", "err", err)
			}
			followup("This alert is past its 14 days, so the bot no longer has it. Ban manually if needed.")
			return
		}
		if err != nil {
			h.logErr(name, err)
			followup(Friendly(err))
			return
		}
		followup(msg)
	case "sa", "sf", "sd":
		n, err := strconv.ParseInt(id, 10, 64)
		if err != nil {
			return
		}
		dec := map[string]string{"sa": DecideApprove, "sf": DecideFollow, "sd": DecideDeny}[action]
		msg, err := h.A.DecideSubscription(ctx, n, dec, act)
		if err != nil {
			h.logErr("subscription", err)
			followup(Friendly(err))
			return
		}
		followup(msg)
	}
}
