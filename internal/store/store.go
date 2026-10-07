// Package store keeps GossipWire's state in one SQLite file under /data.
// One process owns the file and writes are rare, so it runs on a single
// connection. Callers must finish reading rows before running the next query,
// which every function here does for them.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	_ "github.com/ncruces/go-sqlite3/driver"
	_ "github.com/ncruces/go-sqlite3/embed"
)

// ErrDuplicate is returned when a unique row already exists.
var ErrDuplicate = errors.New("already exists")

// Store wraps the database.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS guilds (
	guild_id         TEXT PRIMARY KEY,
	alert_channel_id TEXT NOT NULL,
	configured_by    TEXT NOT NULL,
	configured_at    INTEGER NOT NULL,
	paused           INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS instances (
	host        TEXT PRIMARY KEY,
	public_key  TEXT NOT NULL DEFAULT '',
	reach       TEXT NOT NULL,
	status      TEXT NOT NULL,
	pending_key TEXT NOT NULL DEFAULT '',
	poll_cursor INTEGER NOT NULL DEFAULT 0,
	first_seen  INTEGER NOT NULL,
	last_seen   INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS subscriptions (
	id               INTEGER PRIMARY KEY,
	publisher_guild  TEXT NOT NULL,
	publisher_host   TEXT NOT NULL,
	publisher_name   TEXT NOT NULL DEFAULT '',
	subscriber_guild TEXT NOT NULL,
	subscriber_host  TEXT NOT NULL,
	subscriber_name  TEXT NOT NULL DEFAULT '',
	status           TEXT NOT NULL,
	requested_by     TEXT NOT NULL DEFAULT '',
	approved_by      TEXT NOT NULL DEFAULT '',
	created_at       INTEGER NOT NULL,
	approved_at      INTEGER NOT NULL DEFAULT 0,
	card_channel     TEXT NOT NULL DEFAULT '',
	card_message     TEXT NOT NULL DEFAULT '',
	UNIQUE (publisher_guild, publisher_host, subscriber_guild, subscriber_host)
);
CREATE TABLE IF NOT EXISTS alerts (
	alert_id          TEXT PRIMARY KEY,
	origin_guild      TEXT NOT NULL,
	origin_host       TEXT NOT NULL,
	origin_guild_name TEXT NOT NULL,
	sender_user_id    TEXT NOT NULL,
	sender_name       TEXT NOT NULL,
	target_user_id    TEXT NOT NULL,
	target_name       TEXT NOT NULL,
	target_avatar     TEXT NOT NULL DEFAULT '',
	reason            TEXT NOT NULL,
	evidence_path     TEXT NOT NULL DEFAULT '',
	evidence_name     TEXT NOT NULL DEFAULT '',
	evidence_type     TEXT NOT NULL DEFAULT '',
	evidence_size     INTEGER NOT NULL DEFAULT 0,
	evidence_sha      TEXT NOT NULL DEFAULT '',
	evidence_link     TEXT NOT NULL DEFAULT '',
	created_at        INTEGER NOT NULL,
	retracted_at      INTEGER NOT NULL DEFAULT 0,
	retract_sig       TEXT NOT NULL DEFAULT '',
	status            TEXT NOT NULL DEFAULT 'sent',
	origin_verified   INTEGER NOT NULL DEFAULT 0,
	signed            BLOB NOT NULL,
	sig               TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS alerts_target ON alerts(target_user_id);
CREATE INDEX IF NOT EXISTS alerts_origin ON alerts(origin_guild, origin_host, created_at);
CREATE TABLE IF NOT EXISTS alert_hops (
	alert_id          TEXT NOT NULL,
	hop               INTEGER NOT NULL,
	guild_id          TEXT NOT NULL,
	host              TEXT NOT NULL,
	guild_name        TEXT NOT NULL,
	forwarded_by      TEXT NOT NULL,
	forwarded_by_name TEXT NOT NULL,
	signature         TEXT NOT NULL,
	at                INTEGER NOT NULL,
	verified          INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (alert_id, guild_id, host)
);
CREATE TABLE IF NOT EXISTS alert_recipients (
	alert_id  TEXT NOT NULL,
	guild_id  TEXT NOT NULL,
	host      TEXT NOT NULL,
	via_guild TEXT NOT NULL,
	via_host  TEXT NOT NULL,
	at        INTEGER NOT NULL,
	PRIMARY KEY (alert_id, guild_id, host)
);
CREATE TABLE IF NOT EXISTS deliveries (
	alert_id   TEXT NOT NULL,
	guild_id   TEXT NOT NULL,
	channel_id TEXT NOT NULL DEFAULT '',
	message_id TEXT NOT NULL DEFAULT '',
	status     TEXT NOT NULL,
	error      TEXT NOT NULL DEFAULT '',
	path       TEXT NOT NULL DEFAULT '[]',
	via_host   TEXT NOT NULL DEFAULT '',
	in_server  INTEGER NOT NULL DEFAULT 0,
	at         INTEGER NOT NULL,
	UNIQUE (alert_id, guild_id)
);
CREATE INDEX IF NOT EXISTS deliveries_guild ON deliveries(guild_id, at);
CREATE TABLE IF NOT EXISTS actions (
	id             INTEGER PRIMARY KEY,
	alert_id       TEXT NOT NULL,
	guild_id       TEXT NOT NULL,
	moderator_id   TEXT NOT NULL,
	moderator_name TEXT NOT NULL DEFAULT '',
	action         TEXT NOT NULL,
	at             INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS actions_decision ON actions(alert_id, guild_id) WHERE action IN ('ban', 'ignore');
CREATE UNIQUE INDEX IF NOT EXISTS actions_forward ON actions(alert_id, guild_id) WHERE action = 'forward';
CREATE INDEX IF NOT EXISTS actions_rate ON actions(guild_id, action, at);
CREATE TABLE IF NOT EXISTS outbox (
	seq          INTEGER PRIMARY KEY AUTOINCREMENT,
	peer_host    TEXT NOT NULL,
	type         TEXT NOT NULL,
	payload      BLOB NOT NULL,
	created_at   INTEGER NOT NULL,
	delivered_at INTEGER NOT NULL DEFAULT 0,
	attempts     INTEGER NOT NULL DEFAULT 0,
	next_try     INTEGER NOT NULL DEFAULT 0,
	last_error   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS outbox_peer ON outbox(peer_host, delivered_at, seq);
`

// Open opens or creates the database at path.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(wal)&_pragma=synchronous(normal)"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("could not set up the database at %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func isUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---------------------------------------------------------------- settings

// Limits are the day-to-day knobs set with /instance limits.
type Limits struct {
	PerHour    int
	PerDay     int
	EvidenceMB int
}

// DefaultLimits match the spec: 5 per hour, 20 per day, 8 MB evidence.
var DefaultLimits = Limits{PerHour: 5, PerDay: 20, EvidenceMB: 8}

// GetLimits reads the limits, falling back to defaults.
func (s *Store) GetLimits(ctx context.Context) Limits {
	l := DefaultLimits
	get := func(k string, dst *int) {
		var v string
		if s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, k).Scan(&v) == nil {
			if n, err := strconv.Atoi(v); err == nil {
				*dst = n
			}
		}
	}
	get("per_hour", &l.PerHour)
	get("per_day", &l.PerDay)
	get("evidence_mb", &l.EvidenceMB)
	return l
}

// SetLimits saves the limits.
func (s *Store) SetLimits(ctx context.Context, l Limits) error {
	for k, v := range map[string]int{"per_hour": l.PerHour, "per_day": l.PerDay, "evidence_mb": l.EvidenceMB} {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, k, strconv.Itoa(v)); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------- guilds

// Guild is a server that ran /setup on this instance.
type Guild struct {
	ID           string
	ChannelID    string
	ConfiguredBy string
	ConfiguredAt int64
	Paused       bool
}

// SetGuild creates or moves a server's alert channel. Pause state is kept.
func (s *Store) SetGuild(ctx context.Context, g Guild) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO guilds(guild_id, alert_channel_id, configured_by, configured_at) VALUES(?, ?, ?, ?)
		ON CONFLICT(guild_id) DO UPDATE SET alert_channel_id = excluded.alert_channel_id, configured_by = excluded.configured_by, configured_at = excluded.configured_at`,
		g.ID, g.ChannelID, g.ConfiguredBy, g.ConfiguredAt)
	return err
}

// GetGuild returns nil when the server has not run /setup.
func (s *Store) GetGuild(ctx context.Context, id string) (*Guild, error) {
	var g Guild
	var paused int
	err := s.db.QueryRowContext(ctx, `SELECT guild_id, alert_channel_id, configured_by, configured_at, paused FROM guilds WHERE guild_id = ?`, id).
		Scan(&g.ID, &g.ChannelID, &g.ConfiguredBy, &g.ConfiguredAt, &paused)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	g.Paused = paused == 1
	return &g, err
}

// SetPaused stops or restarts incoming alerts for a server.
func (s *Store) SetPaused(ctx context.Context, id string, paused bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE guilds SET paused = ? WHERE guild_id = ?`, b2i(paused), id)
	return err
}

// CountGuilds returns how many servers ran /setup.
func (s *Store) CountGuilds(ctx context.Context) int {
	var n int
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM guilds`).Scan(&n)
	return n
}

// ---------------------------------------------------------------- instances

// Peer statuses and reach values.
const (
	PeerAllowed    = "allowed"
	PeerBlocked    = "blocked"
	PeerKeyChanged = "key_changed"

	ReachReachable = "reachable"
	ReachOutbound  = "outbound"
)

// Instance is a known peer, or this instance itself.
type Instance struct {
	Host       string
	PublicKey  string
	Reach      string
	Status     string
	PendingKey string
	PollCursor int64
	FirstSeen  int64
	LastSeen   int64
}

const instanceCols = `host, public_key, reach, status, pending_key, poll_cursor, first_seen, last_seen`

func scanInstance(sc interface{ Scan(...any) error }) (*Instance, error) {
	var i Instance
	err := sc.Scan(&i.Host, &i.PublicKey, &i.Reach, &i.Status, &i.PendingKey, &i.PollCursor, &i.FirstSeen, &i.LastSeen)
	return &i, err
}

// GetInstance returns nil if the host is unknown.
func (s *Store) GetInstance(ctx context.Context, host string) (*Instance, error) {
	i, err := scanInstance(s.db.QueryRowContext(ctx, `SELECT `+instanceCols+` FROM instances WHERE host = ?`, host))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return i, err
}

// UpsertInstance writes every column.
func (s *Store) UpsertInstance(ctx context.Context, i Instance) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO instances(`+instanceCols+`) VALUES(?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(host) DO UPDATE SET public_key = excluded.public_key, reach = excluded.reach, status = excluded.status,
		pending_key = excluded.pending_key, poll_cursor = excluded.poll_cursor, last_seen = excluded.last_seen`,
		i.Host, i.PublicKey, i.Reach, i.Status, i.PendingKey, i.PollCursor, i.FirstSeen, i.LastSeen)
	return err
}

// ListInstances returns every known instance, this one included.
func (s *Store) ListInstances(ctx context.Context) ([]Instance, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+instanceCols+` FROM instances ORDER BY host`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Instance
	for rows.Next() {
		i, err := scanInstance(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *i)
	}
	return out, rows.Err()
}

// TouchInstance records that a peer was heard from.
func (s *Store) TouchInstance(ctx context.Context, host string, now int64) {
	s.db.ExecContext(ctx, `UPDATE instances SET last_seen = ? WHERE host = ?`, now, host)
}

// SetPollCursor saves how far this instance has read a peer's outbox.
func (s *Store) SetPollCursor(ctx context.Context, host string, cursor int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE instances SET poll_cursor = ? WHERE host = ?`, cursor, host)
	return err
}

// ---------------------------------------------------------------- subscriptions

// Subscription statuses.
const (
	SubPending = "pending"
	SubActive  = "active"
	SubDenied  = "denied"
)

// Subscription is one direction: publisher's alerts flow to subscriber.
type Subscription struct {
	ID          int64
	PubGuild    string
	PubHost     string
	PubName     string
	SubGuild    string
	SubHost     string
	SubName     string
	Status      string
	RequestedBy string
	ApprovedBy  string
	CreatedAt   int64
	ApprovedAt  int64
	CardChannel string
	CardMessage string
}

const subCols = `id, publisher_guild, publisher_host, publisher_name, subscriber_guild, subscriber_host, subscriber_name, status,
	requested_by, approved_by, created_at, approved_at, card_channel, card_message`

func scanSub(sc interface{ Scan(...any) error }) (*Subscription, error) {
	var x Subscription
	err := sc.Scan(&x.ID, &x.PubGuild, &x.PubHost, &x.PubName, &x.SubGuild, &x.SubHost, &x.SubName, &x.Status,
		&x.RequestedBy, &x.ApprovedBy, &x.CreatedAt, &x.ApprovedAt, &x.CardChannel, &x.CardMessage)
	return &x, err
}

func (s *Store) querySubs(ctx context.Context, q string, args ...any) ([]Subscription, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+subCols+` FROM subscriptions `+q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Subscription
	for rows.Next() {
		x, err := scanSub(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *x)
	}
	return out, rows.Err()
}

// GetSub finds one direction, or nil.
func (s *Store) GetSub(ctx context.Context, pubGuild, pubHost, subGuild, subHost string) (*Subscription, error) {
	x, err := scanSub(s.db.QueryRowContext(ctx, `SELECT `+subCols+` FROM subscriptions
		WHERE publisher_guild = ? AND publisher_host = ? AND subscriber_guild = ? AND subscriber_host = ?`, pubGuild, pubHost, subGuild, subHost))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return x, err
}

// GetSubByID finds a subscription by its row ID, or nil.
func (s *Store) GetSubByID(ctx context.Context, id int64) (*Subscription, error) {
	x, err := scanSub(s.db.QueryRowContext(ctx, `SELECT `+subCols+` FROM subscriptions WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return x, err
}

// PutSub inserts a subscription, or resets an existing one (same four keys)
// to the given values. It returns the row ID.
func (s *Store) PutSub(ctx context.Context, x Subscription) (int64, error) {
	_, err := s.db.ExecContext(ctx, `INSERT INTO subscriptions(publisher_guild, publisher_host, publisher_name, subscriber_guild, subscriber_host,
		subscriber_name, status, requested_by, approved_by, created_at, approved_at, card_channel, card_message)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(publisher_guild, publisher_host, subscriber_guild, subscriber_host) DO UPDATE SET
		publisher_name = excluded.publisher_name, subscriber_name = excluded.subscriber_name, status = excluded.status,
		requested_by = excluded.requested_by, approved_by = excluded.approved_by, created_at = excluded.created_at,
		approved_at = excluded.approved_at, card_channel = excluded.card_channel, card_message = excluded.card_message`,
		x.PubGuild, x.PubHost, x.PubName, x.SubGuild, x.SubHost, x.SubName, x.Status, x.RequestedBy, x.ApprovedBy,
		x.CreatedAt, x.ApprovedAt, x.CardChannel, x.CardMessage)
	if err != nil {
		return 0, err
	}
	var id int64
	err = s.db.QueryRowContext(ctx, `SELECT id FROM subscriptions WHERE publisher_guild = ? AND publisher_host = ? AND subscriber_guild = ? AND subscriber_host = ?`,
		x.PubGuild, x.PubHost, x.SubGuild, x.SubHost).Scan(&id)
	return id, err
}

// UpdateSub writes the mutable columns of an existing subscription.
func (s *Store) UpdateSub(ctx context.Context, x Subscription) error {
	_, err := s.db.ExecContext(ctx, `UPDATE subscriptions SET publisher_name = ?, subscriber_name = ?, status = ?, approved_by = ?,
		approved_at = ?, card_channel = ?, card_message = ? WHERE id = ?`,
		x.PubName, x.SubName, x.Status, x.ApprovedBy, x.ApprovedAt, x.CardChannel, x.CardMessage, x.ID)
	return err
}

// DeleteSub removes a subscription.
func (s *Store) DeleteSub(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM subscriptions WHERE id = ?`, id)
	return err
}

// ActiveSubscribers lists who receives a publisher's alerts.
func (s *Store) ActiveSubscribers(ctx context.Context, pubGuild, pubHost string) ([]Subscription, error) {
	return s.querySubs(ctx, `WHERE publisher_guild = ? AND publisher_host = ? AND status = ? ORDER BY id`, pubGuild, pubHost, SubActive)
}

// SubsForGuild lists every subscription a local server is part of.
func (s *Store) SubsForGuild(ctx context.Context, guild, self string) ([]Subscription, error) {
	return s.querySubs(ctx, `WHERE (publisher_guild = ? AND publisher_host = ?) OR (subscriber_guild = ? AND subscriber_host = ?) ORDER BY status, id`,
		guild, self, guild, self)
}

// CountPendingFor counts subscribe requests waiting on a publisher server.
func (s *Store) CountPendingFor(ctx context.Context, pubGuild, pubHost string) int {
	var n int
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM subscriptions WHERE publisher_guild = ? AND publisher_host = ? AND status = ?`,
		pubGuild, pubHost, SubPending).Scan(&n)
	return n
}

// DeleteSubsWithHost drops every subscription involving a peer instance.
func (s *Store) DeleteSubsWithHost(ctx context.Context, host string) (int64, error) {
	r, err := s.db.ExecContext(ctx, `DELETE FROM subscriptions WHERE publisher_host = ? OR subscriber_host = ?`, host, host)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}

// ---------------------------------------------------------------- alerts

// Alert statuses.
const (
	AlertSent = "sent"
	AlertHeld = "held"
)

// Alert is the stored claim.
type Alert struct {
	ID              string
	OriginGuild     string
	OriginHost      string
	OriginGuildName string
	SenderID        string
	SenderName      string
	TargetID        string
	TargetName      string
	TargetAvatar    string
	Reason          string
	EvidencePath    string
	EvidenceName    string
	EvidenceType    string
	EvidenceSize    int64
	EvidenceSHA     string
	EvidenceLink    string
	CreatedAt       int64
	RetractedAt     int64
	RetractSig      string
	Status          string
	OriginVerified  bool
	Signed          []byte
	Sig             string
}

const alertCols = `alert_id, origin_guild, origin_host, origin_guild_name, sender_user_id, sender_name, target_user_id, target_name,
	target_avatar, reason, evidence_path, evidence_name, evidence_type, evidence_size, evidence_sha, evidence_link, created_at,
	retracted_at, retract_sig, status, origin_verified, signed, sig`

func scanAlert(sc interface{ Scan(...any) error }) (*Alert, error) {
	var a Alert
	var ov int
	err := sc.Scan(&a.ID, &a.OriginGuild, &a.OriginHost, &a.OriginGuildName, &a.SenderID, &a.SenderName, &a.TargetID, &a.TargetName,
		&a.TargetAvatar, &a.Reason, &a.EvidencePath, &a.EvidenceName, &a.EvidenceType, &a.EvidenceSize, &a.EvidenceSHA, &a.EvidenceLink,
		&a.CreatedAt, &a.RetractedAt, &a.RetractSig, &a.Status, &ov, &a.Signed, &a.Sig)
	a.OriginVerified = ov == 1
	return &a, err
}

// InsertAlert stores an alert. It returns ErrDuplicate if it already exists.
func (s *Store) InsertAlert(ctx context.Context, a Alert) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO alerts(`+alertCols+`) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.OriginGuild, a.OriginHost, a.OriginGuildName, a.SenderID, a.SenderName, a.TargetID, a.TargetName,
		a.TargetAvatar, a.Reason, a.EvidencePath, a.EvidenceName, a.EvidenceType, a.EvidenceSize, a.EvidenceSHA, a.EvidenceLink,
		a.CreatedAt, a.RetractedAt, a.RetractSig, a.Status, b2i(a.OriginVerified), a.Signed, a.Sig)
	if isUnique(err) {
		return ErrDuplicate
	}
	return err
}

// GetAlert returns nil if the alert is unknown or purged.
func (s *Store) GetAlert(ctx context.Context, id string) (*Alert, error) {
	a, err := scanAlert(s.db.QueryRowContext(ctx, `SELECT `+alertCols+` FROM alerts WHERE alert_id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return a, err
}

// SetAlertStatus moves an alert between held and sent.
func (s *Store) SetAlertStatus(ctx context.Context, id, status string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE alerts SET status = ? WHERE alert_id = ?`, status, id)
	return err
}

// SetEvidenceFile records where the evidence file landed, under the name and
// type it was saved as.
func (s *Store) SetEvidenceFile(ctx context.Context, id, path, name, contentType string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE alerts SET evidence_path = ?, evidence_name = ?, evidence_type = ? WHERE alert_id = ?`,
		path, name, contentType, id)
	return err
}

// SetRetracted marks an alert retracted. It reports false if it already was.
func (s *Store) SetRetracted(ctx context.Context, id string, at int64, sig string) (bool, error) {
	r, err := s.db.ExecContext(ctx, `UPDATE alerts SET retracted_at = ?, retract_sig = ? WHERE alert_id = ? AND retracted_at = 0`, at, sig, id)
	if err != nil {
		return false, err
	}
	n, _ := r.RowsAffected()
	return n == 1, nil
}

// DeleteAlert removes an alert and everything hanging off it, all or nothing.
func (s *Store) DeleteAlert(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, t := range []string{"alerts", "alert_hops", "alert_recipients", "deliveries", "actions"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+t+` WHERE alert_id = ?`, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SetOriginVerified records that an alert's origin signature checked out.
func (s *Store) SetOriginVerified(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE alerts SET origin_verified = 1 WHERE alert_id = ?`, id)
	return err
}

// ReplaceClaim swaps an unverified alert's content for a verified copy of
// the same ID. Evidence is cleared; the caller saves the new file.
func (s *Store) ReplaceClaim(ctx context.Context, a Alert) error {
	_, err := s.db.ExecContext(ctx, `UPDATE alerts SET origin_guild = ?, origin_host = ?, origin_guild_name = ?, sender_user_id = ?,
		sender_name = ?, target_user_id = ?, target_name = ?, target_avatar = ?, reason = ?, evidence_path = '', evidence_name = ?,
		evidence_type = ?, evidence_size = ?, evidence_sha = ?, evidence_link = ?, created_at = ?, origin_verified = 1, signed = ?, sig = ?
		WHERE alert_id = ? AND origin_verified = 0`,
		a.OriginGuild, a.OriginHost, a.OriginGuildName, a.SenderID, a.SenderName, a.TargetID, a.TargetName, a.TargetAvatar, a.Reason,
		a.EvidenceName, a.EvidenceType, a.EvidenceSize, a.EvidenceSHA, a.EvidenceLink, a.CreatedAt, a.Signed, a.Sig, a.ID)
	return err
}

// CountSends counts alerts a server sent plus forwards it made since a time.
// Held alerts do not count until released.
func (s *Store) CountSends(ctx context.Context, guild, self string, since int64) (int, error) {
	var a, f int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM alerts WHERE origin_guild = ? AND origin_host = ? AND status = ? AND created_at >= ?`,
		guild, self, AlertSent, since).Scan(&a); err != nil {
		return 0, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM actions WHERE guild_id = ? AND action = 'forward' AND at >= ?`, guild, since).Scan(&f); err != nil {
		return 0, err
	}
	return a + f, nil
}

// ---------------------------------------------------------------- hops

// Hop is one stored forward.
type Hop struct {
	AlertID   string
	N         int
	GuildID   string
	Host      string
	GuildName string
	ByID      string
	ByName    string
	Sig       string
	At        int64
	Verified  bool // signature checked against the forwarding instance's key
}

// InsertHop stores a hop. A server forwards an alert at most once, so a
// repeat is ignored, with one exception: a verified hop replaces an
// unverified one. Otherwise a peer could plant a made-up hop for some other
// server first, and every later path through that server would show it.
func (s *Store) InsertHop(ctx context.Context, h Hop) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO alert_hops(alert_id, hop, guild_id, host, guild_name, forwarded_by, forwarded_by_name, signature, at, verified)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(alert_id, guild_id, host) DO UPDATE SET hop = excluded.hop, guild_name = excluded.guild_name,
		forwarded_by = excluded.forwarded_by, forwarded_by_name = excluded.forwarded_by_name, signature = excluded.signature,
		at = excluded.at, verified = 1
		WHERE excluded.verified = 1 AND alert_hops.verified = 0`,
		h.AlertID, h.N, h.GuildID, h.Host, h.GuildName, h.ByID, h.ByName, h.Sig, h.At, b2i(h.Verified))
	return err
}

// HopsByKey returns the stored hops for an alert, keyed by guild@host.
func (s *Store) HopsByKey(ctx context.Context, alertID string) (map[string]Hop, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT alert_id, hop, guild_id, host, guild_name, forwarded_by, forwarded_by_name, signature, at, verified
		FROM alert_hops WHERE alert_id = ?`, alertID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Hop{}
	for rows.Next() {
		var h Hop
		var v int
		if err := rows.Scan(&h.AlertID, &h.N, &h.GuildID, &h.Host, &h.GuildName, &h.ByID, &h.ByName, &h.Sig, &h.At, &v); err != nil {
			return nil, err
		}
		h.Verified = v == 1
		out[h.GuildID+"@"+h.Host] = h
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- recipients

// AddRecipient logs that this instance sent an alert to a server. It reports
// false if it already had, in which case the caller must not send again.
func (s *Store) AddRecipient(ctx context.Context, alertID, guild, host, viaGuild, viaHost string, at int64) (bool, error) {
	r, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO alert_recipients(alert_id, guild_id, host, via_guild, via_host, at) VALUES(?, ?, ?, ?, ?, ?)`,
		alertID, guild, host, viaGuild, viaHost, at)
	if err != nil {
		return false, err
	}
	n, _ := r.RowsAffected()
	return n == 1, nil
}

// RecipientHosts lists every instance this one sent an alert to.
func (s *Store) RecipientHosts(ctx context.Context, alertID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT host FROM alert_recipients WHERE alert_id = ? ORDER BY host`, alertID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- deliveries

// Delivery statuses.
const (
	DeliverySending = "sending"
	DeliverySent    = "sent"
	DeliveryFailed  = "failed"
)

// Delivery is one server on this instance that received an alert.
type Delivery struct {
	AlertID   string
	GuildID   string
	ChannelID string
	MessageID string
	Status    string
	Error     string
	Path      []string // hop keys (guild@host) from the origin to this server, origin excluded
	ViaHost   string
	InServer  bool
	At        int64
}

const deliveryCols = `alert_id, guild_id, channel_id, message_id, status, error, path, via_host, in_server, at`

func scanDelivery(sc interface{ Scan(...any) error }) (*Delivery, error) {
	var d Delivery
	var path string
	var in int
	err := sc.Scan(&d.AlertID, &d.GuildID, &d.ChannelID, &d.MessageID, &d.Status, &d.Error, &path, &d.ViaHost, &in, &d.At)
	if err == nil {
		json.Unmarshal([]byte(path), &d.Path)
		d.InServer = in == 1
	}
	return &d, err
}

// ClaimDelivery reserves the (alert, server) slot before anything is posted.
// It returns ErrDuplicate if the server already has this alert. This unique
// key is the receiving-side dedup.
func (s *Store) ClaimDelivery(ctx context.Context, d Delivery) error {
	path, _ := json.Marshal(d.Path)
	if d.Path == nil {
		path = []byte("[]")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO deliveries(`+deliveryCols+`) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.AlertID, d.GuildID, d.ChannelID, d.MessageID, d.Status, d.Error, string(path), d.ViaHost, b2i(d.InServer), d.At)
	if isUnique(err) {
		return ErrDuplicate
	}
	return err
}

// FinishDelivery records the posted message, or the failure.
func (s *Store) FinishDelivery(ctx context.Context, d Delivery) error {
	_, err := s.db.ExecContext(ctx, `UPDATE deliveries SET channel_id = ?, message_id = ?, status = ?, error = ?, in_server = ? WHERE alert_id = ? AND guild_id = ?`,
		d.ChannelID, d.MessageID, d.Status, d.Error, b2i(d.InServer), d.AlertID, d.GuildID)
	return err
}

// GetDelivery returns nil if the server never received the alert.
func (s *Store) GetDelivery(ctx context.Context, alertID, guild string) (*Delivery, error) {
	d, err := scanDelivery(s.db.QueryRowContext(ctx, `SELECT `+deliveryCols+` FROM deliveries WHERE alert_id = ? AND guild_id = ?`, alertID, guild))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return d, err
}

// DeliveriesForAlert lists every local copy of an alert.
func (s *Store) DeliveriesForAlert(ctx context.Context, alertID string) ([]Delivery, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+deliveryCols+` FROM deliveries WHERE alert_id = ?`, alertID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Delivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- actions

// Action names.
const (
	ActBan     = "ban"
	ActIgnore  = "ignore"
	ActForward = "forward"
)

// Action is who pressed what, where.
type Action struct {
	AlertID       string
	GuildID       string
	ModeratorID   string
	ModeratorName string
	Action        string
	At            int64
}

// InsertAction logs a button press. Ban and Ignore share one slot per server
// and alert, and Forward has its own, so the first press wins and later ones
// get ErrDuplicate.
func (s *Store) InsertAction(ctx context.Context, a Action) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO actions(alert_id, guild_id, moderator_id, moderator_name, action, at) VALUES(?, ?, ?, ?, ?, ?)`,
		a.AlertID, a.GuildID, a.ModeratorID, a.ModeratorName, a.Action, a.At)
	if isUnique(err) {
		return ErrDuplicate
	}
	return err
}

// DeleteAction undoes a press that failed, so it can be tried again.
func (s *Store) DeleteAction(ctx context.Context, alertID, guild, action string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM actions WHERE alert_id = ? AND guild_id = ? AND action = ?`, alertID, guild, action)
	return err
}

// ActionsFor lists presses on one alert in one server, oldest first.
func (s *Store) ActionsFor(ctx context.Context, alertID, guild string) ([]Action, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT alert_id, guild_id, moderator_id, moderator_name, action, at FROM actions
		WHERE alert_id = ? AND guild_id = ? ORDER BY id`, alertID, guild)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Action
	for rows.Next() {
		var a Action
		if err := rows.Scan(&a.AlertID, &a.GuildID, &a.ModeratorID, &a.ModeratorName, &a.Action, &a.At); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- history

// HistoryItem is one line of /history.
type HistoryItem struct {
	Alert    Alert
	Sent     bool // this server is the origin
	Delivery *Delivery
}

// History lists alerts a server sent or received since a time, newest first.
// targetID filters by target when not empty.
func (s *Store) History(ctx context.Context, guild, self, targetID string, since int64, limit int) ([]HistoryItem, error) {
	q := `SELECT ` + prefixCols("a.", alertCols) + `, d.alert_id IS NOT NULL, ` + prefixCols("d.", deliveryCols) + `
		FROM alerts a LEFT JOIN deliveries d ON d.alert_id = a.alert_id AND d.guild_id = ?
		WHERE a.created_at >= ? AND ((a.origin_guild = ? AND a.origin_host = ?) OR d.alert_id IS NOT NULL)`
	args := []any{guild, since, guild, self}
	if targetID != "" {
		q += ` AND a.target_user_id = ?`
		args = append(args, targetID)
	}
	q += ` ORDER BY a.created_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HistoryItem
	for rows.Next() {
		var a Alert
		var ov int
		var hasD bool
		var d Delivery
		var dAlert, dGuild, dCh, dMsg, dStatus, dErr, dPath, dVia sql.NullString
		var dIn, dAt sql.NullInt64
		if err := rows.Scan(&a.ID, &a.OriginGuild, &a.OriginHost, &a.OriginGuildName, &a.SenderID, &a.SenderName, &a.TargetID, &a.TargetName,
			&a.TargetAvatar, &a.Reason, &a.EvidencePath, &a.EvidenceName, &a.EvidenceType, &a.EvidenceSize, &a.EvidenceSHA, &a.EvidenceLink,
			&a.CreatedAt, &a.RetractedAt, &a.RetractSig, &a.Status, &ov, &a.Signed, &a.Sig,
			&hasD, &dAlert, &dGuild, &dCh, &dMsg, &dStatus, &dErr, &dPath, &dVia, &dIn, &dAt); err != nil {
			return nil, err
		}
		a.OriginVerified = ov == 1
		item := HistoryItem{Alert: a, Sent: a.OriginGuild == guild && a.OriginHost == self}
		if hasD {
			d = Delivery{AlertID: dAlert.String, GuildID: dGuild.String, ChannelID: dCh.String, MessageID: dMsg.String,
				Status: dStatus.String, Error: dErr.String, ViaHost: dVia.String, InServer: dIn.Int64 == 1, At: dAt.Int64}
			json.Unmarshal([]byte(dPath.String), &d.Path)
			item.Delivery = &d
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func prefixCols(p, cols string) string {
	parts := strings.Split(cols, ",")
	for i, c := range parts {
		parts[i] = p + strings.TrimSpace(c)
	}
	return strings.Join(parts, ", ")
}

// ---------------------------------------------------------------- outbox

// OutboxRow is one queued message for a peer.
type OutboxRow struct {
	Seq       int64
	Peer      string
	Type      string
	Payload   []byte
	CreatedAt int64
	Attempts  int
}

// Enqueue queues a message for a peer.
func (s *Store) Enqueue(ctx context.Context, peer, typ string, payload []byte, now int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO outbox(peer_host, type, payload, created_at) VALUES(?, ?, ?, ?)`, peer, typ, payload, now)
	return err
}

// PeersWithDueMessages lists peers that have undelivered messages ready to push.
func (s *Store) PeersWithDueMessages(ctx context.Context, now int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT peer_host FROM outbox WHERE delivered_at = 0 AND next_try <= ?`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// Pending returns undelivered messages for a peer after a sequence number,
// oldest first.
func (s *Store) Pending(ctx context.Context, peer string, after int64, limit int) ([]OutboxRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq, peer_host, type, payload, created_at, attempts FROM outbox
		WHERE peer_host = ? AND delivered_at = 0 AND seq > ? ORDER BY seq LIMIT ?`, peer, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutboxRow
	for rows.Next() {
		var r OutboxRow
		if err := rows.Scan(&r.Seq, &r.Peer, &r.Type, &r.Payload, &r.CreatedAt, &r.Attempts); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MarkDelivered closes out a message; lastErr records a permanent refusal.
func (s *Store) MarkDelivered(ctx context.Context, seq, now int64, lastErr string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE outbox SET delivered_at = ?, last_error = ? WHERE seq = ?`, now, lastErr, seq)
	return err
}

// AckThrough marks every message for a peer up to cursor as delivered. This
// is how a polling peer confirms what it has read.
func (s *Store) AckThrough(ctx context.Context, peer string, cursor, now int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE outbox SET delivered_at = ? WHERE peer_host = ? AND seq <= ? AND delivered_at = 0`, now, peer, cursor)
	return err
}

// RetryLater pushes a message's next attempt back.
func (s *Store) RetryLater(ctx context.Context, seq int64, attempts int, next int64, lastErr string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE outbox SET attempts = ?, next_try = ?, last_error = ? WHERE seq = ?`, attempts, next, lastErr, seq)
	return err
}

// DropOutboxFor removes everything queued for a peer.
func (s *Store) DropOutboxFor(ctx context.Context, peer string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM outbox WHERE peer_host = ? AND delivered_at = 0`, peer)
	return err
}

// OutboxBacklog counts undelivered messages per peer.
func (s *Store) OutboxBacklog(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT peer_host, COUNT(*) FROM outbox WHERE delivered_at = 0 GROUP BY peer_host`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var h string
		var n int
		if err := rows.Scan(&h, &n); err != nil {
			return nil, err
		}
		out[h] = n
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- retention

// ExpiredAlerts lists alerts created before cutoff.
func (s *Store) ExpiredAlerts(ctx context.Context, cutoff int64) ([]Alert, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+alertCols+` FROM alerts WHERE created_at < ?`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Alert
	for rows.Next() {
		a, err := scanAlert(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// PurgeOutbox removes delivered messages, and undelivered ones older than cutoff
// (any alert in them has expired by then).
func (s *Store) PurgeOutbox(ctx context.Context, cutoff int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM outbox WHERE delivered_at > 0 OR created_at < ?`, cutoff)
	return err
}
