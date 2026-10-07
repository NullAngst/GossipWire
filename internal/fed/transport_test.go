package fed

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func TestPublicAddr(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "10.1.2.3", "172.16.0.9", "192.168.1.1", "169.254.169.254", "100.100.1.1", "0.0.0.0", "::1", "fd00::1", "fe80::1", "::ffff:127.0.0.1"} {
		if PublicAddr(netip.MustParseAddr(s)) {
			t.Errorf("%s treated as public", s)
		}
	}
	for _, s := range []string{"1.1.1.1", "2606:4700:4700::1111"} {
		if !PublicAddr(netip.MustParseAddr(s)) {
			t.Errorf("%s treated as private", s)
		}
	}
}

// The untrusted client must refuse an instance on an internal address,
// even one that would answer correctly.
func TestUntrustedClientRefusesInternal(t *testing.T) {
	var host string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(WellKnown{Software: "gossipwire", Name: host, PublicKey: newID(t, "x.test").PubB64(), Protocol: ProtocolVersion})
	}))
	defer srv.Close()
	host = srv.Listener.Addr().String()
	c := NewClient()
	c.HTTP = srv.Client()
	if _, _, err := c.FetchWellKnown(context.Background(), host); err != nil {
		t.Fatalf("trusted fetch failed: %v", err)
	}
	if _, _, err := c.FetchWellKnownUntrusted(context.Background(), host); err == nil || !strings.Contains(err.Error(), "non-public") {
		t.Fatalf("untrusted fetch of %s: %v", host, err)
	}
}

func TestNoRedirects(t *testing.T) {
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("redirect was followed")
	}))
	defer target.Close()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+PathWellKnown, http.StatusFound)
	}))
	defer srv.Close()
	c := NewClient()
	tr := srv.Client().Transport
	c.HTTP = &http.Client{Transport: tr, CheckRedirect: noRedirects}
	if _, _, err := c.FetchWellKnown(context.Background(), srv.Listener.Addr().String()); err == nil {
		t.Fatal("redirected fetch succeeded")
	}
}
