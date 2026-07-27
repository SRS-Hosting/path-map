package players

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/USA-RedDragon/rcon"
	"github.com/USA-RedDragon/rcon/rcontest"
)

const testPassword = "pw"

// Canonical map names, hoisted for goconst and to mirror the preset keys.
const (
	mapGondwa  = "gondwa"
	mapPanjura = "panjura"
)

// Poll cadences are compressed for tests; the waits below are generous so a
// loaded CI machine cannot flake them.
const (
	testInterval  = 50 * time.Millisecond
	testIdleAfter = 150 * time.Millisecond
	waitDeadline  = 5 * time.Second
)

const gondwaPOIBody = "(ListPOI): Impact Crater, Grand Plains, Titan's Pass, Snake Gully, Savanna Grassland, Salt Flats, Burned Forest, Red Island"

const panjuraPOIBody = "(ListPOI): Grassland Crater, Arc Mountain, The Mire, Blackwater Bayou, Tyrants Gorge, Star Ravine"

const playerBody = "Total Players: 1.\n" + goldenRecord

// fakeGame is a scriptable RCON server: canned response bodies, an optional
// down mode, and a command log. The mutex is not optional: rcontest serves
// each client connection in its own goroutine, and every command arrives on
// a fresh connection.
type fakeGame struct {
	mu        sync.Mutex
	responses map[string]string
	// down makes every new connection fail authentication. The client dials
	// fresh per command, so this is indistinguishable from a crashed game
	// server, without tearing the listener down.
	down     bool
	commands []string
}

func newFakeGame(t *testing.T) (*fakeGame, *rcon.Client) {
	t.Helper()
	return newFakeGameCap(t, 4)
}

func newFakeGameCap(t *testing.T, maxConcurrent int) (*fakeGame, *rcon.Client) {
	t.Helper()
	fg := &fakeGame{responses: map[string]string{
		commandListPOI:       gondwaPOIBody,
		commandPlayerInfoAll: playerBody,
	}}
	srv, err := rcontest.New(fg.handle)
	if err != nil {
		t.Fatalf("rcontest.New: %v", err)
	}
	t.Cleanup(srv.Close)
	return fg, rcon.New(srv.Addr(), testPassword,
		rcon.WithTimeout(2*time.Second),
		rcon.WithMaxConcurrent(maxConcurrent),
	)
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
	rcontest.Respond(testPassword, 0, fg.respond)(f)
}

func (fg *fakeGame) respond(command string) string {
	fg.mu.Lock()
	defer fg.mu.Unlock()
	fg.commands = append(fg.commands, command)
	if body, ok := fg.responses[command]; ok {
		return body
	}
	return "That command does not exist"
}

func (fg *fakeGame) set(command, body string) {
	fg.mu.Lock()
	defer fg.mu.Unlock()
	fg.responses[command] = body
}

func (fg *fakeGame) setDown(down bool) {
	fg.mu.Lock()
	defer fg.mu.Unlock()
	fg.down = down
}

func (fg *fakeGame) count(command string) int {
	fg.mu.Lock()
	defer fg.mu.Unlock()
	n := 0
	for _, c := range fg.commands {
		if c == command {
			n++
		}
	}
	return n
}

func gondwaInfo() MapInfo {
	return MapInfo{Name: mapGondwa, DisplayName: "Gondwa", HalfExtentX: 403446.75, HalfExtentY: 403857.03, ImageFile: "gondwa.png"}
}

// lookupTestMaps resolves the two maps the tests detect.
func lookupTestMaps(name string) (MapInfo, bool) {
	switch name {
	case mapGondwa:
		return gondwaInfo(), true
	case mapPanjura:
		return MapInfo{Name: mapPanjura, DisplayName: "Panjura", HalfExtentX: 504000, HalfExtentY: 504000, ImageFile: "panjura.png"}, true
	default:
		return MapInfo{}, false
	}
}

// startPoller runs p until the test ends.
func startPoller(t *testing.T, p *Poller) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
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

func TestPollerDemandDriven(t *testing.T) {
	fg, client := newFakeGame(t)
	fixed := gondwaInfo()
	p := NewPoller(client, testInterval, testIdleAfter, &fixed, nil)
	startPoller(t, p)

	// Nobody has asked, so nothing may poll: this sleep is the test.
	time.Sleep(4 * testInterval)
	if n := fg.count(commandPlayerInfoAll); n != 0 {
		t.Fatalf("%d polls before any viewer arrived", n)
	}

	// One viewer wakes it up.
	p.Observe()
	waitFor(t, "first snapshot", func() bool {
		snap, _, _ := p.Observe()
		return snap != nil
	})

	// A snapshot from the fixed-map poller carries projected coordinates.
	snap, info, errMsg := p.Observe()
	if errMsg != "" {
		t.Errorf("errMsg = %q", errMsg)
	}
	if info == nil || info.Name != mapGondwa {
		t.Fatalf("resolved map = %+v", info)
	}
	if len(snap.Players) != 1 || !snap.Complete {
		t.Fatalf("snapshot = %+v", snap)
	}
	if u := snap.Players[0].U; u < 0.41 || u > 0.42 {
		t.Errorf("player u = %v, want ~0.416: projection did not run", u)
	}

	// Note: the Observe calls in waitFor above kept stamping demand. Once the
	// demand window empties, polling must stop again.
	waitFor(t, "polling to go idle", func() bool {
		before := fg.count(commandPlayerInfoAll)
		time.Sleep(3 * testInterval)
		return fg.count(commandPlayerInfoAll) == before
	})
}

func TestPollerWakeAfterIdle(t *testing.T) {
	fg, client := newFakeGame(t)
	fixed := gondwaInfo()
	p := NewPoller(client, testInterval, testIdleAfter, &fixed, nil)
	startPoller(t, p)

	p.Observe()
	waitFor(t, "first snapshot", func() bool {
		snap, _, _ := p.Observe()
		return snap != nil
	})
	waitFor(t, "polling to go idle", func() bool {
		before := fg.count(commandPlayerInfoAll)
		time.Sleep(3 * testInterval)
		return fg.count(commandPlayerInfoAll) == before
	})

	// The returning viewer's own Observe is the wake signal; the snapshot
	// must refresh without waiting out a full idle tick cycle.
	snapBefore, _, _ := p.Observe()
	waitFor(t, "a fresh snapshot after idle", func() bool {
		snap, _, _ := p.Observe()
		return snap != nil && snap.GeneratedAt.After(snapBefore.GeneratedAt)
	})
}

func TestPollerServesStaleOnFailure(t *testing.T) {
	fg, client := newFakeGame(t)
	fixed := gondwaInfo()
	p := NewPoller(client, testInterval, testIdleAfter, &fixed, nil)
	startPoller(t, p)

	p.Observe()
	waitFor(t, "first snapshot", func() bool {
		snap, _, _ := p.Observe()
		return snap != nil
	})
	healthy, _, _ := p.Observe()

	// The game goes away; the last good snapshot must keep serving, labelled
	// with an error rather than blanked.
	fg.setDown(true)
	waitFor(t, "the failure to surface", func() bool {
		_, _, errMsg := p.Observe()
		return errMsg != ""
	})
	snap, _, _ := p.Observe()
	if snap == nil || !snap.GeneratedAt.Equal(healthy.GeneratedAt) {
		t.Fatalf("stale snapshot was not preserved: %+v", snap)
	}
	if len(snap.Players) != 1 {
		t.Errorf("stale snapshot lost its players")
	}
}

// TestPollerPublishesPartialResponse covers the shape a connection-scoped
// page key, an expired page, or a mid-series timeout produces: page 1 arrives
// and the rest never does. The poll must publish what came — fresh, labelled,
// visibly incomplete — rather than freeze the map on an older snapshot.
func TestPollerPublishesPartialResponse(t *testing.T) {
	fg, client := newFakeGame(t)
	fg.set(commandPlayerInfoAll, "[Page(Key 7) 1/2]Total Players: 2.\n"+goldenRecord)
	// page:7-2 falls through to the unknown-command reply, which carries no
	// page header — exactly what a dead key looks like. Auto mode, so the
	// no-re-detection assertion below has teeth.
	p := NewPoller(client, testInterval, testIdleAfter, nil, lookupTestMaps)
	startPoller(t, p)

	p.Observe()
	var snap *Snapshot
	var errMsg string
	waitFor(t, "a partial snapshot", func() bool {
		snap, _, errMsg = p.Observe()
		return snap != nil
	})

	if len(snap.Players) != 1 {
		t.Fatalf("partial snapshot has %d players, want the 1 that arrived", len(snap.Players))
	}
	if snap.Complete {
		t.Error("partial snapshot claims to be complete")
	}
	if errMsg == "" {
		t.Error("partial snapshot carries no error label")
	}
	assertGolden(t, snap.Players[0])
	if u := snap.Players[0].U; u < 0.41 || u > 0.42 {
		t.Errorf("player u = %v: projection did not run on the partial", u)
	}

	// Partials must stay fresh: each poll replaces the last one rather than
	// the map aging while the error persists.
	first := snap.GeneratedAt
	waitFor(t, "a fresher partial snapshot", func() bool {
		s, _, _ := p.Observe()
		return s.GeneratedAt.After(first)
	})

	// A partial response is an answer, not an outage: persistent partials
	// must not put detection into a loop. One ListPOI for the initial
	// detection, none after any number of partial polls.
	if n := fg.count(commandListPOI); n != 1 {
		t.Errorf("ListPOI ran %d times, want exactly the initial detection", n)
	}
}

func TestPollerAutoDetect(t *testing.T) {
	fg, client := newFakeGame(t)
	p := NewPoller(client, testInterval, testIdleAfter, nil, lookupTestMaps)
	startPoller(t, p)

	p.Observe()
	waitFor(t, "detection and first snapshot", func() bool {
		snap, info, _ := p.Observe()
		return snap != nil && info != nil
	})

	snap, info, _ := p.Observe()
	if info.Name != mapGondwa || info.DisplayName != "Gondwa" {
		t.Fatalf("detected map = %+v", info)
	}
	if u := snap.Players[0].U; u < 0.41 || u > 0.42 {
		t.Errorf("player u = %v: detected extents were not applied", u)
	}

	// Detection must precede the first player poll, and must not repeat while
	// the resolution holds.
	if fg.count(commandListPOI) != 1 {
		t.Errorf("ListPOI ran %d times, want exactly once", fg.count(commandListPOI))
	}
}

func TestPollerRedetectsAfterOutage(t *testing.T) {
	fg, client := newFakeGame(t)
	p := NewPoller(client, testInterval, testIdleAfter, nil, lookupTestMaps)
	startPoller(t, p)

	p.Observe()
	waitFor(t, "initial detection", func() bool {
		_, info, _ := p.Observe()
		return info != nil && info.Name == mapGondwa
	})

	// The server dies, restarts on a different map (the only way ServerMap
	// changes), and comes back speaking Panjura.
	fg.setDown(true)
	waitFor(t, "the outage to be noticed", func() bool {
		_, _, errMsg := p.Observe()
		return errMsg != ""
	})
	fg.set(commandListPOI, panjuraPOIBody)
	fg.setDown(false)

	waitFor(t, "re-detection after the outage", func() bool {
		_, info, _ := p.Observe()
		return info != nil && info.Name == mapPanjura
	})
	_, info, _ := p.Observe()
	if info.HalfExtentX != 504000 {
		t.Errorf("re-detected extents = %v, want Panjura's", info.HalfExtentX)
	}
}

func TestPollerExplicitModeNeverDetects(t *testing.T) {
	fg, client := newFakeGame(t)
	fixed := gondwaInfo()
	p := NewPoller(client, testInterval, testIdleAfter, &fixed, nil)
	startPoller(t, p)

	p.Observe()
	waitFor(t, "first snapshot", func() bool {
		snap, _, _ := p.Observe()
		return snap != nil
	})

	// Even across an outage, a pinned map must never trigger detection.
	fg.setDown(true)
	waitFor(t, "the outage to be noticed", func() bool {
		_, _, errMsg := p.Observe()
		return errMsg != ""
	})
	fg.setDown(false)
	p.Observe()
	waitFor(t, "recovery", func() bool {
		_, _, errMsg := p.Observe()
		return errMsg == ""
	})

	if n := fg.count(commandListPOI); n != 0 {
		t.Errorf("ListPOI ran %d times in explicit mode, want never", n)
	}
}

// TestPollerSingleSlotClient runs auto mode on a capacity-1 client: each
// cycle issues detect and poll back to back, and the client releases its one
// slot from a goroutine that can lose a race against the very next claim.
// The poller's busy-wait must absorb that every cycle — a leak would show up
// as a failed poll (errMsg) and re-detection churn (extra ListPOI).
func TestPollerSingleSlotClient(t *testing.T) {
	fg, client := newFakeGameCap(t, 1)
	p := NewPoller(client, testInterval, testIdleAfter, nil, lookupTestMaps)
	startPoller(t, p)

	p.Observe()
	waitFor(t, "detection and first snapshot", func() bool {
		snap, info, _ := p.Observe()
		return snap != nil && info != nil
	})

	polls := 0
	var last time.Time
	waitFor(t, "three further clean polls", func() bool {
		snap, _, errMsg := p.Observe()
		if errMsg != "" {
			t.Fatalf("a poll failed on the single-slot client: %s", errMsg)
		}
		if snap.GeneratedAt.After(last) {
			last = snap.GeneratedAt
			polls++
		}
		return polls >= 3
	})
	if n := fg.count(commandListPOI); n != 1 {
		t.Errorf("ListPOI ran %d times, want exactly the initial detection", n)
	}
}

func TestPollerStopsOnCancel(t *testing.T) {
	_, client := newFakeGame(t)
	fixed := gondwaInfo()
	p := NewPoller(client, testInterval, testIdleAfter, &fixed, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.Run(ctx)
		close(done)
	}()

	p.Observe()
	cancel()
	select {
	case <-done:
	case <-time.After(waitDeadline):
		t.Fatal("Run did not return after cancellation")
	}
}

// TestDetectDoesNotMistakeAMixedList guards the scoring against a plausible
// real-world wrinkle: a response carrying names our reference lists lack must
// still resolve as long as a clear majority matches one map.
func TestDetectDoesNotMistakeAMixedList(t *testing.T) {
	body := gondwaPOIBody + ", Brand New Region, Another Addition"
	got, ok := Detect(body)
	if !ok || got != mapGondwa {
		t.Errorf("Detect = %q, %v; unknown extras should not defeat a clear majority", got, ok)
	}
	if !strings.Contains(gondwaPOIBody, "Impact Crater") {
		t.Fatal("test body lost its Gondwa names")
	}
}
