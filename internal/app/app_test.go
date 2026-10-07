package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/disgoorg/disgo/discord"

	"github.com/NullAngst/GossipWire/internal/config"
	"github.com/NullAngst/GossipWire/internal/fed"
	"github.com/NullAngst/GossipWire/internal/store"
)

// ---------------------------------------------------------------- fake Discord

type fakeMsg struct {
	channel string
	content string
	embeds  []discord.Embed
	comps   []discord.LayoutComponent
	files   []string
}

type fakeDiscord struct {
	mu      sync.Mutex
	next    int
	msgs    map[string]*fakeMsg // message ID -> message
	order   []string
	guilds  map[string][2]string // id -> name, owner
	members map[string]bool      // guild/user
	bans    []string             // guild/user/reason
	dms     map[string][]discord.MessageCreate
	denyBan bool
}

func newFake() *fakeDiscord {
	return &fakeDiscord{msgs: map[string]*fakeMsg{}, guilds: map[string][2]string{}, members: map[string]bool{}, dms: map[string][]discord.MessageCreate{}, next: 900000000000000000}
}

func (f *fakeDiscord) Send(_ context.Context, ch string, m discord.MessageCreate) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	id := fmt.Sprint(f.next)
	fm := &fakeMsg{channel: ch, content: m.Content, embeds: m.Embeds, comps: m.Components}
	for _, file := range m.Files {
		b, _ := io.ReadAll(file.Reader)
		fm.files = append(fm.files, file.Name+":"+string(b))
	}
	f.msgs[id] = fm
	f.order = append(f.order, id)
	return id, nil
}

func (f *fakeDiscord) Edit(_ context.Context, ch, id string, m discord.MessageUpdate) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	fm := f.msgs[id]
	if fm == nil {
		return ErrNotFound
	}
	if m.Embeds != nil {
		fm.embeds = *m.Embeds
	}
	if m.Components != nil {
		fm.comps = *m.Components
	}
	return nil
}

func (f *fakeDiscord) Ban(_ context.Context, g, u, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.denyBan {
		return fmt.Errorf("%w: role order", ErrForbidden)
	}
	f.bans = append(f.bans, g+"/"+u+"/"+reason)
	return nil
}

func (f *fakeDiscord) IsMember(_ context.Context, g, u string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.members[g+"/"+u], nil
}

func (f *fakeDiscord) User(_ context.Context, id string) (*discord.User, error) {
	if strings.HasPrefix(id, "999") {
		return nil, ErrNotFound
	}
	return &discord.User{Username: "scammer" + id[len(id)-3:]}, nil
}

func (f *fakeDiscord) DM(_ context.Context, u string, m discord.MessageCreate) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dms[u] = append(f.dms[u], m)
	return nil
}

func (f *fakeDiscord) Guild(id string) (string, string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	g, ok := f.guilds[id]
	return g[0], g[1], ok
}

func (f *fakeDiscord) GuildCount() int { return len(f.guilds) }

// cards returns messages in a channel that are alert cards, oldest first.
func (f *fakeDiscord) inChannel(ch string) []*fakeMsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*fakeMsg
	for _, id := range f.order {
		if m := f.msgs[id]; m.channel == ch {
			out = append(out, m)
		}
	}
	return out
}

func (f *fakeDiscord) cards(ch string) []*fakeMsg {
	var out []*fakeMsg
	for _, m := range f.inChannel(ch) {
		if len(m.embeds) > 0 && strings.Contains(m.embeds[0].Title, "Alert from") {
			out = append(out, m)
		}
	}
	return out
}

func (f *fakeDiscord) requests(ch string) []*fakeMsg {
	var out []*fakeMsg
	for _, m := range f.inChannel(ch) {
		if len(m.embeds) > 0 && m.embeds[0].Title == "Subscription request" {
			out = append(out, m)
		}
	}
	return out
}

func buttonIDs(m *fakeMsg) []string {
	var ids []string
	for _, c := range m.comps {
		if row, ok := c.(discord.ActionRowComponent); ok {
			for _, b := range row.Components {
				if btn, ok := b.(discord.ButtonComponent); ok {
					ids = append(ids, btn.CustomID)
				}
			}
		}
	}
	return ids
}

func buttonLabels(m *fakeMsg) string {
	var ls []string
	for _, c := range m.comps {
		if row, ok := c.(discord.ActionRowComponent); ok {
			for _, b := range row.Components {
				if btn, ok := b.(discord.ButtonComponent); ok {
					ls = append(ls, btn.Label)
				}
			}
		}
	}
	return strings.Join(ls, ",")
}

func cardText(m *fakeMsg) string {
	e := m.embeds[0]
	s := e.Title + "\n" + e.Description
	for _, f := range e.Fields {
		s += "\n" + f.Name + ": " + f.Value
	}
	return s
}

// ---------------------------------------------------------------- harness

type inst struct {
	a  *App
	dc *fakeDiscord
}

const owner = "100000000000000001"

func newInst(t *testing.T, mode, url string, client *http.Client) *inst {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{OwnerID: owner, FederationMode: mode, DataDir: dir}
	host := ""
	if url != "" {
		cfg.InstanceURL = url
		cfg.InstanceHost = strings.TrimPrefix(url, "https://")
		host = cfg.InstanceHost
	}
	st, err := store.Open(filepath.Join(dir, "gw.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	id, _, err := fed.LoadOrCreateIdentity(dir, host)
	if err != nil {
		t.Fatal(err)
	}
	dc := newFake()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	a := New(cfg, st, dc, id, log)
	a.AppID = "200000000000000002"
	if client != nil {
		a.Fed.HTTP = client
	}
	if err := a.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	return &inst{a: a, dc: dc}
}

// guild registers a server on an instance and runs /setup.
func (in *inst) guild(t *testing.T, id, name, ownerID string) Actor {
	t.Helper()
	in.dc.guilds[id] = [2]string{name, ownerID}
	act := Actor{UserID: "300000000000000" + id[len(id)-3:], UserName: "mod" + strings.ReplaceAll(name, " ", ""), GuildID: id, GuildName: name}
	if _, err := in.a.Setup(context.Background(), act, "chan"+id); err != nil {
		t.Fatal(err)
	}
	return act
}

// must fails the test (by panic, which the test runner reports with a
// stack) when a call that should work returns an error.
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// png makes bytes that sniff as a PNG.
func png(payload string) []byte { return append([]byte("\x89PNG\r\n\x1a\n"), payload...) }

func alertFrom(t *testing.T, in *inst, act Actor, target, reason string, evidence []byte) string {
	t.Helper()
	ctx := context.Background()
	var att *AttachmentIn
	if evidence != nil {
		in.a.FetchEvidence = func(context.Context, string, int64) ([]byte, error) { return evidence, nil }
		att = &AttachmentIn{Name: "shot.png", ContentType: "image/png", Size: len(evidence), URL: "https://cdn.discordapp.com/attachments/1/2/shot.png"}
	}
	d := must(in.a.PrepareAlert(ctx, act, "chan"+act.GuildID, target, reason, "", att))
	msg := must(in.a.SendDraft(ctx, d.ID, act))
	i := strings.Index(msg, "`")
	return msg[i+1 : i+37]
}

func approveAll(t *testing.T, in *inst, act Actor, decision string) {
	t.Helper()
	for _, m := range in.dc.requests("chan" + act.GuildID) {
		ids := buttonIDs(m)
		if len(ids) == 0 {
			continue
		}
		var n int64
		fmt.Sscanf(strings.TrimPrefix(ids[0], "gw:sa:"), "%d", &n)
		must(in.a.DecideSubscription(context.Background(), n, decision, act))
	}
}

const (
	g1 = "110000000000000001"
	g2 = "110000000000000002"
	g3 = "110000000000000003"
	tA = "500000000000000123"
)

// ---------------------------------------------------------------- tests

func TestLocalFlow(t *testing.T) {
	ctx := context.Background()
	in := newInst(t, config.ModeOff, "", nil)
	a1 := in.guild(t, g1, "Server A", "400000000000000001")
	b1 := in.guild(t, g2, "Server B", "400000000000000002")
	c1 := in.guild(t, g3, "Server C", "400000000000000003")

	// B follows A, C follows B.
	must(in.a.Subscribe(ctx, b1, g1))
	approveAll(t, in, a1, DecideApprove)
	must(in.a.Subscribe(ctx, c1, g2))
	approveAll(t, in, b1, DecideApprove)
	if _, err := in.a.Subscribe(ctx, b1, g1); err == nil {
		t.Fatal("duplicate subscribe accepted")
	}

	in.dc.members[g2+"/"+tA] = true
	id := alertFrom(t, in, a1, tA, "spamming crypto scams", png("DATA"))

	if n := len(in.dc.cards("chan" + g1)); n != 0 {
		t.Fatalf("origin got its own card: %d", n)
	}
	bc := in.dc.cards("chan" + g2)
	if len(bc) != 1 {
		t.Fatalf("B got %d cards", len(bc))
	}
	txt := cardText(bc[0])
	for _, want := range []string{"Alert from @modServerA in Server A", "spamming crypto scams", "In this server: Yes", "Attached: `shot.png`"} {
		if !strings.Contains(txt, want) {
			t.Errorf("card missing %q:\n%s", want, txt)
		}
	}
	if strings.Contains(txt, "Origin not verified") {
		t.Error("local alert marked unverified")
	}
	if len(bc[0].files) != 1 || bc[0].files[0] != "shot.png:"+string(png("DATA")) {
		t.Errorf("evidence not re-uploaded: %v", bc[0].files)
	}
	if got := buttonLabels(bc[0]); got != "Ban,Ignore,Forward" {
		t.Errorf("buttons %s", got)
	}
	if len(in.dc.cards("chan"+g3)) != 0 {
		t.Fatal("C got the alert without a forward")
	}

	// Ban in B, then a second press loses.
	if _, err := in.a.Press(ctx, store.ActBan, id, b1); err != nil {
		t.Fatal(err)
	}
	if len(in.dc.bans) != 1 || !strings.Contains(in.dc.bans[0], "GossipWire from Server A via") {
		t.Fatalf("ban %v", in.dc.bans)
	}
	if !strings.Contains(cardText(bc[0]), "Banned by <@") || buttonLabels(bc[0]) != "Forward" {
		t.Fatalf("after ban: %s / %s", cardText(bc[0]), buttonLabels(bc[0]))
	}
	other := b1
	other.UserID = "300000000000000999"
	if _, err := in.a.Press(ctx, store.ActIgnore, id, other); err == nil || !strings.Contains(Friendly(err), "Already handled") {
		t.Fatalf("second press: %v", err)
	}

	// Forward from B reaches C with the chain shown.
	if _, err := in.a.Press(ctx, store.ActForward, id, b1); err != nil {
		t.Fatal(err)
	}
	cc := in.dc.cards("chan" + g3)
	if len(cc) != 1 || !strings.Contains(cardText(cc[0]), "Forwarded by @modServerB in Server B") ||
		!strings.Contains(cardText(cc[0]), "Alert from @modServerA in Server A") {
		t.Fatalf("C card: %d %v", len(cc), cc)
	}
	if buttonLabels(bc[0]) != "" {
		t.Errorf("B buttons after ban and forward: %s", buttonLabels(bc[0]))
	}
	if _, err := in.a.Press(ctx, store.ActForward, id, b1); err == nil {
		t.Fatal("second forward accepted")
	}

	// A follows C: a forward from C must not hand A its own alert.
	must(in.a.Subscribe(ctx, a1, g3))
	approveAll(t, in, c1, DecideApprove)
	if _, err := in.a.Press(ctx, store.ActForward, id, c1); err != nil {
		t.Fatal(err)
	}
	if n := len(in.dc.cards("chan" + g1)); n != 0 {
		t.Fatalf("origin received its own alert back: %d", n)
	}

	// Only the origin can retract, and retract reaches every copy.
	if _, err := in.a.Retract(ctx, b1, id); err == nil {
		t.Fatal("non-origin retract accepted")
	}
	must(in.a.Retract(ctx, a1, id))
	for _, m := range append(in.dc.cards("chan"+g2), in.dc.cards("chan"+g3)...) {
		if !strings.HasPrefix(m.embeds[0].Title, "RETRACTED") || buttonLabels(m) != "" {
			t.Errorf("not retracted: %s %s", m.embeds[0].Title, buttonLabels(m))
		}
	}
	if _, err := in.a.Press(ctx, store.ActBan, id, c1); err == nil {
		t.Fatal("ban on retracted alert accepted")
	}

	// History sees it from both sides.
	h := must(in.a.History(ctx, a1, ""))
	if !strings.Contains(h.Description, "**Sent**") || !strings.Contains(h.Description, "retracted") {
		t.Errorf("A history: %s", h.Description)
	}
	h = must(in.a.History(ctx, c1, tA))
	if !strings.Contains(h.Description, "forward chain: @modServerB in Server B") {
		t.Errorf("C history: %s", h.Description)
	}
}

func TestRateLimitHoldAndRelease(t *testing.T) {
	ctx := context.Background()
	in := newInst(t, config.ModeOff, "", nil)
	a1 := in.guild(t, g1, "Server A", "400000000000000001")
	b1 := in.guild(t, g2, "Server B", "400000000000000002")
	must(in.a.Subscribe(ctx, b1, g1))
	approveAll(t, in, a1, DecideApprove)
	for i := 0; i < 5; i++ {
		alertFrom(t, in, a1, fmt.Sprintf("50000000000000%04d", i), "spam", nil)
	}
	held := alertFrom(t, in, a1, "500000000000009999", "spam", nil)
	if n := len(in.dc.cards("chan" + g2)); n != 5 {
		t.Fatalf("expected 5 delivered, got %d", n)
	}
	dms := in.dc.dms["400000000000000001"]
	if len(dms) != 1 || !strings.Contains(dms[0].Content, "held") {
		t.Fatalf("server owner DM: %v", dms)
	}
	if len(in.dc.dms[owner]) != 1 {
		t.Fatal("instance owner not told")
	}
	if _, err := in.a.ReleaseHeld(ctx, held, "400000000000000002", true); err == nil {
		t.Fatal("non-owner released")
	}
	must(in.a.ReleaseHeld(ctx, held, "400000000000000001", true))
	if n := len(in.dc.cards("chan" + g2)); n != 6 {
		t.Fatalf("release did not deliver: %d", n)
	}
	// Forwards are refused over the limit too.
	id := in.dc.cards("chan" + g2)[0].embeds[0].Footer.Text[len("Alert ID "):]
	for i := 0; i < 5; i++ {
		in.a.St.InsertAction(ctx, store.Action{AlertID: fmt.Sprint("x", i), GuildID: g2, ModeratorID: "1", Action: store.ActForward, At: time.Now().Unix()})
	}
	if _, err := in.a.Press(ctx, store.ActForward, id, b1); err == nil || !strings.Contains(Friendly(err), "Forwards count") {
		t.Fatalf("forward over limit: %v", err)
	}
}

func TestBanFailureAndExpiry(t *testing.T) {
	ctx := context.Background()
	in := newInst(t, config.ModeOff, "", nil)
	a1 := in.guild(t, g1, "Server A", "400000000000000001")
	b1 := in.guild(t, g2, "Server B", "400000000000000002")
	must(in.a.Subscribe(ctx, b1, g1))
	approveAll(t, in, a1, DecideApprove)
	id := alertFrom(t, in, a1, tA, "spam", nil)

	in.dc.denyBan = true
	if _, err := in.a.Press(ctx, store.ActBan, id, b1); err == nil || !strings.Contains(Friendly(err), "above the user's highest role") {
		t.Fatalf("expected a role order message, got %v", err)
	}
	in.dc.denyBan = false
	if _, err := in.a.Press(ctx, store.ActBan, id, b1); err != nil {
		t.Fatalf("retry after fixing roles: %v", err)
	}

	// 15 days later the alert is purged and buttons report expiry.
	in.a.Now = func() time.Time { return time.Now().Add(15 * 24 * time.Hour) }
	in.a.Purge(ctx)
	if _, err := in.a.Press(ctx, store.ActForward, id, b1); err != ErrExpired {
		t.Fatalf("expected ErrExpired, got %v", err)
	}
	if _, err := os.Stat(in.a.evidenceDir(id)); !os.IsNotExist(err) {
		t.Fatal("evidence dir not purged")
	}
}

func TestPrepareValidation(t *testing.T) {
	ctx := context.Background()
	in := newInst(t, config.ModeOff, "", nil)
	a1 := in.guild(t, g1, "Server A", "400000000000000001")
	cases := map[string][5]string{
		"wrong channel": {"otherchan", tA, "spam", "", "alert channel"},
		"bad id":        {"chan" + g1, "zack", "spam", "", "not a Discord user ID"},
		"no account":    {"chan" + g1, "999000000000000123", "spam", "", "No Discord account"},
		"long reason":   {"chan" + g1, tA, strings.Repeat("x", 501), "", "over 500"},
		"bad link":      {"chan" + g1, tA, "spam", "https://evil.example/x", "message link"},
	}
	for name, c := range cases {
		_, err := in.a.PrepareAlert(ctx, a1, c[0], c[1], c[2], c[3], nil)
		if err == nil || !strings.Contains(Friendly(err), c[4]) {
			t.Errorf("%s: %v", name, err)
		}
	}
	notSetUp := Actor{UserID: "1", GuildID: "110000000000000099"}
	if _, err := in.a.PrepareAlert(ctx, notSetUp, "c", tA, "x", "", nil); err == nil || !strings.Contains(Friendly(err), "/setup") {
		t.Errorf("not set up: %v", err)
	}
}

// fedPair is instance B, reachable over TLS, and instance A, outbound-only
// with no open port, both in allowlist mode.
//
// rekeyB gives B a brand new key, as if its operator wiped the data folder
// (or someone took over its domain), and serves the new key from then on.
func fedPair(t *testing.T) (A, B *inst, rekeyB func()) {
	t.Helper()
	var mu sync.Mutex
	var handler http.Handler
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		h := handler
		mu.Unlock()
		h.ServeHTTP(w, r)
	}))
	srv.StartTLS()
	t.Cleanup(srv.Close)
	host := srv.Listener.Addr().String()
	B = newInst(t, config.ModeAllowlist, "https://"+host, srv.Client())
	A = newInst(t, config.ModeAllowlist, "", srv.Client())
	serve := func() {
		mu.Lock()
		handler = (&fed.Server{ID: B.a.ID, Handler: B.a, Log: B.a.Log, Now: time.Now}).Routes()
		mu.Unlock()
	}
	serve()
	return A, B, func() {
		id, _, err := fed.LoadOrCreateIdentity(t.TempDir(), host)
		if err != nil {
			t.Fatal(err)
		}
		B.a.ID = id
		serve()
	}
}

func sync2(a, b *inst) {
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		a.a.backoff = map[string]peerBackoff{}
		b.a.backoff = map[string]peerBackoff{}
		a.a.PushOnce(ctx)
		b.a.PushOnce(ctx)
		a.a.PollOnce(ctx)
	}
}

func TestFederation(t *testing.T) {
	ctx := context.Background()
	A, B, _ := fedPair(t)
	aG := A.guild(t, g1, "Home Server", "400000000000000001")
	bG := B.guild(t, g2, "VPS Server", "400000000000000002")

	// Both operators add each other.
	if _, err := A.a.AddPeer(ctx, B.a.Self()); err != nil {
		t.Fatal(err)
	}
	// Before B allows A, A's messages are refused and dropped.
	must(A.a.Subscribe(ctx, aG, g2+"@"+B.a.Self()))
	sync2(A, B)
	if len(B.dc.requests("chan"+g2)) != 0 {
		t.Fatal("B accepted a message from a peer not on its allowlist")
	}
	must(A.a.Unsubscribe(ctx, aG, g2+"@"+B.a.Self()))

	must(B.a.AddPeer(ctx, A.a.Self()))
	must(A.a.Subscribe(ctx, aG, g2+"@"+B.a.Self()))
	sync2(A, B)
	if len(B.dc.requests("chan"+g2)) != 1 {
		t.Fatal("B did not get the subscribe request")
	}
	approveAll(t, B, bG, DecideFollow)
	sync2(A, B)
	sub := must(A.a.St.GetSub(ctx, g2, B.a.Self(), g1, A.a.Self()))
	if sub == nil || sub.Status != store.SubActive {
		t.Fatalf("A's subscription not active: %+v", sub)
	}
	// Follow back: A gets a card for B following A.
	if len(A.dc.requests("chan"+g1)) != 1 {
		t.Fatal("follow-back request did not reach A")
	}
	approveAll(t, A, aG, DecideApprove)
	sync2(A, B)

	// B alerts; A (outbound-only) gets it by polling.
	A.dc.members[g1+"/"+tA] = true
	id := alertFrom(t, B, bG, tA, "phishing links", png("EVIDENCE"))
	sync2(A, B)
	ac := A.dc.cards("chan" + g1)
	if len(ac) != 1 {
		t.Fatalf("A got %d cards", len(ac))
	}
	txt := cardText(ac[0])
	if !strings.Contains(txt, "via `"+B.a.Self()+"`") || strings.Contains(txt, "Origin not verified") || !strings.Contains(txt, "In this server: Yes") {
		t.Fatalf("A card:\n%s", txt)
	}
	if len(ac[0].files) != 1 || ac[0].files[0] != "shot.png:"+string(png("EVIDENCE")) {
		t.Fatalf("evidence did not travel: %v", ac[0].files)
	}

	// The same alert arriving twice is dropped at the receiving end.
	al := must(B.a.St.GetAlert(ctx, id))
	msg, typ, _ := B.a.alertMessage(ctx, al, nil)
	msg.Targets = []string{g1}
	B.a.enqueue(ctx, A.a.Self(), typ, msg)
	sync2(A, B)
	if n := len(A.dc.cards("chan" + g1)); n != 1 {
		t.Fatalf("duplicate delivered: %d", n)
	}

	// A alerts back to B through B's inbox.
	B.dc.members[g2+"/500000000000000777"] = false
	id2 := alertFrom(t, A, aG, "500000000000000777", "raid account", nil)
	sync2(A, B)
	bc := B.dc.cards("chan" + g2)
	if len(bc) != 1 || !strings.Contains(cardText(bc[0]), "via `"+A.a.Self()+"`") || strings.Contains(cardText(bc[0]), "not verified") {
		t.Fatalf("B card: %v", bc)
	}
	_ = id2

	// Retract from B reaches A's copy.
	must(B.a.Retract(ctx, bG, id))
	sync2(A, B)
	if !strings.HasPrefix(A.dc.cards("chan" + g1)[0].embeds[0].Title, "RETRACTED") {
		t.Fatal("retraction did not reach A")
	}

	// B unsubscribes from A: A stops sending.
	must(B.a.Unsubscribe(ctx, bG, g1+"@"+A.a.Self()))
	sync2(A, B)
	if subs := must(A.a.St.ActiveSubscribers(ctx, g1, A.a.Self())); len(subs) != 0 {
		t.Fatalf("A still sends to B: %v", subs)
	}
}

func TestForgedAndChangedKeys(t *testing.T) {
	ctx := context.Background()
	A, B, rekeyB := fedPair(t)
	aG := A.guild(t, g1, "Home", "400000000000000001")
	bG := B.guild(t, g2, "VPS", "400000000000000002")
	must(A.a.AddPeer(ctx, B.a.Self()))
	must(B.a.AddPeer(ctx, A.a.Self()))
	must(A.a.Subscribe(ctx, aG, g2+"@"+B.a.Self()))
	sync2(A, B)
	approveAll(t, B, bG, DecideApprove)
	sync2(A, B)

	// An alert whose content was rewritten after signing is rejected.
	id := alertFrom(t, B, bG, tA, "spam", nil)
	al, _ := B.a.St.GetAlert(ctx, id)
	forged := strings.Replace(string(al.Signed), `"reason":"spam"`, `"reason":"ban him"`, 1)
	m := fed.AlertMessage{Alert: fed.SignedAlert{Alert: []byte(forged), Sig: al.Sig}, Targets: []string{g1}}
	env := B.a.ID.Seal(A.a.Self(), fed.TypeAlert, mustJSON(t, m), time.Now())
	if err := A.a.handlePolled(ctx, B.a.Self(), env); err == nil || !strings.Contains(err.Error(), "origin signature") {
		t.Fatalf("forged alert: %v", err)
	}

	// Anyone can sign an envelope claiming to be B. That must not get the
	// real B blocked, and must not DM the owner.
	imposter, _, _ := fed.LoadOrCreateIdentity(t.TempDir(), B.a.Self())
	env = imposter.Seal(A.a.Self(), fed.TypeAlert, mustJSON(t, m), time.Now())
	pub, err := fed.VerifyEnvelope(env, A.a.Self(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := A.a.Authenticate(ctx, env, pub); err == nil {
		t.Fatal("imposter accepted")
	}
	if inst := must(A.a.St.GetInstance(ctx, B.a.Self())); inst.Status != store.PeerAllowed {
		t.Fatalf("an imposter changed B's status to %s", inst.Status)
	}
	if len(A.dc.dms[owner]) != 0 {
		t.Fatal("an imposter triggered a key-change DM")
	}

	// B really changes its key (its address now publishes the new one):
	// that blocks B until the operator decides, and the owner hears of it.
	// The published-key check is cached for a minute, so move A's clock on.
	A.a.Now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	rekeyB()
	env = B.a.ID.Seal(A.a.Self(), fed.TypeAlert, mustJSON(t, m), time.Now())
	pub, _ = fed.VerifyEnvelope(env, A.a.Self(), time.Now())
	if err := A.a.Authenticate(ctx, env, pub); err == nil {
		t.Fatal("changed key accepted")
	}
	inst := must(A.a.St.GetInstance(ctx, B.a.Self()))
	if inst.Status != store.PeerKeyChanged || inst.PendingKey != B.a.ID.PubB64() {
		t.Fatalf("status %s", inst.Status)
	}
	if len(A.dc.dms[owner]) != 1 || !strings.Contains(A.dc.dms[owner][0].Content, "different key") {
		t.Fatalf("owner DMs: %v", A.dc.dms[owner])
	}
	// An imposter cannot swap in its own key as the pending one.
	env = imposter.Seal(A.a.Self(), fed.TypeAlert, mustJSON(t, m), time.Now())
	pub, _ = fed.VerifyEnvelope(env, A.a.Self(), time.Now())
	A.a.Authenticate(ctx, env, pub)
	if inst := must(A.a.St.GetInstance(ctx, B.a.Self())); inst.PendingKey != B.a.ID.PubB64() || len(A.dc.dms[owner]) != 1 {
		t.Fatal("imposter changed the pending key or sent another DM")
	}
	must(A.a.DecideKey(ctx, B.a.Self(), true))
	env = B.a.ID.Seal(A.a.Self(), fed.TypeUnsubscribe, []byte(`{"side":"x"}`), time.Now())
	pub, _ = fed.VerifyEnvelope(env, A.a.Self(), time.Now())
	if err := A.a.Authenticate(ctx, env, pub); err != nil {
		t.Fatalf("approved key refused: %v", err)
	}

	// The HTTP inbox refuses stale envelopes.
	stale := A.a.ID.Seal(B.a.Self(), fed.TypeUnsubscribe, []byte(`{}`), time.Now().Add(-10*time.Minute))
	if err := A.a.Fed.Push(ctx, stale); err == nil {
		t.Fatal("stale envelope accepted")
	}
}

// A third instance C originates an alert that B forwards to A. A cannot
// fetch C's key, so the origin is unverified until C is pinned.
func TestUnverifiedOriginsHopsAndCollisions(t *testing.T) {
	ctx := context.Background()
	A, B, _ := fedPair(t)
	aG := A.guild(t, g1, "Home", "400000000000000001")
	bG := B.guild(t, g2, "VPS", "400000000000000002")
	must(A.a.AddPeer(ctx, B.a.Self()))
	must(B.a.AddPeer(ctx, A.a.Self()))
	must(A.a.Subscribe(ctx, aG, g2+"@"+B.a.Self()))
	sync2(A, B)
	approveAll(t, B, bG, DecideApprove)
	sync2(A, B)

	C, _, _ := fed.LoadOrCreateIdentity(t.TempDir(), "c.invalid")
	A.a.keyMemo["c.invalid"] = keyMemo{at: time.Now()} // as if fetching C's key just failed
	const gC, gD = "110000000000000077", "110000000000000088"
	alertID := "0b4d2c5e-8f1a-4c3b-9d2e-7a6f5e4d3c2b"
	mk := func(signer *fed.Identity, reason string) fed.SignedAlert {
		sa, err := signer.SignAlert(&fed.Alert{AlertID: alertID, OriginGuild: gC, OriginGuildName: "C Server", OriginHost: "c.invalid",
			SenderID: "300000000000000077", SenderName: "cmod", TargetID: tA, TargetName: "scammer", Reason: reason, CreatedAt: time.Now().Unix()})
		if err != nil {
			t.Fatal(err)
		}
		return *sa
	}
	send := func(sa fed.SignedAlert) error {
		// A made-up hop for a server on D, which nobody can check, then B's own hop.
		fake := fed.Hop{AlertID: alertID, N: 1, GuildID: gD, GuildName: "D Server", Host: "d.invalid", ByID: "300000000000000088", ByName: "dmod", At: 1, Sig: "AAAA"}
		bh := fed.Hop{AlertID: alertID, N: 2, GuildID: g2, GuildName: "VPS", Host: B.a.Self(), ByID: "300000000000000002", ByName: "bmod", At: 2}
		B.a.ID.SignHop(&bh)
		m := fed.AlertMessage{Alert: sa, Hops: []fed.Hop{fake, bh}, Targets: []string{g1}}
		return A.a.handlePolled(ctx, B.a.Self(), B.a.ID.Seal(A.a.Self(), fed.TypeAlertForward, mustJSON(t, m), time.Now()))
	}

	imposter, _, _ := fed.LoadOrCreateIdentity(t.TempDir(), "")
	if err := send(mk(imposter, "FAKE claim")); err != nil {
		t.Fatal(err)
	}
	card := A.dc.cards("chan" + g1)[0]
	txt := cardText(card)
	for _, want := range []string{"Origin not verified", "FAKE claim", "@dmod in D Server via `d.invalid` (not verified)", "@bmod in VPS via"} {
		if !strings.Contains(txt, want) {
			t.Errorf("card missing %q:\n%s", want, txt)
		}
	}
	if strings.Contains(txt, "@bmod in VPS via `"+B.a.Self()+"` (not verified)") {
		t.Error("B's own hop shown as unverified")
	}

	// A second, different unverified version of the same ID is refused.
	if err := send(mk(imposter, "another story")); err == nil || !strings.Contains(err.Error(), "different content") {
		t.Fatalf("collision: %v", err)
	}

	// Once C's key is known, C's real alert replaces the unverified one.
	A.a.St.UpsertInstance(ctx, store.Instance{Host: "c.invalid", PublicKey: C.PubB64(), Reach: store.ReachReachable, Status: store.PeerAllowed, FirstSeen: 1})
	if err := send(mk(C, "real reason")); err != nil {
		t.Fatal(err)
	}
	txt = cardText(A.dc.cards("chan" + g1)[0])
	if strings.Contains(txt, "Origin not verified") || !strings.Contains(txt, "real reason") || strings.Contains(txt, "FAKE") {
		t.Fatalf("card after the verified copy:\n%s", txt)
	}
	if n := len(A.dc.cards("chan" + g1)); n != 1 {
		t.Fatalf("%d cards", n)
	}
	// And the forgery cannot come back.
	if err := send(mk(imposter, "FAKE claim")); err == nil {
		t.Fatal("forgery accepted after the real alert")
	}

	// Non-canonical IDs and bad hosts are refused outright.
	bad := mk(C, "x")
	bad.Alert = []byte(strings.Replace(string(bad.Alert), alertID, strings.ToUpper(alertID), 1))
	if err := send(bad); err == nil {
		t.Fatal("uppercase alert ID accepted")
	}
}

func TestEvidenceRules(t *testing.T) {
	ctx := context.Background()
	in := newInst(t, config.ModeOff, "", nil)
	a1 := in.guild(t, g1, "A", "400000000000000001")
	att := &AttachmentIn{Name: "evil.png.exe", Size: 10, URL: "https://cdn.discordapp.com/x"}

	in.a.FetchEvidence = func(context.Context, string, int64) ([]byte, error) { return []byte("MZ\x90\x00\x03\x00\x00\x00"), nil }
	if _, err := in.a.PrepareAlert(ctx, a1, "chan"+g1, tA, "x", "", att); err == nil || !strings.Contains(Friendly(err), "Evidence has to be") {
		t.Fatalf("executable accepted: %v", err)
	}
	in.a.FetchEvidence = func(context.Context, string, int64) ([]byte, error) { return png("x"), nil }
	d := must(in.a.PrepareAlert(ctx, a1, "chan"+g1, tA, "x", "", att))
	if d.EvidenceName != "evil.png" || d.EvidenceType != "image/png" {
		t.Fatalf("name %q type %q", d.EvidenceName, d.EvidenceType)
	}
	if _, err := downloadFromCDN(ctx, "https://evil.example/a.png", 100); err == nil {
		t.Fatal("non-CDN download allowed")
	}

	// Display fields from a peer are checked before they are shown.
	row := alertRow(&fed.Alert{AlertID: "x", TargetAvatar: "https://tracker.example/p.png", Link: "https://evil.example/[x](y)"}, &fed.SignedAlert{}, false)
	if row.TargetAvatar != "" || row.EvidenceLink != "" {
		t.Fatalf("avatar %q link %q", row.TargetAvatar, row.EvidenceLink)
	}
}

func TestMarkdownAndDrafts(t *testing.T) {
	ctx := context.Background()
	in := newInst(t, config.ModeOff, "", nil)
	a1 := in.guild(t, g1, "[Discord Staff](https://evil.example)", "400000000000000001")
	b1 := in.guild(t, g2, "B", "400000000000000002")
	must(in.a.Subscribe(ctx, b1, g1))
	approveAll(t, in, a1, DecideApprove)

	// A second /alert from the same mod replaces the first draft.
	first := must(in.a.PrepareAlert(ctx, a1, "chan"+g1, tA, "one", "", nil))
	second := must(in.a.PrepareAlert(ctx, a1, "chan"+g1, tA, "**everyone** <@&123> [link](https://evil.example)", "", nil))
	if _, err := in.a.SendDraft(ctx, first.ID, a1); err == nil {
		t.Fatal("replaced draft still sendable")
	}
	someone := a1
	someone.UserID = "300000000000000999"
	if _, err := in.a.SendDraft(ctx, second.ID, someone); err == nil {
		t.Fatal("another user sent my draft")
	}
	must(in.a.SendDraft(ctx, second.ID, a1))

	txt := cardText(in.dc.cards("chan" + g2)[0])
	for _, bad := range []string{"[Discord Staff](", "[link](", "**everyone**", "<@&123>"} {
		if strings.Contains(txt, bad) {
			t.Errorf("unescaped %q in:\n%s", bad, txt)
		}
	}
	if !strings.Contains(txt, `\[link\]\(https://evil.example\)`) {
		t.Errorf("escaped form missing:\n%s", txt)
	}
}

func TestRequestFloodCap(t *testing.T) {
	ctx := context.Background()
	in := newInst(t, config.ModeOff, "", nil)
	in.guild(t, g1, "Target", "400000000000000001")
	var err error
	for i := 0; i < maxPendingRequests+1; i++ {
		g := in.guild(t, fmt.Sprintf("1200000000000000%02d", i), fmt.Sprint("S", i), "400000000000000009")
		_, err = in.a.Subscribe(ctx, g, g1)
	}
	if err == nil || !strings.Contains(Friendly(err), "too many unanswered") {
		t.Fatalf("request %d: %v", maxPendingRequests+1, err)
	}
	if n := len(in.dc.requests("chan" + g1)); n != maxPendingRequests {
		t.Fatalf("%d request cards", n)
	}
}

func TestOpenModePeerLimit(t *testing.T) {
	in := newInst(t, config.ModeOff, "", nil)
	for i := 0; i < maxNewPeersPerHour; i++ {
		if !in.a.allowNewPeer() {
			t.Fatalf("refused peer %d", i)
		}
	}
	if in.a.allowNewPeer() {
		t.Fatal("limit not enforced")
	}
	in.a.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if !in.a.allowNewPeer() {
		t.Fatal("limit did not reset")
	}
}

func TestOffModeRefusesFederation(t *testing.T) {
	ctx := context.Background()
	in := newInst(t, config.ModeOff, "", nil)
	a1 := in.guild(t, g1, "A", "400000000000000001")
	if _, err := in.a.Subscribe(ctx, a1, g2+"@alerts.mydomain.net"); err == nil || !strings.Contains(Friendly(err), "does not federate") {
		t.Fatalf("got %v", err)
	}
	if _, err := in.a.AddPeer(ctx, "alerts.mydomain.net"); err == nil {
		t.Fatal("peer add allowed in off mode")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
