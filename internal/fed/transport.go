package fed

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"syscall"
	"time"
)

// MaxBodyBytes caps any federation request. Evidence is capped at 25 MB by
// /instance limits, which is about 34 MB once base64-encoded.
const MaxBodyBytes = 40 << 20

// Paths on a reachable instance.
const (
	PathWellKnown = "/.well-known/gossipwire"
	PathInbox     = "/gossipwire/inbox"
	PathPoll      = "/gossipwire/poll"
)

// PermanentError means the peer refused a message and retrying will not help.
type PermanentError struct{ Msg string }

func (e *PermanentError) Error() string { return e.Msg }

// Client talks to other instances. HTTP is for addresses the operator
// chose (peer add, pinned peers). Untrusted is for addresses that arrive in
// a message (an alert's origin, a sender knocking in open mode): it refuses
// loopback, private, link-local and CGNAT addresses, so a peer cannot use
// this instance to probe the network it sits on.
type Client struct {
	HTTP      *http.Client
	Untrusted *http.Client
	Scheme    string // "https"; tests may point at a TLS test server
}

// NewClient returns a client with sane timeouts and no redirects.
func NewClient() *Client {
	guarded := &net.Dialer{Timeout: 10 * time.Second, Control: refusePrivate}
	return &Client{
		// Each call sets its own deadline through its context; this is only a backstop.
		HTTP: &http.Client{Timeout: 15 * time.Minute, CheckRedirect: noRedirects},
		Untrusted: &http.Client{
			Timeout:       15 * time.Second,
			CheckRedirect: noRedirects,
			Transport: &http.Transport{
				Proxy:               nil,
				DialContext:         guarded.DialContext,
				TLSHandshakeTimeout: 10 * time.Second,
				MaxIdleConns:        10,
				IdleConnTimeout:     30 * time.Second,
			},
		},
		Scheme: "https",
	}
}

// Instances answer at fixed paths. A redirect is either a misconfigured
// proxy or someone steering requests elsewhere, so none are followed.
func noRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// refusePrivate runs after DNS resolution, so a hostname that resolves to an
// internal address is caught too.
func refusePrivate(network, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("refusing to dial %s", address)
	}
	if !PublicAddr(ap.Addr()) {
		return fmt.Errorf("refusing to dial non-public address %s", ap.Addr())
	}
	return nil
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// PublicAddr reports whether ip is a normal internet address.
func PublicAddr(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsValid() && !ip.IsLoopback() && !ip.IsPrivate() && !ip.IsLinkLocalUnicast() &&
		!ip.IsLinkLocalMulticast() && !ip.IsMulticast() && !ip.IsUnspecified() && !ip.IsInterfaceLocalMulticast() &&
		!cgnat.Contains(ip)
}

func (c *Client) url(host, path string) string { return c.Scheme + "://" + host + path }

// FetchWellKnown reads the published identity of an address the operator
// chose or already trusts.
func (c *Client) FetchWellKnown(ctx context.Context, host string) (*WellKnown, ed25519.PublicKey, error) {
	return c.fetchWellKnown(ctx, c.HTTP, host)
}

// FetchWellKnownUntrusted is FetchWellKnown for an address that came from a
// message. It will not connect to internal addresses.
func (c *Client) FetchWellKnownUntrusted(ctx context.Context, host string) (*WellKnown, ed25519.PublicKey, error) {
	return c.fetchWellKnown(ctx, c.Untrusted, host)
}

func (c *Client) fetchWellKnown(ctx context.Context, hc *http.Client, host string) (*WellKnown, ed25519.PublicKey, error) {
	if !ValidHost(host) {
		return nil, nil, fmt.Errorf("%q is not an instance address", host)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.url(host, PathWellKnown), nil)
	resp, err := hc.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("could not reach %s: %w", host, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("%s answered %d at %s; is it a GossipWire instance?", host, resp.StatusCode, PathWellKnown)
	}
	var wk WellKnown
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&wk); err != nil {
		return nil, nil, fmt.Errorf("%s did not return GossipWire identity data", host)
	}
	if wk.Name != host {
		return nil, nil, fmt.Errorf("%s says its name is %q; its INSTANCE_URL must match the address you used", host, wk.Name)
	}
	if wk.Protocol != ProtocolVersion {
		return nil, nil, fmt.Errorf("%s speaks protocol %d, this instance speaks %d", host, wk.Protocol, ProtocolVersion)
	}
	pub, err := DecodeKey(wk.PublicKey)
	if err != nil {
		return nil, nil, fmt.Errorf("%s published a bad key", host)
	}
	return &wk, pub, nil
}

// Push delivers one sealed envelope to a reachable peer's inbox.
func (c *Client) Push(ctx context.Context, env *Envelope) error {
	b, _ := json.Marshal(env)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.url(env.To, PathInbox), bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestTimeout:
		return fmt.Errorf("%s: %d %s", env.To, resp.StatusCode, bytes.TrimSpace(msg))
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return &PermanentError{Msg: fmt.Sprintf("%s refused the message: %d %s", env.To, resp.StatusCode, bytes.TrimSpace(msg))}
	default:
		return fmt.Errorf("%s: %d %s", env.To, resp.StatusCode, bytes.TrimSpace(msg))
	}
}

// Poll asks a reachable peer for messages queued for this instance.
func (c *Client) Poll(ctx context.Context, env *Envelope) (*PollResponse, error) {
	b, _ := json.Marshal(env)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.url(env.To, PathPoll), bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("%s: %d %s", env.To, resp.StatusCode, bytes.TrimSpace(msg))
	}
	var pr PollResponse
	// A poll answer carries at most about 30 MB of payload, plus base64 and
	// envelope overhead. Anything past this cap is a misbehaving peer.
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2*MaxBodyBytes)).Decode(&pr); err != nil {
		return nil, err
	}
	return &pr, nil
}

// Handler is what the server calls into. Both methods get an envelope whose
// signature, target and timestamp already checked out.
type Handler interface {
	// Authenticate decides whether the sender may talk to this instance at
	// all (mode, allowlist, pinned key). It returns a reason on refusal.
	Authenticate(ctx context.Context, env *Envelope, pub ed25519.PublicKey) error
	// HandleMessage processes one inbox message.
	HandleMessage(ctx context.Context, env *Envelope) error
	// PollFor returns queued messages for peer after cursor.
	PollFor(ctx context.Context, peer string, after int64) (*PollResponse, error)
}

// Server is the federation HTTP endpoint.
type Server struct {
	ID      *Identity
	Handler Handler
	Log     *slog.Logger
	Now     func() time.Time

	mu   sync.Mutex
	seen map[string]time.Time // envelope IDs, to drop replays inside the 5 minute window
	sem  chan struct{}        // bounds how many bodies (up to 40 MB each) are in memory at once
}

// MaxConcurrent is how many federation requests are read at once. Senders
// retry on 503, so a burst only slows down.
const MaxConcurrent = 8

// Routes returns the HTTP handler.
func (s *Server) Routes() http.Handler {
	s.sem = make(chan struct{}, MaxConcurrent)
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+PathWellKnown, s.wellKnown)
	mux.HandleFunc("POST "+PathInbox, s.limit(s.inbox))
	mux.HandleFunc("POST "+PathPoll, s.limit(s.poll))
	return mux
}

func (s *Server) limit(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		select {
		case s.sem <- struct{}{}:
			defer func() { <-s.sem }()
			h(w, r)
		default:
			http.Error(w, "busy, retry later", http.StatusServiceUnavailable)
		}
	}
}

// ErrRejected wraps a refusal the peer should not retry.
var ErrRejected = errors.New("rejected")

func (s *Server) wellKnown(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(WellKnown{Software: "gossipwire", Name: s.ID.ID, PublicKey: s.ID.PubB64(), Protocol: ProtocolVersion})
}

func (s *Server) readEnvelope(w http.ResponseWriter, r *http.Request) (*Envelope, ed25519.PublicKey, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		http.Error(w, "message too large", http.StatusRequestEntityTooLarge)
		return nil, nil, false
	}
	var env Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		http.Error(w, "not a GossipWire envelope", http.StatusBadRequest)
		return nil, nil, false
	}
	pub, err := VerifyEnvelope(&env, s.ID.ID, s.Now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return nil, nil, false
	}
	if !s.markSeen(env.ID) {
		// Already processed this exact message. Say yes so the sender stops.
		w.WriteHeader(http.StatusAccepted)
		return nil, nil, false
	}
	if err := s.Handler.Authenticate(r.Context(), &env, pub); err != nil {
		s.forget(env.ID)
		s.Log.Info("refused message from peer", "from", env.From, "type", env.Type, "reason", err)
		http.Error(w, err.Error(), http.StatusForbidden)
		return nil, nil, false
	}
	return &env, pub, true
}

func (s *Server) inbox(w http.ResponseWriter, r *http.Request) {
	env, _, ok := s.readEnvelope(w, r)
	if !ok {
		return
	}
	if !KnownType(env.Type) {
		http.Error(w, "unknown message type "+env.Type, http.StatusBadRequest)
		return
	}
	if err := s.Handler.HandleMessage(r.Context(), env); err != nil {
		if errors.Is(err, ErrRejected) {
			s.Log.Info("rejected message", "from", env.From, "type", env.Type, "reason", err)
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		s.forget(env.ID)
		s.Log.Warn("failed to process message", "from", env.From, "type", env.Type, "err", err)
		http.Error(w, "temporary failure, retry later", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) poll(w http.ResponseWriter, r *http.Request) {
	env, _, ok := s.readEnvelope(w, r)
	if !ok {
		return
	}
	if env.Type != TypePoll {
		http.Error(w, "expected a poll message", http.StatusBadRequest)
		return
	}
	var pr PollRequest
	if err := json.Unmarshal(env.Body, &pr); err != nil {
		http.Error(w, "bad poll body", http.StatusBadRequest)
		return
	}
	resp, err := s.Handler.PollFor(r.Context(), env.From, pr.After)
	if err != nil {
		http.Error(w, "temporary failure", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) markSeen(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.Now()
	if s.seen == nil {
		s.seen = map[string]time.Time{}
	}
	if len(s.seen) > 10000 {
		for k, t := range s.seen {
			if now.Sub(t) > 2*MaxClockSkew {
				delete(s.seen, k)
			}
		}
	}
	if _, dup := s.seen[id]; dup {
		return false
	}
	s.seen[id] = now
	return true
}

func (s *Server) forget(id string) {
	s.mu.Lock()
	delete(s.seen, id)
	s.mu.Unlock()
}
