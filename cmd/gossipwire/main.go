// Command gossipwire runs the GossipWire Discord bot.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/disgoorg/disgo"
	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/disgo/rest"

	"github.com/NullAngst/GossipWire/internal/app"
	"github.com/NullAngst/GossipWire/internal/config"
	"github.com/NullAngst/GossipWire/internal/fed"
	"github.com/NullAngst/GossipWire/internal/store"
)

// fail prints a message a person can act on and exits. No stack traces.
func fail(msg string) {
	fmt.Fprintln(os.Stderr, "GossipWire cannot start: "+msg)
	os.Exit(1)
}

func main() {
	cfg, warnings, err := config.Load(os.Getenv, os.ReadFile)
	if err != nil {
		fail(err.Error())
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	for _, w := range warnings {
		log.Warn(w)
	}
	if err := config.CheckDataDir(cfg.DataDir); err != nil {
		fail(err.Error())
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "gossipwire.db"))
	if err != nil {
		fail(err.Error())
	}
	defer st.Close()
	id, created, err := fed.LoadOrCreateIdentity(cfg.DataDir, cfg.InstanceHost)
	if err != nil {
		fail(err.Error())
	}
	if created {
		log.Info("created a new instance identity key", "fingerprint", fed.Fingerprint(id.Pub))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var a *app.App
	var h *app.Handlers
	ready := make(chan struct{}, 1)
	client, err := disgo.New(cfg.Token,
		bot.WithLogger(log),
		bot.WithGatewayConfigOpts(gateway.WithIntents(gateway.IntentGuilds)),
		bot.WithCacheConfigOpts(cache.WithCaches(cache.FlagGuilds)),
		bot.WithEventManagerConfigOpts(bot.WithAsyncEventsEnabled()),
		bot.WithEventListenerFunc(func(e *events.ApplicationCommandInteractionCreate) { h.OnCommand(e) }),
		bot.WithEventListenerFunc(func(e *events.ComponentInteractionCreate) { h.OnComponent(e) }),
		bot.WithEventListenerFunc(func(e *events.GuildsReady) {
			select {
			case ready <- struct{}{}:
			default:
			}
		}),
		bot.WithEventListenerFunc(func(e *events.GuildJoin) {
			log.Info("added to a server", "guild", e.Guild.ID, "name", e.Guild.Name, "servers", a.DC.GuildCount())
			if a.DC.GuildCount() >= 100 {
				log.Warn("this bot is in 100 servers, Discord's limit for unverified bots; new servers cannot add it and should self-host")
			}
		}),
	)
	if err != nil {
		fail("Discord rejected the bot token. It was probably reset or copied wrong. Get a new one from the developer portal under Bot > Reset Token.")
	}

	appInfo, err := client.Rest.GetBotApplicationInfo(rest.WithCtx(ctx))
	if err != nil {
		var re *rest.Error
		if errors.As(err, &re) && re.Response != nil && re.Response.StatusCode == http.StatusUnauthorized {
			fail("Discord rejected the bot token. It was probably reset or copied wrong. Get a new one from the developer portal under Bot > Reset Token.")
		}
		fail(fmt.Sprintf("Could not reach Discord to check the bot token: %v. Check this machine's internet connection and DNS.", err))
	}

	a = app.New(cfg, st, app.NewDisgo(client), id, log)
	a.AppID = appInfo.ID.String()
	h = &app.Handlers{A: a}
	if err := a.Init(ctx); err != nil {
		fail(err.Error())
	}

	if _, err := client.Rest.SetGlobalCommands(client.ApplicationID, app.Commands(), rest.WithCtx(ctx)); err != nil {
		fail(fmt.Sprintf("Could not register slash commands with Discord: %v", err))
	}

	if cfg.Reachable() {
		srv := &fed.Server{ID: id, Handler: a, Log: log, Now: time.Now}
		hs := &http.Server{
			Addr:              cfg.FedListen,
			Handler:           srv.Routes(),
			ReadHeaderTimeout: 10 * time.Second,
			// Long enough for a 40 MB body on a slow uplink, short enough
			// that a stalled sender gives back its slot (8 at a time).
			ReadTimeout:    5 * time.Minute,
			WriteTimeout:   5 * time.Minute,
			IdleTimeout:    2 * time.Minute,
			MaxHeaderBytes: 16 << 10,
		}
		ln, err := net.Listen("tcp", cfg.FedListen)
		if err != nil {
			fail(fmt.Sprintf("Cannot listen on FED_LISTEN %s: %v. Something else may be using that port.", cfg.FedListen, err))
		}
		go func() {
			if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("federation endpoint stopped", "err", err)
			}
		}()
		go func() {
			<-ctx.Done()
			sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			hs.Shutdown(sctx)
		}()
		log.Info("federation endpoint listening", "address", cfg.FedListen, "public", cfg.InstanceURL)
	}

	if err := client.OpenGateway(ctx); err != nil {
		fail(fmt.Sprintf("Could not connect to Discord: %v", err))
	}
	defer client.Close(context.Background())

	go a.RunWorkers(ctx)

	select {
	case <-ready:
	case <-time.After(30 * time.Second):
	case <-ctx.Done():
	}
	log.Info(a.StartupSummary(), "instance", id.ID, "federation_mode", cfg.FederationMode)

	<-ctx.Done()
	log.Info("shutting down")
}
