package players

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SRS-Hosting/rcon"
	"github.com/SRS-Hosting/rcon/rcontest"
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

const playerBody = "(PlayerInfoAll): Total Players: 1. \n" + goldenBare

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

// countPrefix counts by prefix, which is how the attribute commands have to be
// counted: they name a player, so no exact match can see them.
func (fg *fakeGame) countPrefix(prefix string) int {
	fg.mu.Lock()
	defer fg.mu.Unlock()
	n := 0
	for _, c := range fg.commands {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// countHealth is the number the budget is about: every command spent on vitals.
// It counts both attribute commands, not just the one the poller issues, so a
// change that starts asking per value shows up as a budget breach instead of
// slipping past. "GetAllAttr" does not start with "GetAttr ", so the two
// prefixes cannot double count.
func (fg *fakeGame) countHealth() int {
	return fg.countPrefix("GetAttr ") + fg.countPrefix(commandGetAllAttr)
}

// rosterBody builds a PlayerInfoAll answer in the verified bare layout for the
// named players, all at the same growth, with distinct AGIDs and positions.
func rosterBody(growth float64, names ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "(PlayerInfoAll): Total Players: %d. \n", len(names))
	for i, name := range names {
		fmt.Fprintf(&b, "Name: %s / AGID: %d-000-000 / Dinosaur: Rex / Role: None / Marks: 10 / Growth: %v / Location: (X=%d.0 Y=0.0 Z=0.0)\n",
			name, i+1, growth, (i+1)*1000)
	}
	return b.String()
}

// scriptHealth teaches the fake the attribute answer for each player: one
// GetAllAttr carrying both vitals and both maxima, which is what the real
// command does.
func (fg *fakeGame) scriptHealth(names []string, health, maxHealth, stamina, maxStamina float64) {
	for _, name := range names {
		fg.set(commandGetAllAttr+" "+name, fmt.Sprintf(
			"(GetAllAttr %s): LocomotionState=3.000000, Health=%f, MaxHealth=%f, "+
				"Stamina=%f, MaxStamina=%f, Growth=1.000000",
			name, health, maxHealth, stamina, maxStamina))
	}
}

// scriptHealthAnswer makes the attribute command for these players answer the
// given body: for a game with no pawn to report, or no such command.
func (fg *fakeGame) scriptHealthAnswer(names []string, body string) {
	for _, name := range names {
		fg.set(commandGetAllAttr+" "+name, body)
	}
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
	snap, _, errMsg := p.Observe()
	if snap == nil || !snap.GeneratedAt.Equal(healthy.GeneratedAt) {
		t.Fatalf("stale snapshot was not preserved: %+v", snap)
	}
	if len(snap.Players) != 1 {
		t.Errorf("stale snapshot lost its players")
	}
	// The error message reaches every viewer's browser; the RCON address is
	// the operator's business and must stay in the logs.
	if strings.Contains(errMsg, client.Addr()) {
		t.Errorf("errMsg %q leaks the RCON address", errMsg)
	}
}

// TestPollerPublishesPartialResponse covers the shape a connection-scoped
// page key, an expired page, or a mid-series timeout produces: page 1 arrives
// and the rest never does. The poll must publish what came — fresh, labelled,
// visibly incomplete — rather than freeze the map on an older snapshot.
func TestPollerPublishesPartialResponse(t *testing.T) {
	fg, client := newFakeGame(t)
	fg.set(commandPlayerInfoAll, "[Page(Key 7) 1/2](PlayerInfoAll): Total Players: 2. \n"+goldenBare)
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

// The vitals tests all use the verified live readings: 96.5 hit points out of
// 850, which is a player at 11% — not, as the raw number invites, one at 96%.
// Stamina's maximum is deliberately not the 100 the live server reported: with
// 250 the value and the percentage differ, so a missing division shows up here
// instead of hiding behind a coincidence.
const (
	liveHealth     = 96.534752
	liveMaxHealth  = 850.0
	liveStamina    = 33.199955
	liveMaxStamina = 250.0
)

// healthNames is a four-player roster: more than any budget these tests grant,
// so "one cycle cannot cover everyone" is the case under test.
func healthNames() []string { return []string{"kittykat95", "rex", "trike", "ptera"} }

// newHealthGame is a fake game with a roster and the attribute answer for every
// player in it.
func newHealthGame(t *testing.T, growth float64) (*fakeGame, *rcon.Client) {
	t.Helper()
	fg, client := newFakeGame(t)
	fg.set(commandPlayerInfoAll, rosterBody(growth, healthNames()...))
	fg.scriptHealth(healthNames(), liveHealth, liveMaxHealth, liveStamina, liveMaxStamina)
	return fg, client
}

// waitForHealth waits until every player in the snapshot carries both readings.
func waitForHealth(t *testing.T, p *Poller) *Snapshot {
	t.Helper()
	var snap *Snapshot
	waitFor(t, "vitals for every player", func() bool {
		snap, _, _ = p.Observe()
		if snap == nil || len(snap.Players) != len(healthNames()) {
			return false
		}
		for _, player := range snap.Players {
			if !player.HasHealth || !player.HasStamina {
				return false
			}
		}
		return true
	})
	return snap
}

// TestPollerHealthBudgetIsFlatInPlayerCount is the cost contract this feature
// had to be designed around. Vitals only answer per player, so the obvious
// implementation spends a command per player per poll — the linear game-thread
// tax the whole application exists to avoid. Instead the spend per poll is
// capped, and every player still gets a reading, just a cycle or two later.
func TestPollerHealthBudgetIsFlatInPlayerCount(t *testing.T) {
	const budget = 2
	fg, client := newHealthGame(t, 1)
	fixed := gondwaInfo()
	p := NewPoller(client, testInterval, testIdleAfter, &fixed, nil, WithHealth(budget))
	startPoller(t, p)

	p.Observe()
	snap := waitForHealth(t, p)

	// Vitals are read before the poll count, so the poll count can only ever
	// over-count the cycles those commands came from — never under-count them
	// and turn a passing budget into a failure.
	health := fg.countHealth()
	polls := fg.count(commandPlayerInfoAll)
	if polls == 0 {
		t.Fatal("no polls were issued at all")
	}
	if health > budget*polls {
		t.Errorf("%d vitals commands across %d polls exceeds the budget of %d per poll",
			health, polls, budget)
	}
	if health >= len(healthNames())*polls {
		t.Errorf("%d vitals commands across %d polls for %d players: the cost is scaling with the roster",
			health, polls, len(healthNames()))
	}
	// Both vitals came out of those commands: adding stamina must not have added
	// a command per player.
	if n := fg.countPrefix("GetAttr "); n != 0 {
		t.Errorf("%d single-value commands were issued; one GetAllAttr carries both vitals", n)
	}

	// Every player has both readings even though no single cycle could have
	// asked them all: that is the cache doing its job between samples.
	for _, player := range snap.Players {
		if player.Health != liveHealth || player.MaxHealth != liveMaxHealth {
			t.Errorf("%s health = %v/%v", player.Name, player.Health, player.MaxHealth)
		}
		if player.HealthPercent < 11 || player.HealthPercent > 12 {
			t.Errorf("%s is at %v%% health, want ~11.4: the value is absolute hit points and must "+
				"be divided by MaxHealth, or a dying player renders as a healthy one",
				player.Name, player.HealthPercent)
		}
		if player.Stamina != liveStamina || player.MaxStamina != liveMaxStamina {
			t.Errorf("%s stamina = %v/%v", player.Name, player.Stamina, player.MaxStamina)
		}
		if player.StaminaPercent < 13 || player.StaminaPercent > 14 {
			t.Errorf("%s is at %v%% stamina, want ~13.3: stamina is absolute too, and its maximum "+
				"is not always 100", player.Name, player.StaminaPercent)
		}
		if !player.HasPosition {
			t.Errorf("%s lost its position to the vitals step", player.Name)
		}
	}
}

// TestPollerHealthSpendsOneCommandPerSample pins the cost model: one command per
// sampled player, whatever the roster has been through. The maxima ride along in
// every answer, so there is no second question to ask and no invalidation rule
// that can go stale — a player who grows, or a patch that rebalances a species,
// heals on the next sample by itself.
func TestPollerHealthSpendsOneCommandPerSample(t *testing.T) {
	fg, client := newHealthGame(t, 0.5)
	fixed := gondwaInfo()
	p := NewPoller(client, testInterval, testIdleAfter, &fixed, nil, WithHealth(len(healthNames())))
	startPoller(t, p)

	p.Observe()
	waitForHealth(t, p)

	polls := fg.count(commandPlayerInfoAll)
	waitFor(t, "three further polls", func() bool {
		p.Observe()
		return fg.count(commandPlayerInfoAll) >= polls+3
	})

	// One attribute command per sampled player, and nothing else: no follow-up
	// question for the second value, no re-read for a maximum.
	samples := fg.countPrefix(commandGetAllAttr)
	if total := fg.countHealth(); total != samples {
		t.Errorf("%d vitals commands for %d samples; a sample must cost exactly one command",
			total, samples)
	}

	// The maxima heal without any invalidation event: the game starts reporting
	// different ones — a rebalance, a growth spurt, a species swap — and the next
	// sample simply carries them.
	fg.scriptHealth(healthNames(), liveHealth, 1200, liveStamina, 400)
	waitFor(t, "the new maxima to arrive", func() bool {
		snap, _, _ := p.Observe()
		if snap == nil {
			return false
		}
		for _, player := range snap.Players {
			if player.MaxHealth != 1200 || player.MaxStamina != 400 {
				return false
			}
		}
		return true
	})

	snap, _, _ := p.Observe()
	for _, player := range snap.Players {
		// 96.5 of 1200 is 8%, and the page must be told 8 rather than the 11 the
		// old maximum would have produced.
		if player.HealthPercent < 8 || player.HealthPercent > 8.1 {
			t.Errorf("%s is at %v%% against the new maximum, want ~8.04", player.Name, player.HealthPercent)
		}
	}
}

// TestPollerHealthIsSilentWhileIdle is the rule vitals must obey exactly as
// positions do: a map nobody is looking at costs the game nothing at all.
func TestPollerHealthIsSilentWhileIdle(t *testing.T) {
	fg, client := newHealthGame(t, 1)
	fixed := gondwaInfo()
	p := NewPoller(client, testInterval, testIdleAfter, &fixed, nil, WithHealth(4))
	startPoller(t, p)

	// Nobody has asked yet, so not one attribute command may go out.
	time.Sleep(4 * testInterval)
	if n := fg.countHealth(); n != 0 {
		t.Fatalf("%d vitals commands before any viewer arrived", n)
	}

	p.Observe()
	waitForHealth(t, p)

	waitFor(t, "polling to go idle", func() bool {
		before := fg.count(commandPlayerInfoAll)
		time.Sleep(3 * testInterval)
		return fg.count(commandPlayerInfoAll) == before
	})
	// And once the viewers are gone, vitals go quiet with the positions.
	before := fg.countHealth()
	time.Sleep(3 * testInterval)
	if after := fg.countHealth(); after != before {
		t.Errorf("vitals sampling continued while idle: %d -> %d", before, after)
	}
}

// TestPollerHealthNoPawn covers a player sitting in the menus or dead and not
// yet respawned. The game answers "No Player Pawn." — a normal state that must
// read as no vitals at all, leave the rest of the snapshot alone, and never
// surface as an error on the page.
func TestPollerHealthNoPawn(t *testing.T) {
	fg, client := newHealthGame(t, 1)
	fg.scriptHealthAnswer(healthNames(), "No Player Pawn.")
	fixed := gondwaInfo()
	p := NewPoller(client, testInterval, testIdleAfter, &fixed, nil, WithHealth(len(healthNames())))
	startPoller(t, p)

	p.Observe()
	waitFor(t, "a snapshot after a full sampling round", func() bool {
		snap, _, _ := p.Observe()
		return snap != nil && fg.countHealth() >= len(healthNames())
	})

	snap, _, errMsg := p.Observe()
	if errMsg != "" {
		t.Errorf("errMsg = %q; an unspawned player is not a failure", errMsg)
	}
	if len(snap.Players) != len(healthNames()) || !snap.Complete {
		t.Fatalf("snapshot = %+v", snap)
	}
	for _, player := range snap.Players {
		if player.HasHealth || player.HealthPercent != 0 {
			t.Errorf("%s = %+v, want unknown health", player.Name, player)
		}
		if player.HasStamina || player.StaminaPercent != 0 {
			t.Errorf("%s = %+v, want unknown stamina", player.Name, player)
		}
		if !player.HasPosition || player.U == 0 {
			t.Errorf("%s lost its position: %+v", player.Name, player)
		}
	}
}

// TestPollerHealthUnavailableLeavesPositionsIntact is the degradation promise:
// on a build that has never heard of this command, the map works exactly as it
// did before vitals existed.
func TestPollerHealthUnavailableLeavesPositionsIntact(t *testing.T) {
	fg, client := newHealthGame(t, 1)
	// The fake answers unknown commands the way the game does.
	fg.scriptHealthAnswer(healthNames(), "That command does not exist")
	fixed := gondwaInfo()
	p := NewPoller(client, testInterval, testIdleAfter, &fixed, nil, WithHealth(len(healthNames())))
	startPoller(t, p)

	p.Observe()
	waitFor(t, "a snapshot after a full sampling round", func() bool {
		snap, _, _ := p.Observe()
		return snap != nil && fg.countHealth() >= len(healthNames())
	})

	snap, info, errMsg := p.Observe()
	if errMsg != "" {
		t.Errorf("errMsg = %q; a health failure must not label the map", errMsg)
	}
	if info == nil || !snap.Complete || len(snap.Players) != len(healthNames()) {
		t.Fatalf("snapshot = %+v, map = %+v", snap, info)
	}
	for _, player := range snap.Players {
		if player.HasHealth || player.HasStamina {
			t.Errorf("%s claims vitals the game never reported: %+v", player.Name, player)
		}
		if !player.HasPosition || player.V != 0.5 {
			t.Errorf("%s = %+v, want an intact projected position", player.Name, player)
		}
	}
}

// TestPollerHealthFailureDoesNotStarveTheRotation covers the failure the budget
// could otherwise turn into a permanent blackout: a player the client will not
// even send a command about. The cycle stops early on purpose — a failure is
// usually the server itself, and spending the rest of the budget on timeouts
// would only delay the next position poll — but the player who failed must still
// lose its place at the front of the queue, or everyone behind it would never be
// sampled again.
func TestPollerHealthFailureDoesNotStarveTheRotation(t *testing.T) {
	// A name this long makes the command longer than the protocol allows, so the
	// client refuses it without dialling: an RCON-level failure with no timeout
	// to wait out.
	unaskable := strings.Repeat("x", rcon.MaxCommandLen)
	fg, client := newFakeGame(t)
	fg.set(commandPlayerInfoAll, rosterBody(1, append([]string{unaskable}, healthNames()...)...))
	fg.scriptHealth(healthNames(), liveHealth, liveMaxHealth, liveStamina, liveMaxStamina)

	fixed := gondwaInfo()
	p := NewPoller(client, testInterval, testIdleAfter, &fixed, nil, WithHealth(len(healthNames())))
	startPoller(t, p)

	p.Observe()
	var snap *Snapshot
	waitFor(t, "vitals for everyone the client will ask about", func() bool {
		snap, _, _ = p.Observe()
		if snap == nil || len(snap.Players) != len(healthNames())+1 {
			return false
		}
		for _, player := range snap.Players {
			if player.Name != unaskable && (!player.HasHealth || !player.HasStamina) {
				return false
			}
		}
		return true
	})

	_, _, errMsg := p.Observe()
	if errMsg != "" {
		t.Errorf("errMsg = %q; a vitals failure must not label the map", errMsg)
	}
	for _, player := range snap.Players {
		if !player.HasPosition {
			t.Errorf("%.8s lost its position to a vitals failure", player.Name)
		}
	}
	if snap.Players[0].HasHealth || snap.Players[0].HasStamina {
		t.Error("the unaskable player reports vitals nobody could have read")
	}
}

// TestPollerHealthOffCostsNothing is the operator's escape hatch: with vitals
// switched off, not one attribute command is issued, ever.
func TestPollerHealthOffCostsNothing(t *testing.T) {
	for name, opts := range map[string][]PollerOption{
		"never asked for": nil,
		"explicitly zero": {WithHealth(0)},
		"nonsense budget": {WithHealth(-1)},
	} {
		t.Run(name, func(t *testing.T) {
			fg, client := newHealthGame(t, 1)
			fixed := gondwaInfo()
			p := NewPoller(client, testInterval, testIdleAfter, &fixed, nil, opts...)
			startPoller(t, p)

			p.Observe()
			waitFor(t, "three polls", func() bool {
				p.Observe()
				return fg.count(commandPlayerInfoAll) >= 3
			})

			if n := fg.countHealth(); n != 0 {
				t.Errorf("%d vitals commands with sampling switched off", n)
			}
			snap, _, _ := p.Observe()
			for _, player := range snap.Players {
				if player.HasHealth || player.HasStamina {
					t.Errorf("%s carries vitals nobody asked for", player.Name)
				}
				if !player.HasPosition {
					t.Errorf("%s lost its position: %+v", player.Name, player)
				}
			}
		})
	}
}

// TestPollerHealthNotSampledDuringAnOutage covers the other half of failure
// containment: when the position poll itself fails, vitals must not go hunting
// for players on a server that just proved it cannot answer, and the last good
// snapshot keeps the readings it already had.
func TestPollerHealthNotSampledDuringAnOutage(t *testing.T) {
	fg, client := newHealthGame(t, 1)
	fixed := gondwaInfo()
	p := NewPoller(client, testInterval, testIdleAfter, &fixed, nil, WithHealth(len(healthNames())))
	startPoller(t, p)

	p.Observe()
	healthy := waitForHealth(t, p)

	fg.setDown(true)
	waitFor(t, "the outage to surface", func() bool {
		_, _, errMsg := p.Observe()
		return errMsg != ""
	})
	// A failed connection still logs no command at the fake, so this counts
	// attempts the poller decided to make: after the outage is known, it must
	// make none.
	before := fg.countHealth()
	time.Sleep(3 * testInterval)
	if after := fg.countHealth(); after != before {
		t.Errorf("vitals kept probing a server that is down: %d -> %d", before, after)
	}

	snap, _, _ := p.Observe()
	if snap == nil || !snap.GeneratedAt.Equal(healthy.GeneratedAt) {
		t.Fatalf("stale snapshot was not preserved: %+v", snap)
	}
	if !snap.Players[0].HasHealth || snap.Players[0].HealthPercent < 11 {
		t.Errorf("the outage cost the stale snapshot its health: %+v", snap.Players[0])
	}
	if !snap.Players[0].HasStamina || snap.Players[0].StaminaPercent < 13 {
		t.Errorf("the outage cost the stale snapshot its stamina: %+v", snap.Players[0])
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
