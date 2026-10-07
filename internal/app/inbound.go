package app

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/NullAngst/GossipWire/internal/config"
	"github.com/NullAngst/GossipWire/internal/fed"
	"github.com/NullAngst/GossipWire/internal/store"
)

func rejected(format string, args ...any) error {
	return fmt.Errorf("%w: %s", fed.ErrRejected, fmt.Sprintf(format, args...))
}

// Authenticate decides whether a peer may talk to this instance, pins keys
// on first contact and blocks a peer whose key changed.
func (a *App) Authenticate(ctx context.Context, env *fed.Envelope, pub ed25519.PublicKey) error {
	if a.Cfg.FederationMode == config.ModeOff {
		return errors.New("this instance does not federate")
	}
	if env.From == a.Self() {
		return errors.New("message claims to come from this instance")
	}
	now := a.Now().Unix()
	keyB64 := env.Key
	inst, err := a.St.GetInstance(ctx, env.From)
	if err != nil {
		return err
	}
	if inst == nil {
		if a.Cfg.FederationMode != config.ModeOpen {
			return fmt.Errorf("%s is not on this instance's allowlist; its operator needs to run /instance peer add %s", env.From, env.From)
		}
		if !a.allowNewPeer() {
			return errors.New("too many new instances are connecting right now; try again later")
		}
		reach := store.ReachOutbound
		if !fed.IsKeyID(env.From) {
			// A hostname has to prove it is that host.
			_, wkPub, err := a.Fed.FetchWellKnownUntrusted(ctx, env.From)
			if err != nil {
				return fmt.Errorf("could not confirm the sender's key: %v", err)
			}
			if !wkPub.Equal(pub) {
				return errors.New("sender key does not match the key published at its address")
			}
			reach = store.ReachReachable
		}
		a.Log.Info("new peer instance (open mode)", "host", env.From, "fingerprint", fed.Fingerprint(pub))
		return a.St.UpsertInstance(ctx, store.Instance{Host: env.From, PublicKey: keyB64, Reach: reach, Status: store.PeerAllowed, FirstSeen: now, LastSeen: now})
	}
	switch inst.Status {
	case store.PeerBlocked:
		return errors.New("this instance has blocked you")
	case store.PeerKeyChanged:
		if keyB64 != inst.PendingKey && keyB64 != inst.PublicKey && a.publishedKeyIs(ctx, env.From, pub) {
			inst.PendingKey = keyB64
			a.St.UpsertInstance(ctx, *inst)
			a.dmKeyChange(ctx, inst, pub)
		}
		return errors.New("your key changed; this instance's operator has to approve the new key")
	}
	if inst.PublicKey == "" {
		// Added by the operator before first contact. key- addresses are
		// self-certifying (already checked); a hostname is checked against
		// what it publishes.
		if !fed.IsKeyID(env.From) {
			_, wkPub, err := a.Fed.FetchWellKnown(ctx, env.From)
			if err != nil || !wkPub.Equal(pub) {
				return errors.New("could not confirm the sender's key at its address")
			}
		}
		inst.PublicKey = keyB64
		inst.LastSeen = now
		a.Log.Info("pinned peer key", "host", env.From, "fingerprint", fed.Fingerprint(pub))
		return a.St.UpsertInstance(ctx, *inst)
	}
	if inst.PublicKey != keyB64 {
		// Anyone can sign an envelope that claims to be from any hostname.
		// Only treat it as a key change if the host itself now publishes
		// this key; otherwise it is a forgery and the pinned peer is fine.
		// key- addresses cannot get here: their key is their name.
		if fed.IsKeyID(env.From) || !a.publishedKeyIs(ctx, env.From, pub) {
			return errors.New("sender key does not match the key pinned for this address")
		}
		inst.Status = store.PeerKeyChanged
		inst.PendingKey = keyB64
		if err := a.St.UpsertInstance(ctx, *inst); err != nil {
			return err
		}
		a.Log.Warn("peer key changed; peer blocked until the operator approves", "host", env.From, "new_fingerprint", fed.Fingerprint(pub))
		a.dmKeyChange(ctx, inst, pub)
		return errors.New("your key changed; this instance's operator has to approve the new key")
	}
	a.St.TouchInstance(ctx, env.From, now)
	return nil
}

// publishedKeyIs reports whether host currently publishes pub at its
// well-known address. The host is a pinned peer the operator added (or that
// already passed an open-mode check), so the trusted client is used.
//
// The answer is cached for a minute, so a stream of forged envelopes costs
// the real peer one request a minute, not one each.
func (a *App) publishedKeyIs(ctx context.Context, host string, pub ed25519.PublicKey) bool {
	memoKey := "pinned:" + host
	a.keysMu.Lock()
	m, ok := a.keyMemo[memoKey]
	a.keysMu.Unlock()
	if !ok || a.Now().Sub(m.at) > time.Minute {
		fctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, wk, err := a.Fed.FetchWellKnown(fctx, host)
		cancel()
		if err != nil {
			wk = nil
		}
		m = keyMemo{pub: wk, at: a.Now()}
		a.keysMu.Lock()
		if len(a.keyMemo) >= 1000 {
			a.keyMemo = map[string]keyMemo{}
		}
		a.keyMemo[memoKey] = m
		a.keysMu.Unlock()
	}
	return m.pub != nil && m.pub.Equal(pub)
}

// allowNewPeer limits how many unknown instances open mode admits per hour.
// key- identities cost nothing to make, so without this a flood of them
// would fill the instances table.
func (a *App) allowNewPeer() bool {
	a.backoffMu.Lock()
	defer a.backoffMu.Unlock()
	now := a.Now()
	if now.Sub(a.newPeerWindow) > time.Hour {
		a.newPeerWindow, a.newPeerCount = now, 0
	}
	if a.newPeerCount >= maxNewPeersPerHour {
		return false
	}
	a.newPeerCount++
	return true
}

const maxNewPeersPerHour = 20

func (a *App) dmKeyChange(ctx context.Context, inst *store.Instance, newPub ed25519.PublicKey) {
	oldFP := "unknown"
	if k, err := fed.DecodeKey(inst.PublicKey); err == nil {
		oldFP = fed.Fingerprint(k)
	}
	err := a.DC.DM(ctx, a.Cfg.OwnerID, plain(fmt.Sprintf(
		"GossipWire: peer `%s` showed up with a different key, so I blocked it.\nOld key: `%s`\nNew key: `%s`\n"+
			"A changed key is how a hijacked domain shows up. Ask that instance's operator whether they reset their data folder, "+
			"then run `/instance peers` to approve the new key or keep it blocked.", inst.Host, oldFP, fed.Fingerprint(newPub))))
	if err != nil {
		a.Log.Warn("could not DM the owner about a key change", "err", err)
	}
}

// HandleMessage processes one authenticated message from a peer.
func (a *App) HandleMessage(ctx context.Context, env *fed.Envelope) error {
	switch env.Type {
	case fed.TypeSubscribeRequest:
		var m fed.SubscribeRequest
		if err := json.Unmarshal(env.Body, &m); err != nil {
			return rejected("bad body")
		}
		return a.inSubscribeRequest(ctx, env.From, &m)
	case fed.TypeSubscribeAccept, fed.TypeSubscribeDeny:
		var m fed.SubscribeReply
		if err := json.Unmarshal(env.Body, &m); err != nil {
			return rejected("bad body")
		}
		return a.inSubscribeReply(ctx, env.From, env.Type == fed.TypeSubscribeAccept, &m)
	case fed.TypeUnsubscribe:
		var m fed.Unsubscribe
		if err := json.Unmarshal(env.Body, &m); err != nil {
			return rejected("bad body")
		}
		return a.inUnsubscribe(ctx, env.From, &m)
	case fed.TypeAlert, fed.TypeAlertForward:
		var m fed.AlertMessage
		if err := json.Unmarshal(env.Body, &m); err != nil {
			return rejected("bad body")
		}
		return a.inAlert(ctx, env, &m)
	case fed.TypeAlertRetract:
		var m fed.Retraction
		if err := json.Unmarshal(env.Body, &m); err != nil {
			return rejected("bad body")
		}
		return a.inRetract(ctx, env, &m)
	}
	return rejected("unknown message type %s", env.Type)
}

func (a *App) inSubscribeRequest(ctx context.Context, from string, m *fed.SubscribeRequest) error {
	if !config.IsSnowflake(m.PublisherGuild) || !config.IsSnowflake(m.SubscriberGuild) {
		return rejected("bad server IDs")
	}
	g, err := a.St.GetGuild(ctx, m.PublisherGuild)
	if err != nil {
		return err
	}
	if g == nil {
		return a.enqueue(ctx, from, fed.TypeSubscribeDeny, &fed.SubscribeReply{PublisherGuild: m.PublisherGuild,
			SubscriberGuild: m.SubscriberGuild, Reason: "That server is not using GossipWire on this instance, or has not run /setup."})
	}
	existing, err := a.St.GetSub(ctx, m.PublisherGuild, a.Self(), m.SubscriberGuild, from)
	if err != nil {
		return err
	}
	if existing != nil && existing.Status == store.SubActive {
		// They lost track; tell them again.
		return a.enqueue(ctx, from, fed.TypeSubscribeAccept, &fed.SubscribeReply{PublisherGuild: m.PublisherGuild,
			PublisherGuildName: cleanName(a.guildName(m.PublisherGuild, existing.PubName)), SubscriberGuild: m.SubscriberGuild})
	}
	if existing != nil && existing.Status == store.SubPending {
		return nil
	}
	if a.St.CountPendingFor(ctx, m.PublisherGuild, a.Self()) >= maxPendingRequests {
		return a.enqueue(ctx, from, fed.TypeSubscribeDeny, &fed.SubscribeReply{PublisherGuild: m.PublisherGuild,
			SubscriberGuild: m.SubscriberGuild, Reason: "That server has too many unanswered requests. Ask its mods to clear them, then try again."})
	}
	sub := store.Subscription{PubGuild: m.PublisherGuild, PubHost: a.Self(), PubName: cleanName(a.guildName(m.PublisherGuild, "")),
		SubGuild: m.SubscriberGuild, SubHost: from, SubName: cleanName(m.SubscriberGuildName), Status: store.SubPending,
		RequestedBy: cleanName(m.ByName), CreatedAt: a.Now().Unix()}
	id, err := a.St.PutSub(ctx, sub)
	if err != nil {
		return err
	}
	sub.ID = id
	if err := a.postRequestCard(ctx, &sub, g.ChannelID, m.ByName); err != nil {
		a.Log.Warn("could not post a remote subscribe request", "guild", m.PublisherGuild, "err", err)
	}
	return nil
}

func (a *App) inSubscribeReply(ctx context.Context, from string, accepted bool, m *fed.SubscribeReply) error {
	sub, err := a.St.GetSub(ctx, m.PublisherGuild, from, m.SubscriberGuild, a.Self())
	if err != nil {
		return err
	}
	if sub == nil {
		return nil // cancelled on our side meanwhile
	}
	name := orID(cleanName(m.PublisherGuildName), m.PublisherGuild)
	if !accepted {
		if err := a.St.DeleteSub(ctx, sub.ID); err != nil {
			return err
		}
		text := fmt.Sprintf("%s (`%s@%s`) denied your request to receive their alerts.", md(name), m.PublisherGuild, from)
		if m.Reason != "" {
			text = fmt.Sprintf("Your request to follow `%s@%s` did not go through: %s", m.PublisherGuild, from, md(truncate(m.Reason, 300)))
		}
		a.notifyGuild(ctx, m.SubscriberGuild, text)
		return nil
	}
	wasActive := sub.Status == store.SubActive
	sub.Status = store.SubActive
	sub.PubName = name
	sub.ApprovedBy = cleanName(m.ByName)
	sub.ApprovedAt = a.Now().Unix()
	if err := a.St.UpdateSub(ctx, *sub); err != nil {
		return err
	}
	if !wasActive {
		a.notifyGuild(ctx, m.SubscriberGuild, fmt.Sprintf("%s (`%s@%s`) approved your request. Their alerts will arrive here.", md(name), m.PublisherGuild, from))
	}
	return nil
}

func (a *App) inUnsubscribe(ctx context.Context, from string, m *fed.Unsubscribe) error {
	var sub *store.Subscription
	var err error
	switch m.Side {
	case "subscriber":
		sub, err = a.St.GetSub(ctx, m.PublisherGuild, a.Self(), m.SubscriberGuild, from)
	case "publisher":
		sub, err = a.St.GetSub(ctx, m.PublisherGuild, from, m.SubscriberGuild, a.Self())
	default:
		return rejected("bad side")
	}
	if err != nil || sub == nil {
		return err
	}
	if err := a.St.DeleteSub(ctx, sub.ID); err != nil {
		return err
	}
	if m.Side == "publisher" && sub.Status == store.SubActive {
		a.notifyGuild(ctx, m.SubscriberGuild, fmt.Sprintf("%s (`%s@%s`) stopped sending its alerts to this server.", md(orID(sub.PubName, sub.PubGuild)), sub.PubGuild, from))
	}
	if sub.Status == store.SubPending {
		a.closeRequestCard(ctx, sub, "Cancelled by the requesting server.")
	}
	return nil
}

// originKey finds the key to check an alert's origin signature, or nil when
// it cannot be found.
func (a *App) originKey(ctx context.Context, origin, carried string, env *fed.Envelope) ed25519.PublicKey {
	if origin == a.Self() {
		return a.ID.Pub
	}
	if origin == env.From {
		k, _ := fed.DecodeKey(env.Key)
		return k
	}
	if inst, err := a.St.GetInstance(ctx, origin); err == nil && inst != nil && inst.PublicKey != "" {
		if k, err := fed.DecodeKey(inst.PublicKey); err == nil {
			return k
		}
	}
	if fed.IsKeyID(origin) {
		if k, err := fed.DecodeKey(carried); err == nil && fed.KeyMatchesID(k, origin) {
			return k
		}
		return nil
	}
	a.keysMu.Lock()
	m, ok := a.keyMemo[origin]
	a.keysMu.Unlock()
	if ok && a.Now().Sub(m.at) < time.Hour {
		return m.pub // nil when the last try failed, so a dead origin is not refetched for every alert
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, pub, err := a.Fed.FetchWellKnownUntrusted(ctx, origin)
	if err != nil {
		a.Log.Debug("could not fetch origin key", "origin", origin, "err", err)
		pub = nil
	}
	a.keysMu.Lock()
	if len(a.keyMemo) >= 1000 {
		a.keyMemo = map[string]keyMemo{}
	}
	a.keyMemo[origin] = keyMemo{pub: pub, at: a.Now()}
	a.keysMu.Unlock()
	return pub
}

// hopKey finds the key to check a hop, or nil.
func (a *App) hopKey(ctx context.Context, host string, env *fed.Envelope) ed25519.PublicKey {
	if host == a.Self() {
		return a.ID.Pub
	}
	if host == env.From {
		k, _ := fed.DecodeKey(env.Key)
		return k
	}
	if inst, err := a.St.GetInstance(ctx, host); err == nil && inst != nil && inst.PublicKey != "" {
		k, _ := fed.DecodeKey(inst.PublicKey)
		return k
	}
	return nil
}

func (a *App) inAlert(ctx context.Context, env *fed.Envelope, m *fed.AlertMessage) error {
	al, err := m.Alert.Parse()
	if err != nil {
		return rejected("bad alert: %v", err)
	}
	now := a.Now()
	if fed.Expired(al.CreatedAt, now) {
		return rejected("alert %s is past its 14 days", al.AlertID)
	}
	if time.Unix(al.CreatedAt, 0).Sub(now) > fed.MaxClockSkew {
		return rejected("alert is dated in the future")
	}
	if !config.IsSnowflake(al.TargetID) || !config.IsSnowflake(al.OriginGuild) || !config.IsSnowflake(al.SenderID) {
		return rejected("alert has bad IDs")
	}
	if len(m.Hops) > fed.MaxHops || len(m.Targets) > fed.MaxTargets {
		return rejected("alert carries too many hops or targets")
	}
	verified := false
	if k := a.originKey(ctx, al.OriginHost, al.OriginKey, env); k != nil {
		if !m.Alert.Verify(k) {
			return rejected("origin signature does not verify")
		}
		verified = true
	}

	viaGuild, viaHost := al.OriginGuild, al.OriginHost
	var path []string
	hopOK := make([]bool, len(m.Hops))
	if len(m.Hops) == 0 {
		if al.OriginHost != env.From {
			return rejected("alert from %s arrived from %s with no forward record", al.OriginHost, env.From)
		}
	} else {
		last := m.Hops[len(m.Hops)-1]
		if last.Host != env.From {
			return rejected("last forward is from %s but the message came from %s", last.Host, env.From)
		}
		seen := map[string]bool{}
		for i := range m.Hops {
			h := &m.Hops[i]
			if h.AlertID != al.AlertID || h.N != i+1 || !config.IsSnowflake(h.GuildID) || !config.IsSnowflake(h.ByID) ||
				!fed.ValidHost(h.Host) || seen[h.Key()] || (h.GuildID == al.OriginGuild && h.Host == al.OriginHost) {
				return rejected("forward chain is malformed")
			}
			seen[h.Key()] = true
			if k := a.hopKey(ctx, h.Host, env); k != nil {
				if !fed.VerifyHop(h, k) {
					return rejected("forward %d does not verify", h.N)
				}
				hopOK[i] = true
			}
			path = append(path, h.Key())
		}
		viaGuild, viaHost = last.GuildID, last.Host
	}

	row := alertRow(al, &m.Alert, verified)
	existing, err := a.St.GetAlert(ctx, al.AlertID)
	if err != nil {
		return err
	}
	switch {
	case existing == nil:
		if err := a.St.InsertAlert(ctx, row); err != nil && !errors.Is(err, store.ErrDuplicate) {
			return err
		}
		a.keepEvidence(ctx, &row, al, m.Evidence, env.From)
		existing = &row
	case !bytes.Equal(existing.Signed, m.Alert.Alert):
		// Same ID, different content. Alert IDs are printed on every card,
		// so a peer can try to plant its own version under a real ID.
		// Only a copy the origin actually signed may replace one that
		// could not be checked; everything else is refused.
		if !verified || existing.OriginVerified {
			return rejected("alert %s is already known here with different content", al.AlertID)
		}
		if err := a.St.ReplaceClaim(ctx, row); err != nil {
			return err
		}
		os.RemoveAll(a.evidenceDir(al.AlertID))
		row.RetractedAt, row.Status = existing.RetractedAt, existing.Status
		a.keepEvidence(ctx, &row, al, m.Evidence, env.From)
		a.Log.Warn("replaced an unverified alert with the origin-signed version", "alert", al.AlertID, "from", env.From)
		existing = &row
		defer a.refreshAllCards(ctx, al.AlertID)
	case verified && !existing.OriginVerified:
		a.St.SetOriginVerified(ctx, al.AlertID)
		existing.OriginVerified = true
		defer a.refreshAllCards(ctx, al.AlertID)
	}
	if existing.Status == store.AlertHeld {
		return rejected("alert is held on this instance")
	}
	for i, h := range m.Hops {
		a.St.InsertHop(ctx, store.Hop{AlertID: h.AlertID, N: h.N, GuildID: h.GuildID, Host: h.Host, GuildName: cleanName(h.GuildName),
			ByID: h.ByID, ByName: cleanName(h.ByName), Sig: h.Sig, At: h.At, Verified: hopOK[i]})
	}
	if existing.RetractedAt > 0 {
		return nil
	}
	delivered := 0
	for _, target := range m.Targets {
		if target == al.OriginGuild && al.OriginHost == a.Self() {
			continue
		}
		sub, err := a.St.GetSub(ctx, viaGuild, viaHost, target, a.Self())
		if err != nil {
			return err
		}
		if sub == nil || sub.Status != store.SubActive {
			a.Log.Info("dropped alert for a server that does not follow the sender", "alert", al.AlertID, "target", target, "via", viaGuild+"@"+viaHost)
			continue
		}
		if st, _ := a.deliverLocal(ctx, existing, target, path, env.From); st == deliverOK {
			delivered++
		}
	}
	a.Log.Info("alert received", "alert", al.AlertID, "from", env.From, "delivered", delivered, "targets", len(m.Targets), "origin_verified", verified)
	return nil
}

// keepEvidence saves evidence that came with a remote alert if it matches
// the origin's signed hash, fits this instance's cap and is a type GossipWire
// passes on. Anything else is dropped and the card says so.
func (a *App) keepEvidence(ctx context.Context, row *store.Alert, al *fed.Alert, data []byte, from string) {
	if al.Evidence == nil || len(data) == 0 {
		return
	}
	sum := sha256.Sum256(data)
	limit := int64(a.St.GetLimits(ctx).EvidenceMB) << 20
	switch {
	case hex.EncodeToString(sum[:]) != al.Evidence.SHA256:
		a.Log.Warn("evidence does not match the signed hash; dropped", "alert", al.AlertID, "from", from)
	case int64(len(data)) > limit:
		a.Log.Info("evidence over this instance's cap; dropped", "alert", al.AlertID, "size", len(data))
	case evidenceKind(data) == "":
		a.Log.Info("evidence is not an image, video or text file; dropped", "alert", al.AlertID, "from", from)
	default:
		if err := a.saveEvidence(ctx, row, data); err != nil {
			a.Log.Warn("could not save evidence", "alert", al.AlertID, "err", err)
		}
	}
}

func (a *App) inRetract(ctx context.Context, env *fed.Envelope, r *fed.Retraction) error {
	if !fed.CanonicalUUID(r.AlertID) {
		return rejected("bad alert ID")
	}
	al, err := a.St.GetAlert(ctx, r.AlertID)
	if err != nil {
		return err
	}
	if al == nil {
		return nil // never had it, or already purged
	}
	if r.OriginHost != al.OriginHost {
		return rejected("retraction origin does not match the alert")
	}
	if k := a.originKey(ctx, al.OriginHost, r.OriginKey, env); k != nil {
		if !fed.VerifyRetraction(r, k) {
			return rejected("retraction signature does not verify")
		}
	} else {
		// Without the origin's key, accept a retraction only from an
		// instance that delivered this alert here: it is trusted exactly
		// as far as the alert it passed on.
		ds, err := a.St.DeliveriesForAlert(ctx, al.ID)
		if err != nil {
			return err
		}
		ok := false
		for _, d := range ds {
			if d.ViaHost == env.From {
				ok = true
				break
			}
		}
		if !ok {
			return rejected("cannot verify the retraction and %s did not deliver this alert here", env.From)
		}
	}
	changed, err := a.St.SetRetracted(ctx, al.ID, r.RetractedAt, r.Sig)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	a.refreshAllCards(ctx, al.ID)
	a.relayRetraction(ctx, r, env.From)
	a.Log.Info("alert retracted by origin", "alert", al.ID, "from", env.From)
	return nil
}

// PollFor hands an outbound-only peer the messages queued for it. Everything
// up to after is taken as read.
func (a *App) PollFor(ctx context.Context, peer string, after int64) (*fed.PollResponse, error) {
	now := a.Now()
	if after > 0 {
		if err := a.St.AckThrough(ctx, peer, after, now.Unix()); err != nil {
			return nil, err
		}
	}
	rows, err := a.St.Pending(ctx, peer, after, 25)
	if err != nil {
		return nil, err
	}
	resp := &fed.PollResponse{Messages: []fed.Polled{}}
	total := 0
	for _, r := range rows {
		if total > 0 && total+len(r.Payload) > 30<<20 {
			break
		}
		total += len(r.Payload)
		resp.Messages = append(resp.Messages, fed.Polled{Seq: r.Seq, Envelope: a.ID.Seal(peer, r.Type, r.Payload, now)})
	}
	return resp, nil
}
