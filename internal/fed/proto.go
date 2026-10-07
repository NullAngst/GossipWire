// Package fed holds the federation wire format: instance identity, signed
// envelopes, the origin-signed alert, hop records and retractions. It has no
// knowledge of Discord or the database.
package fed

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ProtocolVersion is published at /.well-known/gossipwire.
const ProtocolVersion = 1

// MaxClockSkew is how old (or how far in the future) an envelope may be.
const MaxClockSkew = 5 * time.Minute

// AlertLifetime is how long an alert lives, counted from the origin's created_at.
const AlertLifetime = 14 * 24 * time.Hour

// Message types.
const (
	TypeSubscribeRequest = "subscribe.request"
	TypeSubscribeAccept  = "subscribe.accept"
	TypeSubscribeDeny    = "subscribe.deny"
	TypeUnsubscribe      = "unsubscribe"
	TypeAlert            = "alert"
	TypeAlertForward     = "alert.forward"
	TypeAlertRetract     = "alert.retract"
	TypePoll             = "poll" // outbound-only peers asking for their queued messages
)

// KnownType reports whether t is a message type peers may send to an inbox.
func KnownType(t string) bool {
	switch t {
	case TypeSubscribeRequest, TypeSubscribeAccept, TypeSubscribeDeny, TypeUnsubscribe,
		TypeAlert, TypeAlertForward, TypeAlertRetract:
		return true
	}
	return false
}

// ---------------------------------------------------------------- identity

// Identity is this instance's signing key and the name other instances know
// it by.
type Identity struct {
	Priv ed25519.PrivateKey
	Pub  ed25519.PublicKey
	ID   string // hostname for reachable instances, key-<hex> for outbound-only
}

// KeyIDPrefix marks an outbound-only instance, named after its own key.
const KeyIDPrefix = "key-"

// KeyID returns the self-certifying name for a public key: key- plus the
// first 128 bits of its SHA-256, in hex.
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return KeyIDPrefix + hex.EncodeToString(sum[:16])
}

// Fingerprint is the human-readable form of a key, shown in /instance peers
// and in key-change DMs.
func Fingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	h := hex.EncodeToString(sum[:16])
	var parts []string
	for i := 0; i < len(h); i += 4 {
		parts = append(parts, h[i:i+4])
	}
	return strings.Join(parts, " ")
}

// IsKeyID reports whether host is a self-certifying key- name.
func IsKeyID(host string) bool {
	return keyIDRe.MatchString(host)
}

var keyIDRe = regexp.MustCompile(`^key-[0-9a-f]{32}$`)

// KeyMatchesID reports whether pub is the key a key- name was made from.
func KeyMatchesID(pub ed25519.PublicKey, host string) bool {
	return IsKeyID(host) && KeyID(pub) == host
}

var guildRe = regexp.MustCompile(`^[0-9]{17,20}$`)

var hostRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*(:[0-9]{1,5})?$`)

// ValidHost reports whether s is already a normalized instance address.
// Anything that arrives from a peer and names an instance goes through this
// before it is stored, fetched or shown.
func ValidHost(s string) bool {
	h, err := NormalizeHost(s)
	return err == nil && h == s && len(s) <= 253
}

// NormalizeHost lowercases a peer address and strips a scheme or trailing
// slash someone pasted. It returns an error a layman can act on.
func NormalizeHost(s string) (string, error) {
	h := strings.ToLower(strings.TrimSpace(s))
	h = strings.TrimPrefix(h, "https://")
	h = strings.TrimPrefix(h, "http://")
	h = strings.TrimRight(h, "/")
	if IsKeyID(h) {
		return h, nil
	}
	if h == "" || strings.ContainsAny(h, "/?#@ ") || !hostRe.MatchString(h) {
		return "", fmt.Errorf("%q is not an instance address. Use a host like alerts.example.org, or a key- address like key-0123abcd... from the other operator's /instance status", s)
	}
	return h, nil
}

// LoadOrCreateIdentity reads the key from dir/identity.key, creating it on
// first boot. host is the INSTANCE_URL host, or empty for an outbound-only
// instance.
func LoadOrCreateIdentity(dir, host string) (*Identity, bool, error) {
	path := filepath.Join(dir, "identity.key")
	created := false
	b, err := os.ReadFile(path)
	var priv ed25519.PrivateKey
	switch {
	case err == nil:
		seed, derr := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
		if derr != nil || len(seed) != ed25519.SeedSize {
			return nil, false, fmt.Errorf("%s is damaged. Restore it from your backup of the data folder. Deleting it gives this instance a new identity, and every peer will see it as a stranger", path)
		}
		priv = ed25519.NewKeyFromSeed(seed)
	case errors.Is(err, os.ErrNotExist):
		_, p, gerr := ed25519.GenerateKey(rand.Reader)
		if gerr != nil {
			return nil, false, gerr
		}
		priv = p
		tmp := path + ".tmp"
		if werr := os.WriteFile(tmp, []byte(base64.StdEncoding.EncodeToString(priv.Seed())+"\n"), 0o600); werr != nil {
			return nil, false, werr
		}
		if rerr := os.Rename(tmp, path); rerr != nil {
			return nil, false, rerr
		}
		created = true
	default:
		return nil, false, err
	}
	pub := priv.Public().(ed25519.PublicKey)
	id := &Identity{Priv: priv, Pub: pub, ID: host}
	if id.ID == "" {
		id.ID = KeyID(pub)
	}
	return id, created, nil
}

// PubB64 is the public key in the form it travels on the wire.
func (id *Identity) PubB64() string { return EncodeKey(id.Pub) }

// EncodeKey is the wire form of a public key.
func EncodeKey(pub ed25519.PublicKey) string { return base64.StdEncoding.EncodeToString(pub) }

// DecodeKey parses a base64 public key.
func DecodeKey(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("bad public key")
	}
	return ed25519.PublicKey(b), nil
}

// WellKnown is served at /.well-known/gossipwire.
type WellKnown struct {
	Software  string `json:"software"`
	Name      string `json:"name"`
	PublicKey string `json:"public_key"`
	Protocol  int    `json:"protocol"`
}

// ---------------------------------------------------------------- envelope

// Envelope wraps every message between instances. The signature covers the
// sender, target, time, message ID, type and the exact body bytes.
type Envelope struct {
	V    int             `json:"v"`
	ID   string          `json:"id"`
	From string          `json:"from"`
	To   string          `json:"to"`
	Key  string          `json:"key"`
	TS   int64           `json:"ts"`
	Type string          `json:"type"`
	Body json.RawMessage `json:"body"`
	Sig  string          `json:"sig"`
}

func envelopeSigningBytes(e *Envelope) []byte {
	sum := sha256.Sum256(e.Body)
	return []byte("gossipwire-envelope/v1\n" + e.From + "\n" + e.To + "\n" +
		strconv.FormatInt(e.TS, 10) + "\n" + e.ID + "\n" + e.Type + "\n" + hex.EncodeToString(sum[:]))
}

// Seal builds and signs an envelope to host to.
func (id *Identity) Seal(to, typ string, body []byte, now time.Time) *Envelope {
	e := &Envelope{V: 1, ID: uuid.NewString(), From: id.ID, To: to, Key: id.PubB64(), TS: now.Unix(), Type: typ, Body: body}
	e.Sig = base64.StdEncoding.EncodeToString(ed25519.Sign(id.Priv, envelopeSigningBytes(e)))
	return e
}

// VerifyEnvelope checks the signature with the key carried in the envelope,
// the target and the clock. It does not decide whether that key is the right
// one for From; the caller checks that against its pinned peers.
func VerifyEnvelope(e *Envelope, self string, now time.Time) (ed25519.PublicKey, error) {
	if e.V != 1 {
		return nil, fmt.Errorf("unsupported envelope version %d", e.V)
	}
	if e.To != self {
		return nil, fmt.Errorf("envelope is addressed to %q, not %q", e.To, self)
	}
	if !ValidHost(e.From) {
		return nil, errors.New("envelope sender is not a valid instance address")
	}
	ts := time.Unix(e.TS, 0)
	if now.Sub(ts) > MaxClockSkew || ts.Sub(now) > MaxClockSkew {
		return nil, errors.New("envelope is older than 5 minutes or from the future; check the clocks on both machines")
	}
	if !CanonicalUUID(e.ID) {
		return nil, errors.New("envelope has a bad message ID")
	}
	pub, err := DecodeKey(e.Key)
	if err != nil {
		return nil, err
	}
	sig, err := base64.StdEncoding.DecodeString(e.Sig)
	if err != nil || !ed25519.Verify(pub, envelopeSigningBytes(e), sig) {
		return nil, errors.New("envelope signature does not verify")
	}
	if IsKeyID(e.From) && !KeyMatchesID(pub, e.From) {
		return nil, errors.New("sender key does not match its key- address")
	}
	return pub, nil
}

// ---------------------------------------------------------------- alerts

// EvidenceMeta describes the attachment. The bytes travel next to the signed
// alert and are checked against SHA256.
type EvidenceMeta struct {
	Name        string `json:"name"`
	ContentType string `json:"content_type,omitempty"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
}

// Alert is the claim itself, signed once by the origin instance and carried
// unchanged through every forward.
type Alert struct {
	AlertID         string        `json:"alert_id"`
	OriginGuild     string        `json:"origin_guild"`
	OriginGuildName string        `json:"origin_guild_name"`
	OriginHost      string        `json:"origin_host"`
	OriginKey       string        `json:"origin_key"`
	SenderID        string        `json:"sender_id"`
	SenderName      string        `json:"sender_name"`
	TargetID        string        `json:"target_id"`
	TargetName      string        `json:"target_name"`
	TargetAvatar    string        `json:"target_avatar,omitempty"`
	Reason          string        `json:"reason"`
	Link            string        `json:"link,omitempty"`
	Evidence        *EvidenceMeta `json:"evidence,omitempty"`
	CreatedAt       int64         `json:"created_at"`
}

// SignedAlert is the origin's alert bytes plus its signature.
type SignedAlert struct {
	Alert json.RawMessage `json:"alert"`
	Sig   string          `json:"sig"`
}

func alertSigningBytes(raw []byte) []byte {
	return append([]byte("gossipwire-alert/v1\n"), raw...)
}

// SignAlert serializes and signs an alert.
func (id *Identity) SignAlert(a *Alert) (*SignedAlert, error) {
	raw, err := json.Marshal(a)
	if err != nil {
		return nil, err
	}
	return &SignedAlert{Alert: raw, Sig: base64.StdEncoding.EncodeToString(ed25519.Sign(id.Priv, alertSigningBytes(raw)))}, nil
}

// Parse decodes the alert without verifying it.
func (s *SignedAlert) Parse() (*Alert, error) {
	var a Alert
	if err := json.Unmarshal(s.Alert, &a); err != nil {
		return nil, err
	}
	if !CanonicalUUID(a.AlertID) {
		return nil, errors.New("alert has a bad ID")
	}
	if !ValidHost(a.OriginHost) {
		return nil, errors.New("alert has a bad origin address")
	}
	return &a, nil
}

// CanonicalUUID accepts only the lowercase 36-character form. uuid.Parse
// also takes braces, urn: prefixes and uppercase, which would let one alert
// arrive under several IDs and slip past dedup.
func CanonicalUUID(s string) bool {
	u, err := uuid.Parse(s)
	return err == nil && u.String() == s
}

// Verify checks the origin signature with pub.
func (s *SignedAlert) Verify(pub ed25519.PublicKey) bool {
	sig, err := base64.StdEncoding.DecodeString(s.Sig)
	return err == nil && ed25519.Verify(pub, alertSigningBytes(s.Alert), sig)
}

// Expired reports whether an alert created at createdAt is past its lifetime.
func Expired(createdAt int64, now time.Time) bool {
	return now.Sub(time.Unix(createdAt, 0)) > AlertLifetime
}

// Hop is one forward: which server passed the alert on, and who pressed the
// button. Hop 1 is the first forward; the origin is hop 0 and is the alert
// itself. Each forwarding instance signs its own hop.
type Hop struct {
	AlertID   string `json:"alert_id"`
	N         int    `json:"n"`
	GuildID   string `json:"guild_id"`
	GuildName string `json:"guild_name"`
	Host      string `json:"host"`
	ByID      string `json:"by_id"`
	ByName    string `json:"by_name"`
	At        int64  `json:"at"`
	Sig       string `json:"sig"`
}

// Key is how a hop is referred to in a delivery path.
func (h *Hop) Key() string { return h.GuildID + "@" + h.Host }

func hopSigningBytes(h *Hop) []byte {
	return []byte(fmt.Sprintf("gossipwire-hop/v1\n%s\n%d\n%s\n%s\n%s\n%s\n%s\n%d",
		h.AlertID, h.N, h.GuildID, h.GuildName, h.Host, h.ByID, h.ByName, h.At))
}

// SignHop fills in the hop's signature.
func (id *Identity) SignHop(h *Hop) {
	h.Sig = base64.StdEncoding.EncodeToString(ed25519.Sign(id.Priv, hopSigningBytes(h)))
}

// VerifyHop checks a hop's signature with pub.
func VerifyHop(h *Hop, pub ed25519.PublicKey) bool {
	sig, err := base64.StdEncoding.DecodeString(h.Sig)
	return err == nil && ed25519.Verify(pub, hopSigningBytes(h), sig)
}

// Retraction is signed by the origin and relayed along the paths the alert took.
type Retraction struct {
	AlertID     string `json:"alert_id"`
	OriginHost  string `json:"origin_host"`
	OriginKey   string `json:"origin_key"`
	RetractedAt int64  `json:"retracted_at"`
	Sig         string `json:"sig"`
}

func retractSigningBytes(r *Retraction) []byte {
	return []byte(fmt.Sprintf("gossipwire-retract/v1\n%s\n%s\n%d", r.AlertID, r.OriginHost, r.RetractedAt))
}

// SignRetraction fills in the retraction's signature.
func (id *Identity) SignRetraction(r *Retraction) {
	r.Sig = base64.StdEncoding.EncodeToString(ed25519.Sign(id.Priv, retractSigningBytes(r)))
}

// VerifyRetraction checks a retraction's signature with pub.
func VerifyRetraction(r *Retraction, pub ed25519.PublicKey) bool {
	sig, err := base64.StdEncoding.DecodeString(r.Sig)
	return err == nil && ed25519.Verify(pub, retractSigningBytes(r), sig)
}

// ---------------------------------------------------------------- bodies

// Caps on what one alert message may carry. Real chains and target lists are
// far smaller; these only bound the work a hostile peer can cause.
const (
	MaxHops    = 100
	MaxTargets = 500
)

// AlertMessage is the body of alert and alert.forward.
type AlertMessage struct {
	Alert    SignedAlert `json:"alert"`
	Hops     []Hop       `json:"hops,omitempty"`
	Targets  []string    `json:"targets"`
	Evidence []byte      `json:"evidence,omitempty"` // base64 in JSON
}

// SubscribeRequest asks a publisher server to send its alerts to a subscriber.
type SubscribeRequest struct {
	PublisherGuild      string `json:"publisher_guild"`
	SubscriberGuild     string `json:"subscriber_guild"`
	SubscriberGuildName string `json:"subscriber_guild_name"`
	ByName              string `json:"by_name"`
}

// SubscribeReply is the body of subscribe.accept and subscribe.deny.
type SubscribeReply struct {
	PublisherGuild     string `json:"publisher_guild"`
	PublisherGuildName string `json:"publisher_guild_name"`
	SubscriberGuild    string `json:"subscriber_guild"`
	ByName             string `json:"by_name"`
	Reason             string `json:"reason,omitempty"`
}

// Unsubscribe ends a subscription from either side.
type Unsubscribe struct {
	PublisherGuild  string `json:"publisher_guild"`
	SubscriberGuild string `json:"subscriber_guild"`
	Side            string `json:"side"` // "subscriber" or "publisher"
}

// PollRequest is the body of a poll envelope.
type PollRequest struct {
	After int64 `json:"after"`
}

// Polled is one queued message, sealed fresh at poll time so it passes the
// 5 minute check however long it waited.
type Polled struct {
	Seq      int64     `json:"seq"`
	Envelope *Envelope `json:"envelope"`
}

// PollResponse carries queued messages in order. The poller sends back the
// last Seq it processed as After on its next poll, which acknowledges them.
type PollResponse struct {
	Messages []Polled `json:"messages"`
}

// ParseAddress splits "guild" or "guild@host". host is empty for a bare ID.
func ParseAddress(s string) (guild, host string, err error) {
	s = strings.TrimSpace(s)
	guild, host, _ = strings.Cut(s, "@")
	if !guildRe.MatchString(guild) {
		return "", "", fmt.Errorf("%q does not start with a server ID. Server IDs are long numbers: right-click the server icon with Developer Mode on and pick Copy Server ID", s)
	}
	if host != "" {
		h, herr := NormalizeHost(host)
		if herr != nil {
			return "", "", herr
		}
		host = h
	}
	return guild, host, nil
}
