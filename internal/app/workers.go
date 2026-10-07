package app

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/NullAngst/GossipWire/internal/config"
	"github.com/NullAngst/GossipWire/internal/fed"
	"github.com/NullAngst/GossipWire/internal/store"
)

const (
	pollInterval   = 60 * time.Second
	maxPeerBackoff = 5 * time.Minute
	firstRetry     = 10 * time.Second
)

// RunWorkers starts the outbox pusher, the poller (outbound-only instances)
// and the daily purge. It returns when ctx is cancelled.
func (a *App) RunWorkers(ctx context.Context) {
	go a.purgeLoop(ctx)
	if a.Cfg.FederationMode == config.ModeOff {
		<-ctx.Done()
		return
	}
	if !a.Cfg.Reachable() {
		go a.pollLoop(ctx)
	}
	a.pushLoop(ctx)
}

func (a *App) purgeLoop(ctx context.Context) {
	a.Purge(ctx)
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.Purge(ctx)
		}
	}
}

func (a *App) pushLoop(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		a.PushOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-a.wake:
		}
	}
}

func (a *App) due(peer string) bool {
	a.backoffMu.Lock()
	defer a.backoffMu.Unlock()
	b, ok := a.backoff[peer]
	return !ok || !a.Now().Before(b.next)
}

func (a *App) failed(peer string, base time.Duration) time.Duration {
	a.backoffMu.Lock()
	defer a.backoffMu.Unlock()
	b := a.backoff[peer]
	if b.delay == 0 {
		b.delay = base
	} else {
		b.delay *= 2
	}
	if b.delay > maxPeerBackoff {
		b.delay = maxPeerBackoff
	}
	b.next = a.Now().Add(b.delay)
	a.backoff[peer] = b
	return b.delay
}

func (a *App) succeeded(peer string) {
	a.backoffMu.Lock()
	delete(a.backoff, peer)
	a.backoffMu.Unlock()
}

// PushOnce sends queued messages to every reachable peer that is due.
// Messages for outbound-only peers wait in the outbox until they poll.
func (a *App) PushOnce(ctx context.Context) {
	peers, err := a.St.PeersWithDueMessages(ctx, a.Now().Unix())
	if err != nil {
		a.Log.Warn("outbox: listing peers failed", "err", err)
		return
	}
	for _, peer := range peers {
		if fed.IsKeyID(peer) || !a.due(peer) {
			continue
		}
		inst, err := a.St.GetInstance(ctx, peer)
		if err != nil {
			continue
		}
		if inst == nil || inst.Status == store.PeerBlocked {
			a.St.DropOutboxFor(ctx, peer)
			continue
		}
		if inst.Status != store.PeerAllowed {
			continue // key changed: hold until the operator decides
		}
		a.pushPeer(ctx, peer)
	}
}

func (a *App) pushPeer(ctx context.Context, peer string) {
	for {
		rows, err := a.St.Pending(ctx, peer, 0, 20)
		if err != nil {
			a.Log.Warn("outbox: reading queue failed", "peer", peer, "err", err)
			return
		}
		if len(rows) == 0 {
			return
		}
		for _, r := range rows {
			if ctx.Err() != nil {
				return
			}
			env := a.ID.Seal(peer, r.Type, r.Payload, a.Now())
			pctx, cancel := context.WithTimeout(ctx, pushTimeout(len(r.Payload)))
			err := a.Fed.Push(pctx, env)
			cancel()
			now := a.Now().Unix()
			var perm *fed.PermanentError
			switch {
			case err == nil:
				a.succeeded(peer)
				if derr := a.St.MarkDelivered(ctx, r.Seq, now, ""); derr != nil {
					// Without this check a database error would resend the
					// same message in a tight loop.
					a.Log.Error("outbox: could not mark a message delivered", "peer", peer, "err", derr)
					a.failed(peer, firstRetry)
					return
				}
			case errors.As(err, &perm):
				a.Log.Warn("peer refused a message; dropping it", "peer", peer, "type", r.Type, "reason", perm.Msg)
				if derr := a.St.MarkDelivered(ctx, r.Seq, now, truncate(perm.Msg, 500)); derr != nil {
					a.Log.Error("outbox: could not drop a refused message", "peer", peer, "err", derr)
					a.failed(peer, firstRetry)
					return
				}
			default:
				d := a.failed(peer, firstRetry)
				a.St.RetryLater(ctx, r.Seq, r.Attempts+1, 0, truncate(err.Error(), 500))
				a.Log.Info("peer unreachable, will retry", "peer", peer, "in", d.String(), "err", err)
				return // keep order: nothing else goes to this peer until this one does
			}
		}
	}
}

// pushTimeout gives large messages (evidence up to 25 MB) time to upload on
// a slow home connection. A fixed timeout would fail the same big message
// forever and block everything queued behind it.
func pushTimeout(size int) time.Duration {
	return 60*time.Second + time.Duration(size>>20)*10*time.Second
}

func (a *App) pollLoop(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		a.PollOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// PollOnce asks each reachable peer that is due for messages queued for
// this instance. Only outbound-only instances poll.
func (a *App) PollOnce(ctx context.Context) {
	insts, err := a.St.ListInstances(ctx)
	if err != nil {
		return
	}
	for _, inst := range insts {
		if inst.Host == a.Self() || fed.IsKeyID(inst.Host) || inst.Status != store.PeerAllowed || !a.due("poll:"+inst.Host) {
			continue
		}
		if err := a.pollPeer(ctx, inst); err != nil {
			d := a.failed("poll:"+inst.Host, pollInterval)
			a.Log.Info("poll failed, backing off", "peer", inst.Host, "next_in", d.String(), "err", err)
			continue
		}
		// Back to the normal 60 second rhythm.
		a.backoffMu.Lock()
		a.backoff["poll:"+inst.Host] = peerBackoff{next: a.Now().Add(pollInterval)}
		a.backoffMu.Unlock()
	}
}

func (a *App) pollPeer(ctx context.Context, inst store.Instance) error {
	cursor := inst.PollCursor
	for round := 0; round < 20; round++ {
		body, _ := json.Marshal(fed.PollRequest{After: cursor})
		env := a.ID.Seal(inst.Host, fed.TypePoll, body, a.Now())
		pctx, cancel := context.WithTimeout(ctx, 5*time.Minute) // an answer can carry ~30 MB
		resp, err := a.Fed.Poll(pctx, env)
		cancel()
		if err != nil {
			return err
		}
		if len(resp.Messages) == 0 {
			if cursor != inst.PollCursor {
				// Acknowledge what was read on the next poll; save the cursor now.
				return a.St.SetPollCursor(ctx, inst.Host, cursor)
			}
			return nil
		}
		for _, m := range resp.Messages {
			if m.Seq <= cursor || m.Envelope == nil {
				continue
			}
			if err := a.handlePolled(ctx, inst.Host, m.Envelope); err != nil {
				if errors.Is(err, fed.ErrRejected) {
					a.Log.Info("rejected polled message", "peer", inst.Host, "type", m.Envelope.Type, "reason", err)
				} else {
					a.St.SetPollCursor(ctx, inst.Host, cursor)
					return err
				}
			}
			cursor = m.Seq
		}
		if err := a.St.SetPollCursor(ctx, inst.Host, cursor); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) handlePolled(ctx context.Context, peer string, env *fed.Envelope) error {
	pub, err := fed.VerifyEnvelope(env, a.Self(), a.Now())
	if err != nil {
		return rejected("%v", err)
	}
	if env.From != peer {
		return rejected("message in %s's outbox claims to be from %s", peer, env.From)
	}
	if !fed.KnownType(env.Type) {
		return rejected("unknown type %s", env.Type)
	}
	if err := a.Authenticate(ctx, env, pub); err != nil {
		return rejected("%v", err)
	}
	return a.HandleMessage(ctx, env)
}
