package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"
)

// Discord is everything the core needs from Discord. The real one wraps
// disgo's REST client; tests use a fake.
type Discord interface {
	Send(ctx context.Context, channelID string, m discord.MessageCreate) (messageID string, err error)
	Edit(ctx context.Context, channelID, messageID string, m discord.MessageUpdate) error
	Ban(ctx context.Context, guildID, userID, reason string) error
	IsMember(ctx context.Context, guildID, userID string) (bool, error)
	User(ctx context.Context, userID string) (*discord.User, error)
	DM(ctx context.Context, userID string, m discord.MessageCreate) error
	// Guild reads the gateway cache: server name and owner.
	Guild(guildID string) (name, ownerID string, ok bool)
	GuildCount() int
}

// ErrNotFound means Discord says the user, member or message does not exist.
var ErrNotFound = errors.New("not found")

// ErrForbidden means the bot lacks a permission, or the role order blocks it.
var ErrForbidden = errors.New("missing permissions")

type disgoDiscord struct {
	c *bot.Client
}

// NewDisgo wraps a disgo client.
func NewDisgo(c *bot.Client) Discord { return &disgoDiscord{c: c} }

func sf(s string) snowflake.ID {
	id, _ := snowflake.Parse(s)
	return id
}

func classify(err error) error {
	if err == nil {
		return nil
	}
	var re *rest.Error
	if errors.As(err, &re) && re.Response != nil {
		switch re.Response.StatusCode {
		case http.StatusNotFound:
			return fmt.Errorf("%w: %s", ErrNotFound, re.Message)
		case http.StatusForbidden:
			return fmt.Errorf("%w: %s", ErrForbidden, re.Message)
		}
	}
	return err
}

func (d *disgoDiscord) Send(ctx context.Context, channelID string, m discord.MessageCreate) (string, error) {
	msg, err := d.c.Rest.CreateMessage(sf(channelID), m, rest.WithCtx(ctx))
	if err != nil {
		return "", classify(err)
	}
	return msg.ID.String(), nil
}

func (d *disgoDiscord) Edit(ctx context.Context, channelID, messageID string, m discord.MessageUpdate) error {
	_, err := d.c.Rest.UpdateMessage(sf(channelID), sf(messageID), m, rest.WithCtx(ctx))
	return classify(err)
}

func (d *disgoDiscord) Ban(ctx context.Context, guildID, userID, reason string) error {
	return classify(d.c.Rest.AddBan(sf(guildID), sf(userID), 0, rest.WithCtx(ctx), rest.WithReason(reason)))
}

func (d *disgoDiscord) IsMember(ctx context.Context, guildID, userID string) (bool, error) {
	_, err := d.c.Rest.GetMember(sf(guildID), sf(userID), rest.WithCtx(ctx))
	if err == nil {
		return true, nil
	}
	if err = classify(err); errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return false, err
}

func (d *disgoDiscord) User(ctx context.Context, userID string) (*discord.User, error) {
	u, err := d.c.Rest.GetUser(sf(userID), rest.WithCtx(ctx))
	return u, classify(err)
}

func (d *disgoDiscord) DM(ctx context.Context, userID string, m discord.MessageCreate) error {
	ch, err := d.c.Rest.CreateDMChannel(sf(userID), rest.WithCtx(ctx))
	if err != nil {
		return classify(err)
	}
	_, err = d.c.Rest.CreateMessage(ch.ID(), m, rest.WithCtx(ctx))
	return classify(err)
}

func (d *disgoDiscord) Guild(guildID string) (string, string, bool) {
	g, ok := d.c.Caches.Guild(sf(guildID))
	if !ok {
		return "", "", false
	}
	return g.Name, g.OwnerID.String(), true
}

func (d *disgoDiscord) GuildCount() int { return d.c.Caches.GuildsLen() }

type discordID = snowflake.ID
