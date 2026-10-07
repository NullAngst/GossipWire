package fed

import (
	"encoding/json"
	"testing"
	"time"
)

func newID(t *testing.T, host string) *Identity {
	t.Helper()
	id, created, err := LoadOrCreateIdentity(t.TempDir(), host)
	if err != nil || !created {
		t.Fatal(err)
	}
	return id
}

func TestIdentityPersists(t *testing.T) {
	dir := t.TempDir()
	a, _, err := LoadOrCreateIdentity(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	b, created, err := LoadOrCreateIdentity(dir, "")
	if err != nil || created {
		t.Fatal("second load should reuse the key", err)
	}
	if a.ID != b.ID || !IsKeyID(a.ID) || !KeyMatchesID(a.Pub, a.ID) {
		t.Fatalf("ids %s %s", a.ID, b.ID)
	}
}

func TestEnvelope(t *testing.T) {
	a := newID(t, "a.test")
	now := time.Now()
	env := a.Seal("b.test", TypeAlert, []byte(`{"x":1}`), now)
	if _, err := VerifyEnvelope(env, "b.test", now); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyEnvelope(env, "c.test", now); err == nil {
		t.Fatal("wrong target accepted")
	}
	if _, err := VerifyEnvelope(env, "b.test", now.Add(6*time.Minute)); err == nil {
		t.Fatal("stale envelope accepted")
	}
	tampered := *env
	tampered.Body = json.RawMessage(`{"x":2}`)
	if _, err := VerifyEnvelope(&tampered, "b.test", now); err == nil {
		t.Fatal("tampered body accepted")
	}
	// A key- sender must use the key its name came from.
	k := newID(t, "")
	other := newID(t, "")
	env = k.Seal("b.test", TypeAlert, []byte(`{}`), now)
	env.Key = other.PubB64()
	if _, err := VerifyEnvelope(env, "b.test", now); err == nil {
		t.Fatal("key mismatch accepted")
	}
}

func TestAlertHopRetract(t *testing.T) {
	a := newID(t, "a.test")
	b := newID(t, "b.test")
	sa, err := a.SignAlert(&Alert{AlertID: "6f1c1d4e-58c4-4b9e-bf54-3a3f1d2a9c10", Reason: "spam", CreatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !sa.Verify(a.Pub) || sa.Verify(b.Pub) {
		t.Fatal("alert signature check wrong")
	}
	// A middle instance rewriting the header breaks the origin signature.
	forged := *sa
	forged.Alert = json.RawMessage(string(sa.Alert[:len(sa.Alert)-1]) + `,"x":1}`)
	if forged.Verify(a.Pub) {
		t.Fatal("rewritten alert verified")
	}
	h := Hop{AlertID: "id", N: 1, GuildID: "1", GuildName: "G", Host: "b.test", ByID: "2", ByName: "mod", At: 5}
	b.SignHop(&h)
	if !VerifyHop(&h, b.Pub) {
		t.Fatal("hop does not verify")
	}
	h.ByName = "someone else"
	if VerifyHop(&h, b.Pub) {
		t.Fatal("edited hop verified")
	}
	r := Retraction{AlertID: "id", OriginHost: "a.test", RetractedAt: 9}
	a.SignRetraction(&r)
	if !VerifyRetraction(&r, a.Pub) || VerifyRetraction(&r, b.Pub) {
		t.Fatal("retraction check wrong")
	}
}

func TestParseAddress(t *testing.T) {
	g, h, err := ParseAddress("112233445566778899")
	if err != nil || g != "112233445566778899" || h != "" {
		t.Fatal(g, h, err)
	}
	g, h, err = ParseAddress("112233445566778899@Alerts.Example.org")
	if err != nil || h != "alerts.example.org" {
		t.Fatal(g, h, err)
	}
	for _, bad := range []string{"abc", "123@", "112233445566778899@bad host", "112233445566778899@a/b"} {
		if _, _, err := ParseAddress(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestExpired(t *testing.T) {
	now := time.Now()
	if Expired(now.Add(-13*24*time.Hour).Unix(), now) || !Expired(now.Add(-15*24*time.Hour).Unix(), now) {
		t.Fatal("14 day window wrong")
	}
}

func TestCanonicalUUID(t *testing.T) {
	good := "6f1c1d4e-58c4-4b9e-bf54-3a3f1d2a9c10"
	if !CanonicalUUID(good) {
		t.Fatal("canonical form refused")
	}
	for _, bad := range []string{"6F1C1D4E-58C4-4B9E-BF54-3A3F1D2A9C10", "{" + good + "}", "urn:uuid:" + good, "6f1c1d4e58c44b9ebf543a3f1d2a9c10", ""} {
		if CanonicalUUID(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestEnvelopeSenderMustBeAHost(t *testing.T) {
	now := time.Now()
	for _, from := range []string{"evil.example/path?x=", "a b", "EVIL.example", "evil`.example", ""} {
		id := newID(t, "placeholder.test")
		id.ID = from
		env := id.Seal("b.test", TypeAlert, []byte(`{}`), now)
		if _, err := VerifyEnvelope(env, "b.test", now); err == nil {
			t.Errorf("sender %q accepted", from)
		}
	}
}
