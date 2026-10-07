package store

import (
	"context"
	"path/filepath"
	"testing"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// A verified hop replaces an unverified one with the same key; nothing
// replaces a verified hop.
func TestHopUpgrade(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	h := Hop{AlertID: "a", N: 1, GuildID: "g", Host: "h", GuildName: "planted", ByID: "1", ByName: "liar", Sig: "x", At: 1}
	s.InsertHop(ctx, h)
	h.GuildName, h.ByName, h.Verified = "Real", "realmod", true
	s.InsertHop(ctx, h)
	h.GuildName, h.ByName, h.Verified = "late forgery", "liar", false
	s.InsertHop(ctx, h)
	got := must(s.HopsByKey(ctx, "a"))["g@h"]
	if got.GuildName != "Real" || !got.Verified {
		t.Fatalf("got %+v", got)
	}
}

func TestDecisionSlot(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if err := s.InsertAction(ctx, Action{AlertID: "a", GuildID: "g", ModeratorID: "1", Action: ActBan, At: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertAction(ctx, Action{AlertID: "a", GuildID: "g", ModeratorID: "2", Action: ActIgnore, At: 2}); err != ErrDuplicate {
		t.Fatalf("second decision: %v", err)
	}
	if err := s.InsertAction(ctx, Action{AlertID: "a", GuildID: "g", ModeratorID: "2", Action: ActForward, At: 2}); err != nil {
		t.Fatalf("forward after a decision: %v", err)
	}
	if err := s.InsertAction(ctx, Action{AlertID: "a", GuildID: "g2", ModeratorID: "3", Action: ActBan, At: 3}); err != nil {
		t.Fatalf("other server: %v", err)
	}
}

func TestDeleteAlertRemovesEverything(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	s.InsertAlert(ctx, Alert{ID: "a", Status: AlertSent, Signed: []byte("{}"), CreatedAt: 1})
	s.InsertHop(ctx, Hop{AlertID: "a", GuildID: "g", Host: "h"})
	s.AddRecipient(ctx, "a", "g", "h", "g0", "h0", 1)
	s.ClaimDelivery(ctx, Delivery{AlertID: "a", GuildID: "g", Status: DeliverySent, At: 1})
	s.InsertAction(ctx, Action{AlertID: "a", GuildID: "g", Action: ActBan})
	if err := s.DeleteAlert(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	for _, tbl := range []string{"alerts", "alert_hops", "alert_recipients", "deliveries", "actions"} {
		var n int
		s.db.QueryRow(`SELECT COUNT(*) FROM ` + tbl).Scan(&n)
		if n != 0 {
			t.Errorf("%s has %d rows left", tbl, n)
		}
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
