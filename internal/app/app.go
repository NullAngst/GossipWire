// Package app is GossipWire's core: setup, subscriptions, alerts, button
// presses, retractions, federation message handling and the background
// workers. Discord is reached through the Discord interface, so the whole
// flow can be tested without a live bot.
package app

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/NullAngst/GossipWire/internal/config"
	"github.com/NullAngst/GossipWire/internal/fed"
	"github.com/NullAngst/GossipWire/internal/store"
)

// App is one running instance.
type App struct {
	Cfg   *config.Config
	St    *store.Store
	DC    Discord
	ID    *fed.Identity
	Fed   *fed.Client
	Log   *slog.Logger
	Now   func() time.Time
	AppID string // Discord application ID, for the install link

	// FetchEvidence downloads an /alert attachment. Tests swap it out.
	FetchEvidence func(ctx context.Context, url string, limit int64) ([]byte, error)

	wake chan struct{}

	draftsMu sync.Mutex
	drafts   map[string]*Draft

	keysMu  sync.Mutex
	keyMemo map[string]keyMemo // origin keys fetched for verification only, never pinned

	backoffMu     sync.Mutex
	backoff       map[string]peerBackoff
	newPeerWindow time.Time
	newPeerCount  int
}

type keyMemo struct {
	pub ed25519.PublicKey
	at  time.Time
}

type peerBackoff struct {
	next  time.Time
	delay time.Duration
}

// Actor is the person doing something, and where.
type Actor struct {
	UserID    string
	UserName  string
	GuildID   string
	GuildName string
}

// New builds an App. Call Init before use.
func New(cfg *config.Config, st *store.Store, dc Discord, id *fed.Identity, log *slog.Logger) *App {
	return &App{
		Cfg: cfg, St: st, DC: dc, ID: id, Fed: fed.NewClient(), Log: log, Now: time.Now, FetchEvidence: downloadFromCDN,
		wake:    make(chan struct{}, 1),
		drafts:  map[string]*Draft{},
		keyMemo: map[string]keyMemo{},
		backoff: map[string]peerBackoff{},
	}
}

// Init records this instance as a row in instances, as the spec asks.
func (a *App) Init(ctx context.Context) error {
	reach := store.ReachOutbound
	if a.Cfg.Reachable() {
		reach = store.ReachReachable
	}
	now := a.Now().Unix()
	cur, err := a.St.GetInstance(ctx, a.ID.ID)
	if err != nil {
		return err
	}
	row := store.Instance{Host: a.ID.ID, PublicKey: a.ID.PubB64(), Reach: reach, Status: store.PeerAllowed, FirstSeen: now, LastSeen: now}
	if cur != nil {
		row.FirstSeen = cur.FirstSeen
	}
	return a.St.UpsertInstance(ctx, row)
}

// Self is this instance's address.
func (a *App) Self() string { return a.ID.ID }

// InstallLink is the link server admins use to add this bot.
func (a *App) InstallLink() string {
	return "https://discord.com/oauth2/authorize?client_id=" + a.AppID + "&scope=bot+applications.commands&permissions=52228"
}

func (a *App) evidenceDir(alertID string) string {
	return filepath.Join(a.Cfg.DataDir, "evidence", alertID)
}

// Wake nudges the outbox worker.
func (a *App) Wake() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// enqueue queues a message for a peer instance.
func (a *App) enqueue(ctx context.Context, peer, typ string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	if err := a.St.Enqueue(ctx, peer, typ, b, a.Now().Unix()); err != nil {
		return err
	}
	a.Wake()
	return nil
}

// guildName prefers the gateway cache, then a stored fallback.
func (a *App) guildName(id, fallback string) string {
	if n, _, ok := a.DC.Guild(id); ok && n != "" {
		return n
	}
	if fallback != "" {
		return fallback
	}
	return id
}

// addr renders a server address the way users type it.
func (a *App) addr(guild, host string) string {
	if host == a.Self() {
		return guild
	}
	return guild + "@" + host
}

// UserError is shown to the person as-is.
type UserError struct{ Msg string }

func (e *UserError) Error() string { return e.Msg }

func userErr(format string, args ...any) error { return &UserError{Msg: fmt.Sprintf(format, args...)} }

// Friendly turns any error into text for an ephemeral reply.
func Friendly(err error) string {
	var ue *UserError
	if errors.As(err, &ue) {
		return ue.Msg
	}
	return "Something went wrong on the bot's side. The instance operator can see the details in the logs."
}

// ---------------------------------------------------------------- server setup

// Setup sets or moves a server's alert channel. It posts a test message
// first, so a channel the bot cannot write to fails here and not later.
func (a *App) Setup(ctx context.Context, act Actor, channelID string) (string, error) {
	_, err := a.DC.Send(ctx, channelID, plain("GossipWire alerts for this server will post in this channel. "+
		"Use `/subscribe` to follow another server's alerts, and `/alert` here to send one."))
	if err != nil {
		if errors.Is(err, ErrForbidden) {
			return "", userErr("I cannot post in <#%s>. Give my role View Channel, Send Messages, Embed Links and Attach Files there, then run /setup again.", channelID)
		}
		a.Log.Warn("setup test message failed", "guild", act.GuildID, "err", err)
		return "", userErr("I could not post in <#%s>. Check that it is a text channel I can see, then run /setup again.", channelID)
	}
	if err := a.St.SetGuild(ctx, store.Guild{ID: act.GuildID, ChannelID: channelID, ConfiguredBy: act.UserID, ConfiguredAt: a.Now().Unix()}); err != nil {
		return "", err
	}
	a.Log.Info("server set up", "guild", act.GuildID, "channel", channelID, "by", act.UserID)
	return fmt.Sprintf("Alert channel set to <#%s>. This server's address for others to subscribe to is `%s`.", channelID, a.ServerAddress(act.GuildID)), nil
}

// ServerAddress is what other servers type into /subscribe to follow this one.
func (a *App) ServerAddress(guild string) string {
	if a.Cfg.FederationMode == config.ModeOff {
		return guild
	}
	return guild + "@" + a.Self()
}

// SetPaused stops or restarts incoming alerts.
func (a *App) SetPaused(ctx context.Context, act Actor, paused bool) (string, error) {
	g, err := a.requireSetup(ctx, act.GuildID)
	if err != nil {
		return "", err
	}
	if g.Paused == paused {
		if paused {
			return "Incoming alerts are already paused. Run /resume to start them again.", nil
		}
		return "Incoming alerts are not paused.", nil
	}
	if err := a.St.SetPaused(ctx, act.GuildID, paused); err != nil {
		return "", err
	}
	if paused {
		return "Incoming alerts are paused. Subscriptions are untouched, and alerts sent while paused are not delivered here. Run /resume to start again.", nil
	}
	return "Incoming alerts are back on.", nil
}

func (a *App) requireSetup(ctx context.Context, guild string) (*store.Guild, error) {
	g, err := a.St.GetGuild(ctx, guild)
	if err != nil {
		return nil, err
	}
	if g == nil {
		return nil, userErr("This server has not picked an alert channel yet. Run `/setup channel:#your-mod-channel` first.")
	}
	return g, nil
}

// ---------------------------------------------------------------- retention

// Purge deletes alerts past their 14 days, with their evidence, hops,
// recipients, deliveries, actions and old outbox rows.
func (a *App) Purge(ctx context.Context) {
	cutoff := a.Now().Add(-fed.AlertLifetime).Unix()
	old, err := a.St.ExpiredAlerts(ctx, cutoff)
	if err != nil {
		a.Log.Warn("purge: listing expired alerts failed", "err", err)
		return
	}
	for _, al := range old {
		if err := a.St.DeleteAlert(ctx, al.ID); err != nil {
			a.Log.Warn("purge: delete failed", "alert", al.ID, "err", err)
			continue
		}
		os.RemoveAll(a.evidenceDir(al.ID))
	}
	if err := a.St.PurgeOutbox(ctx, cutoff); err != nil {
		a.Log.Warn("purge: outbox cleanup failed", "err", err)
	}
	a.draftsMu.Lock()
	for k, d := range a.drafts {
		if a.Now().Sub(d.Created) > draftLifetime {
			delete(a.drafts, k)
		}
	}
	a.draftsMu.Unlock()
	if len(old) > 0 {
		a.Log.Info("purged expired alerts", "count", len(old))
	}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-3]) + "..."
}

func cleanName(s string) string {
	s = strings.ReplaceAll(s, "`", "'")
	return truncate(strings.TrimSpace(s), 100)
}
