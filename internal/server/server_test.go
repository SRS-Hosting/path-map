package server

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/USA-RedDragon/path-map/internal/config"
	"github.com/USA-RedDragon/rcon/rcontest"
)

const waitDeadline = 5 * time.Second

// The canned PlayerInfoAll spans three pages under one key — the series-wide
// key the game actually uses — tearing one record's "Location" label across
// the 1/2 seam and rex's species across the 2/3 seam. A server test that sees
// both players whole has exercised pagination reassembly, the tolerant
// parser, and the projection end to end.
const (
	playersPage1 = "[Page(Key 7) 1/3]Total Players: 2.\n(PlayerInfo kittykat95): Name: kittykat95 / AGID: 746-132-258 / Dinosaur: Hatzegopteryx / Role: None / Marks: 2715 / Growth: 1 / Loc"
	playersPage2 = "[Page(Key 7) 2/3]ation: (X=-67904.590 Y=-237666.790 Z=-297.420)\n(PlayerInfo rex): Name: rex / AGID: 111-222-333 / Dinosaur: Tyrannosau"
	playersPage3 = "[Page(Key 7) 3/3]rus / Role: None / Marks: 10 / Growth: 0.75 / Location: (X=0.0 Y=0.0 Z=12.0)"
)

const gondwaPOIBody = "(ListPOI): Impact Crater, Grand Plains, Titan's Pass, Snake Gully, Savanna Grassland, Salt Flats, Burned Forest, Red Island"

// fakeGame fakes the paginated game server. down makes new connections fail
// authentication, which is how an outage is simulated: the client dials fresh
// per command, so refusing auth is indistinguishable from a crashed server
// without tearing the listener down. The mutex guards against rcontest's
// per-connection goroutines.
type fakeGame struct {
	mu   sync.Mutex
	down bool
	// partial kills page fetches the way an expired or connection-scoped
	// page key would: the follow-up request gets a header-less reply.
	partial  bool
	commands []string
}

func newTestRCON(t *testing.T) (*fakeGame, *rcontest.Server) {
	t.Helper()
	fg := &fakeGame{}
	srv, err := rcontest.New(fg.handle)
	if err != nil {
		t.Fatalf("rcontest.New: %v", err)
	}
	t.Cleanup(srv.Close)
	return fg, srv
}

// handle answers one connection: an auth refusal while down, the normal
// scripted session otherwise.
func (fg *fakeGame) handle(f *rcontest.Framer) {
	fg.mu.Lock()
	down := fg.down
	fg.mu.Unlock()

	if down {
		if _, err := f.Read(); err != nil {
			return
		}
		// A failed authentication is signalled by ID -1.
		_ = f.Write(rcontest.TypeAuthResponse, rcontest.AuthFailedID, "")
		return
	}
	rcontest.Respond("pw", 0, fg.respond)(f)
}

func (fg *fakeGame) respond(command string) string {
	fg.mu.Lock()
	fg.commands = append(fg.commands, command)
	partial := fg.partial
	fg.mu.Unlock()

	switch command {
	case "PlayerInfoAll":
		return playersPage1
	case "Page:7-2":
		if partial {
			return "Could not find pages for key"
		}
		return playersPage2
	case "Page:7-3":
		return playersPage3
	case "ListPOI":
		return gondwaPOIBody
	default:
		return "That command does not exist"
	}
}

func (fg *fakeGame) setPartial(partial bool) {
	fg.mu.Lock()
	defer fg.mu.Unlock()
	fg.partial = partial
}

func (fg *fakeGame) setDown(down bool) {
	fg.mu.Lock()
	defer fg.mu.Unlock()
	fg.down = down
}

func (fg *fakeGame) playerPolls() int {
	fg.mu.Lock()
	defer fg.mu.Unlock()
	n := 0
	for _, c := range fg.commands {
		if c == "PlayerInfoAll" {
			n++
		}
	}
	return n
}

func splitAddr(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split %q: %v", addr, err)
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		t.Fatalf("parse port %q: %v", portStr, err)
	}
	return host, int(port)
}

var testImageBytes = []byte("not really a png, but bytes are bytes") //nolint:gochecknoglobals // test fixture

// writeTestImage writes a stand-in map image and returns its path. The server
// never decodes the image — it serves bytes — so the content is arbitrary.
func writeTestImage(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gondwa.png")
	if err := os.WriteFile(path, testImageBytes, 0o600); err != nil {
		t.Fatalf("write test image: %v", err)
	}
	return path
}

func testConfig(t *testing.T, rconAddr string) *config.Config {
	t.Helper()
	host, port := splitAddr(t, rconAddr)
	return &config.Config{
		LogLevel: config.LogLevelInfo,
		HTTP:     config.HTTP{Bind: "127.0.0.1", Port: 8080},
		RCON:     config.RCON{Host: host, Port: port, Password: "pw", TimeoutSeconds: 5, MaxConcurrent: 4},
		Map:      config.Map{Name: "gondwa", ImagePath: writeTestImage(t)},
		Poller:   config.Poller{IntervalSeconds: 1, IdleAfterSeconds: 2},
	}
}

func newTestServer(t *testing.T, cfg *config.Config) *Server {
	t.Helper()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("test config is invalid: %v", err)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// startPoller runs the server's poller for the in-process tests that never
// call Run.
func startPoller(t *testing.T, s *Server) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.poller.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

func getRec(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func getPlayers(t *testing.T, s *Server) (int, playersResponse) {
	t.Helper()
	rec := getRec(t, s, "/api/players")
	var got playersResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	return rec.Code, got
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitDeadline)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// get exists so network tests go through http.NewRequestWithContext: a test
// that hangs should fail on its own deadline, not wait forever.
func get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(req)
}

func closeBody(t *testing.T, resp *http.Response) {
	t.Helper()
	if err := resp.Body.Close(); err != nil {
		t.Errorf("close body: %v", err)
	}
}

func waitUp(t *testing.T, url string) {
	t.Helper()
	var err error
	for range 200 {
		var resp *http.Response
		resp, err = get(context.Background(), url)
		if err == nil {
			closeBody(t, resp)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server never came up at %s: %v", url, err)
}

func TestRoutes(t *testing.T) {
	_, rs := newTestRCON(t)
	s := newTestServer(t, testConfig(t, rs.Addr()))

	rec := getRec(t, s, "/")
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "Player Map") {
		t.Fatalf("index: %d", rec.Code)
	}
	if !strings.Contains(body, rs.Addr()) {
		t.Fatal("page does not name the target server")
	}
	// html/template pads values injected into a script context with spaces.
	if !strings.Contains(body, "INTERVAL_MS =  1  * 1000") {
		t.Fatal("poll interval was not injected into the page script")
	}
	if !strings.Contains(body, "ABORT_MS =  5  * 1000") {
		t.Fatal("timeout was not injected into the page script")
	}
	// The page must carry no external references, or it will not work
	// offline. The map image itself is loaded by JS assigning img.src, so
	// even "src=" stays banned in the markup.
	for _, bad := range []string{"http://", "https://", "src=", "href=", "@import", "url(", "@font-face", "integrity=", "crossorigin"} {
		if strings.Contains(body, bad) {
			t.Errorf("page references something external: %q", bad)
		}
	}

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/nope"},
		{http.MethodGet, "/api/command"},
		{http.MethodPost, "/"},
		{http.MethodPost, "/api/players"},
		{http.MethodPost, "/map.png"},
	} {
		rec := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code == http.StatusOK {
			t.Errorf("%s %s unexpectedly returned 200", tc.method, tc.path)
		}
		t.Logf("%s %s -> %d", tc.method, tc.path, rec.Code)
	}
}

func TestSecurityHeaders(t *testing.T) {
	_, rs := newTestRCON(t)
	s := newTestServer(t, testConfig(t, rs.Addr()))

	rec := getRec(t, s, "/")
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("CSP = %q, want default-src 'none'", csp)
	}
	// The one grant beyond rcon-web's policy: the page loads /map.png.
	if !strings.Contains(csp, "img-src 'self'") {
		t.Errorf("CSP = %q, want img-src 'self'", csp)
	}
	for header, want := range map[string]string{
		"X-Frame-Options":        "DENY",
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "no-referrer",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
}

func TestPlayersEndpointPending(t *testing.T) {
	_, rs := newTestRCON(t)
	s := newTestServer(t, testConfig(t, rs.Addr()))
	// No poller is running: this is the page's first paint, pre-poll.

	code, got := getPlayers(t, s)
	if code != http.StatusOK {
		t.Fatalf("pending state must still be 200, got %d", code)
	}
	if got.GeneratedAt != nil {
		t.Errorf("generatedAt = %v before any poll", got.GeneratedAt)
	}
	// An explicitly configured map is known before any RCON traffic.
	if got.Map == nil || got.Map.Name != "gondwa" {
		t.Errorf("map = %+v, want the configured map", got.Map)
	}

	raw := getRec(t, s, "/api/players").Body.String()
	if !strings.Contains(raw, `"players":[]`) {
		t.Errorf("players must serialise as [] pre-poll, got %s", raw)
	}
	if !strings.Contains(raw, `"generatedAt":null`) {
		t.Errorf("generatedAt must serialise as null pre-poll, got %s", raw)
	}
}

func TestPlayersEndToEnd(t *testing.T) {
	_, rs := newTestRCON(t)
	s := newTestServer(t, testConfig(t, rs.Addr()))
	startPoller(t, s)

	var got playersResponse
	waitFor(t, "a snapshot with both players", func() bool {
		_, got = getPlayers(t, s)
		return len(got.Players) == 2
	})

	if !got.Complete || got.Total != 2 {
		t.Errorf("complete = %v, total = %d", got.Complete, got.Total)
	}
	if got.GeneratedAt == nil {
		t.Error("generatedAt is null after a successful poll")
	}

	kitty := got.Players[0]
	if kitty.Name != "kittykat95" || kitty.AGID != "746-132-258" || kitty.Dinosaur != "Hatzegopteryx" {
		t.Errorf("first player = %+v", kitty)
	}
	// The "Location" label was torn across the 1/2 page seam; the coordinates
	// must have survived reassembly and projection.
	if !kitty.HasPosition {
		t.Fatal("kittykat95 lost her position to the page seam")
	}
	if kitty.U < 0.41 || kitty.U > 0.42 || kitty.V < 0.20 || kitty.V > 0.21 {
		t.Errorf("kittykat95 projected to (%v, %v), want (~0.416, ~0.206)", kitty.U, kitty.V)
	}

	// rex's species was torn across the 2/3 seam, and he stands at the world
	// origin, which is the exact centre of the map image.
	rex := got.Players[1]
	if rex.Dinosaur != "Tyrannosaurus" {
		t.Errorf("rex.Dinosaur = %q: the page seam split was not reassembled", rex.Dinosaur)
	}
	if rex.U != 0.5 || rex.V != 0.5 {
		t.Errorf("rex projected to (%v, %v), want the exact centre", rex.U, rex.V)
	}
}

func TestPlayersStaleAfterFailure(t *testing.T) {
	fg, rs := newTestRCON(t)
	s := newTestServer(t, testConfig(t, rs.Addr()))
	startPoller(t, s)

	var healthy playersResponse
	waitFor(t, "a healthy snapshot", func() bool {
		_, healthy = getPlayers(t, s)
		return len(healthy.Players) == 2
	})

	fg.setDown(true)
	var got playersResponse
	waitFor(t, "the failure to surface", func() bool {
		_, got = getPlayers(t, s)
		return got.Error != ""
	})

	if len(got.Players) != 2 {
		t.Errorf("stale snapshot lost its players: %d", len(got.Players))
	}
	if got.GeneratedAt == nil || !got.GeneratedAt.Equal(*healthy.GeneratedAt) {
		t.Errorf("generatedAt changed during the outage: %v vs %v", got.GeneratedAt, healthy.GeneratedAt)
	}
}

// TestPlayersPartialAfterPageLoss is the whole degradation story end to end:
// a healthy two-player map loses its second page (expired key, or a key that
// did not survive the connection boundary) and must degrade to a fresh,
// labelled, incomplete roster of what page 1 still carries — never freeze on
// the old snapshot, never present the shortfall as a healthy small server.
func TestPlayersPartialAfterPageLoss(t *testing.T) {
	fg, rs := newTestRCON(t)
	s := newTestServer(t, testConfig(t, rs.Addr()))
	startPoller(t, s)

	var healthy playersResponse
	waitFor(t, "a healthy snapshot", func() bool {
		_, healthy = getPlayers(t, s)
		return len(healthy.Players) == 2
	})

	fg.setPartial(true)
	var got playersResponse
	waitFor(t, "the partial to surface", func() bool {
		_, got = getPlayers(t, s)
		return got.Error != "" && len(got.Players) == 1
	})

	if got.Complete {
		t.Error("partial roster claims to be complete")
	}
	if got.Total != 2 {
		t.Errorf("total = %d, want the game's own count of 2", got.Total)
	}
	// kittykat95 is entirely on page 1 except her coordinates, which were on
	// the lost page: she stays listed, just not drawn.
	if got.Players[0].Name != "kittykat95" || got.Players[0].HasPosition {
		t.Errorf("partial player = %+v, want kittykat95 without a position", got.Players[0])
	}
	// Partials are fresh data, not preserved history.
	if got.GeneratedAt == nil || !got.GeneratedAt.After(*healthy.GeneratedAt) {
		t.Errorf("partial generatedAt %v is not fresher than %v", got.GeneratedAt, healthy.GeneratedAt)
	}

	// And the map heals when the pages come back.
	fg.setPartial(false)
	waitFor(t, "recovery to the full roster", func() bool {
		_, got = getPlayers(t, s)
		return got.Error == "" && len(got.Players) == 2 && got.Complete
	})
}

func TestIdlePollingStops(t *testing.T) {
	fg, rs := newTestRCON(t)
	cfg := testConfig(t, rs.Addr())
	s := newTestServer(t, cfg)
	startPoller(t, s)

	waitFor(t, "the first snapshot", func() bool {
		_, got := getPlayers(t, s)
		return got.GeneratedAt != nil
	})

	// Stop asking. Once the idle window passes, the game must hear nothing:
	// this quiet is the whole point of demand-driven polling.
	time.Sleep(cfg.Poller.IdleAfter() + cfg.Poller.Interval())
	before := fg.playerPolls()
	time.Sleep(3 * cfg.Poller.Interval())
	if after := fg.playerPolls(); after != before {
		t.Errorf("polling continued while idle: %d -> %d", before, after)
	}
}

func TestMapImage(t *testing.T) {
	_, rs := newTestRCON(t)
	s := newTestServer(t, testConfig(t, rs.Addr()))

	rec := getRec(t, s, "/map.png")
	if rec.Code != http.StatusOK {
		t.Fatalf("map image: %d", rec.Code)
	}
	if rec.Body.String() != string(testImageBytes) {
		t.Error("served image differs from the file on disk")
	}
	if got := rec.Header().Get("Content-Type"); got != "image/png" {
		t.Errorf("Content-Type = %q", got)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on the map image")
	}

	req := httptest.NewRequest(http.MethodGet, "/map.png", nil)
	req.Header.Set("If-None-Match", etag)
	rec = httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Errorf("revalidation got %d, want 304", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("304 carried %d body bytes", rec.Body.Len())
	}
}

func TestMissingMapImage(t *testing.T) {
	_, rs := newTestRCON(t)
	cfg := testConfig(t, rs.Addr())
	cfg.Map.ImagePath = filepath.Join(t.TempDir(), "missing.png")

	if _, err := New(cfg); err == nil {
		t.Fatal("New accepted a missing map image")
	} else if !strings.Contains(err.Error(), "missing.png") {
		t.Errorf("error %q does not name the path", err)
	}
}

func TestMapImageDirExplicitName(t *testing.T) {
	_, rs := newTestRCON(t)
	cfg := testConfig(t, rs.Addr())
	cfg.Map.ImagePath = filepath.Dir(writeTestImage(t))

	s := newTestServer(t, cfg)
	if rec := getRec(t, s, "/map.png"); rec.Code != http.StatusOK || rec.Body.String() != string(testImageBytes) {
		t.Errorf("directory-mode image: %d", rec.Code)
	}

	// An explicit map whose image is absent from the directory is a broken
	// deployment, caught at startup rather than at first page load.
	cfg = testConfig(t, rs.Addr())
	cfg.Map.Name = "panjura"
	cfg.Map.ImagePath = filepath.Dir(writeTestImage(t))
	if _, err := New(cfg); err == nil {
		t.Fatal("New accepted a directory without the named map's image")
	}
}

func TestMapImageUndetected(t *testing.T) {
	_, rs := newTestRCON(t)
	cfg := testConfig(t, rs.Addr())
	cfg.Map.Name = config.MapNameAuto
	cfg.Map.ImagePath = filepath.Dir(writeTestImage(t))
	s := newTestServer(t, cfg)

	// Undetected: the image endpoint cannot know which file to serve.
	if rec := getRec(t, s, "/map.png"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("undetected map image: %d, want 503", rec.Code)
	}
	if _, got := getPlayers(t, s); got.Map != nil {
		t.Errorf("map = %+v before detection, want null", got.Map)
	}

	startPoller(t, s)
	waitFor(t, "detection", func() bool {
		_, got := getPlayers(t, s)
		return got.Map != nil
	})
	if rec := getRec(t, s, "/map.png"); rec.Code != http.StatusOK || rec.Body.String() != string(testImageBytes) {
		t.Errorf("post-detection image: %d", rec.Code)
	}
}

func TestAutoDetectEndToEnd(t *testing.T) {
	_, rs := newTestRCON(t)
	cfg := testConfig(t, rs.Addr())
	cfg.Map.Name = config.MapNameAuto
	cfg.Map.ImagePath = filepath.Dir(writeTestImage(t))
	s := newTestServer(t, cfg)
	startPoller(t, s)

	var got playersResponse
	waitFor(t, "detection and a snapshot", func() bool {
		_, got = getPlayers(t, s)
		return got.Map != nil && len(got.Players) == 2
	})
	if got.Map.Name != "gondwa" || got.Map.DisplayName != "Gondwa" {
		t.Errorf("detected map = %+v", got.Map)
	}
	if u := got.Players[0].U; u < 0.41 || u > 0.42 {
		t.Errorf("player u = %v: detected extents were not applied", u)
	}
}

// TestAutoDetectMissingImageDirFailsFast: an auto-mode directory containing
// none of the known map images can never serve anything, so New refuses it.
func TestAutoDetectMissingImageDirFailsFast(t *testing.T) {
	_, rs := newTestRCON(t)
	cfg := testConfig(t, rs.Addr())
	cfg.Map.Name = config.MapNameAuto
	cfg.Map.ImagePath = t.TempDir()
	if _, err := New(cfg); err == nil {
		t.Fatal("New accepted an image directory with no known map images")
	}
}

func TestRunStopsPoller(t *testing.T) {
	fg, rs := newTestRCON(t)
	cfg := testConfig(t, rs.Addr())
	cfg.HTTP.Port = 18094
	s := newTestServer(t, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	waitUp(t, "http://127.0.0.1:18094/")

	resp, err := get(context.Background(), "http://127.0.0.1:18094/api/players")
	if err != nil {
		t.Fatalf("GET /api/players: %v", err)
	}
	closeBody(t, resp)
	waitFor(t, "the poller to have polled", func() bool {
		return fg.playerPolls() > 0
	})

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(waitDeadline):
		t.Fatal("Run did not return after cancellation")
	}

	// Run has returned, so the poller must be gone with it: the counter can
	// no longer move even across several would-be intervals.
	before := fg.playerPolls()
	time.Sleep(2 * cfg.Poller.Interval())
	if after := fg.playerPolls(); after != before {
		t.Errorf("poller outlived Run: %d -> %d", before, after)
	}
}

// TestDualStack is the point of the explicit two-listener setup: the same port
// must answer over both IPv4 and IPv6 loopback.
//
// It skips where IPv6 is unavailable rather than failing. The production code
// deliberately degrades to one family there, so failing would contradict the
// behaviour the rest of this file asserts.
func TestDualStack(t *testing.T) {
	var probe net.ListenConfig
	if ln, err := probe.Listen(context.Background(), "tcp6", "[::1]:0"); err != nil {
		t.Skipf("no IPv6 loopback on this host: %v", err)
	} else {
		_ = ln.Close()
	}

	_, rs := newTestRCON(t)
	cfg := testConfig(t, rs.Addr())
	cfg.HTTP.Bind = ""
	cfg.HTTP.Port = 18093
	s := newTestServer(t, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
	})

	waitUp(t, "http://127.0.0.1:18093/")

	for _, url := range []string{"http://127.0.0.1:18093/", "http://[::1]:18093/"} {
		resp, err := get(context.Background(), url)
		if err != nil {
			t.Fatalf("GET %s: %v", url, err)
		}
		closeBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %d", url, resp.StatusCode)
		}
		t.Logf("GET %s -> %d", url, resp.StatusCode)
	}
}

func TestGracefulShutdown(t *testing.T) {
	_, rs := newTestRCON(t)
	cfg := testConfig(t, rs.Addr())
	cfg.HTTP.Port = 18092
	s := newTestServer(t, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	waitUp(t, "http://127.0.0.1:18092/")

	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
		t.Logf("shut down cleanly in %s", time.Since(start).Round(time.Millisecond))
	case <-time.After(waitDeadline):
		t.Fatal("Run did not return after cancellation")
	}

	// The port must actually be released.
	if _, err := get(context.Background(), "http://127.0.0.1:18092/"); err == nil {
		t.Fatal("server still accepting connections after shutdown")
	}
}

// TestListenFallsBackToOneFamily covers a host where one stack is unavailable:
// holding the IPv6 wildcard makes the tcp6 bind fail, and the service must still
// come up on IPv4 rather than refusing to start. Occupying "[::]" with Go's tcp6
// network sets IPV6_V6ONLY, so it does not also take the IPv4 wildcard.
func TestListenFallsBackToOneFamily(t *testing.T) {
	var lc net.ListenConfig
	blocker, err := lc.Listen(context.Background(), "tcp6", "[::]:18091")
	if err != nil {
		t.Skipf("cannot hold the IPv6 wildcard on this host: %v", err)
	}
	defer func() { _ = blocker.Close() }()

	_, rs := newTestRCON(t)
	cfg := testConfig(t, rs.Addr())
	cfg.HTTP.Bind = ""
	cfg.HTTP.Port = 18091
	s := newTestServer(t, cfg)

	listeners, err := s.listen(context.Background())
	if err != nil {
		t.Fatalf("listen should have fallen back to IPv4, got: %v", err)
	}
	defer func() {
		for _, ln := range listeners {
			_ = ln.Close()
		}
	}()

	if len(listeners) != 1 {
		t.Fatalf("got %d listeners, want 1 (IPv4 only)", len(listeners))
	}
	if got := listeners[0].Addr().String(); got != "0.0.0.0:18091" {
		t.Fatalf("fell back to %s, want the IPv4 wildcard", got)
	}
	t.Logf("degraded to a single family: %s", listeners[0].Addr())
}

// TestListenBothFamiliesUnavailable is the other half of the policy: degrading is
// fine, but a port that is unusable on every family is still a hard failure.
func TestListenBothFamiliesUnavailable(t *testing.T) {
	var lc net.ListenConfig
	v4, err := lc.Listen(context.Background(), "tcp4", "0.0.0.0:18090")
	if err != nil {
		t.Skipf("cannot hold the IPv4 wildcard: %v", err)
	}
	defer func() { _ = v4.Close() }()
	v6, err := lc.Listen(context.Background(), "tcp6", "[::]:18090")
	if err != nil {
		t.Skipf("cannot hold the IPv6 wildcard: %v", err)
	}
	defer func() { _ = v6.Close() }()

	_, rs := newTestRCON(t)
	cfg := testConfig(t, rs.Addr())
	cfg.HTTP.Bind = ""
	cfg.HTTP.Port = 18090
	s := newTestServer(t, cfg)

	if _, err := s.listen(context.Background()); err == nil {
		t.Fatal("expected an error when no family could bind")
	} else {
		t.Logf("hard failure when nothing could bind: %v", err)
	}
}

func TestPortInUse(t *testing.T) {
	blocker := httptest.NewServer(http.NotFoundHandler())
	defer blocker.Close()
	_, port := splitAddr(t, strings.TrimPrefix(blocker.URL, "http://"))

	_, rs := newTestRCON(t)
	cfg := testConfig(t, rs.Addr())
	cfg.HTTP.Port = port
	s := newTestServer(t, cfg)

	done := make(chan error, 1)
	go func() { done <- s.Run(context.Background()) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected a bind error")
		}
		t.Logf("bind failure surfaced: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("Run swallowed the bind error")
	}
}

// TestShutdownGraceExceedsRCONTimeout guards the drain invariant: a poll
// already in flight when the signal arrives gets to finish or time out on its
// own terms, so the grace must always clear the RCON deadline.
func TestShutdownGraceExceedsRCONTimeout(t *testing.T) {
	for _, timeout := range []time.Duration{time.Second, 5 * time.Second, 60 * time.Second} {
		if grace := shutdownGrace(timeout); grace <= timeout {
			t.Errorf("shutdownGrace(%s) = %s, does not clear the timeout", timeout, grace)
		}
	}
}
