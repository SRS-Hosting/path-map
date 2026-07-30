package players

import (
	"math"
	"testing"
	"time"
)

// goldenAllAttr is the GetAllAttr answer shape from a live server, cut from
// about 150 pairs down to the two that matter plus enough neighbours to prove
// the scan ignores what it does not know — including HealthRecoveryRate, which
// starts with the key we are looking for. The numbers are the real ones, and
// they are the point of this whole file: 96.5 health against 850 max is a
// player at 11%, not a player at 96%.
const goldenAllAttr = "(GetAllAttr kittykat95): LocomotionState=3.000000, Health=96.534752, " +
	"MaxHealth=850.000000, HealthRecoveryRate=1.900000, Hunger=412.000000, Thirst=380.500000, " +
	"Growth=1.000000, BleedingRate=0.000000"

// goldenGetAttr is the single-value shape. Note the lower-cased attribute name
// in the message: the game says "Property health is" for a "Health" query.
const goldenGetAttr = "(GetAttr kittykat95 Health): Property health is 96.708755."

// The verified live player and her AGID, so every test that names one names the
// same one.
const (
	goldenName = "kittykat95"
	goldenAGID = "746-132-258"
)

func TestParseAttrsGetAllAttr(t *testing.T) {
	a := parseAttrs(goldenAllAttr)
	if !a.hasHealth || a.health != 96.534752 {
		t.Errorf("health = %v, %v", a.health, a.hasHealth)
	}
	if !a.hasMaxHealth || a.maxHealth != 850 {
		t.Errorf("maxHealth = %v, %v", a.maxHealth, a.hasMaxHealth)
	}
	if a.noPawn {
		t.Error("noPawn = true for a spawned player")
	}
}

func TestParseAttrsGetAttr(t *testing.T) {
	a := parseAttrs(goldenGetAttr)
	if !a.hasHealth || a.health != 96.708755 {
		t.Errorf("health = %v, %v: the trailing period must stay out of the number", a.health, a.hasHealth)
	}
	// A Health query says nothing about the maximum, and pretending otherwise
	// would make the percentage a division by zero.
	if a.hasMaxHealth {
		t.Errorf("maxHealth = %v from a Health query", a.maxHealth)
	}

	// The MaxHealth query's exact wording is unverified, so both spellings of
	// the property name must land.
	for _, raw := range []string{
		"(GetAttr kittykat95 MaxHealth): Property maxhealth is 850.000000.",
		"(GetAttr kittykat95 MaxHealth): Property MaxHealth is 850.",
	} {
		a := parseAttrs(raw)
		if !a.hasMaxHealth || a.maxHealth != 850 {
			t.Errorf("parseAttrs(%q) maxHealth = %v, %v", raw, a.maxHealth, a.hasMaxHealth)
		}
	}
}

// TestParseAttrsNoPawn covers a player in the menus or freshly dead. It is a
// normal state, so it must read as "no health" and never as a failure.
func TestParseAttrsNoPawn(t *testing.T) {
	for _, raw := range []string{
		"No Player Pawn.",
		"(GetAttr kittykat95 Health): No Player Pawn.",
		"(GetAllAttr kittykat95): No Player Pawn.",
	} {
		a := parseAttrs(raw)
		if !a.noPawn {
			t.Errorf("parseAttrs(%q).noPawn = false", raw)
		}
		if a.hasHealth || a.hasMaxHealth {
			t.Errorf("parseAttrs(%q) invented a reading: %+v", raw, a)
		}
	}
}

func TestParseAttrsGarbage(t *testing.T) {
	for _, raw := range []string{
		"",
		"That command does not exist",
		"(GetAttr kittykat95 Health): Property health is banana.",
		// A page seam landing mid-key, and a key with no value at all.
		"(GetAllAttr kittykat95): LocomotionState=3.000000, Hea",
		"(GetAllAttr kittykat95): Health=, MaxHealth=",
		"Health",
	} {
		a := parseAttrs(raw)
		if a.hasHealth || a.hasMaxHealth {
			t.Errorf("parseAttrs(%q) = %+v, want no readings", raw, a)
		}
	}
}

// TestHealthPercentIsRelativeToMax is the trap this feature lives or dies on.
// Health is absolute hit points: the live reading of 96.5 belongs to a player
// at 11% of an 850 maximum. Anything that treats Health as a percentage paints
// them bright green while they are one bite from dead, so this test fails
// loudly rather than subtly.
func TestHealthPercentIsRelativeToMax(t *testing.T) {
	pct, ok := healthPercent(96.534752, 850)
	if !ok {
		t.Fatal("healthPercent refused the verified live reading")
	}
	if pct < 11 || pct > 12 {
		t.Errorf("healthPercent(96.534752, 850) = %v, want ~11.4; Health is absolute hit points, "+
			"not a percentage, and reading it as one shows a dying player as healthy", pct)
	}

	// The same trap one layer up: parsed straight from the wire, the reading
	// must still land in the red band rather than the green one.
	a := parseAttrs(goldenAllAttr)
	pct, ok = healthPercent(a.health, a.maxHealth)
	if !ok || pct >= 33 {
		t.Errorf("parsed live reading is %v%%, want under 33 (the red band)", pct)
	}
}

func TestHealthPercent(t *testing.T) {
	tests := []struct {
		name              string
		health, maxHealth float64
		want              float64
		wantOK            bool
	}{
		{"full health is exactly 100", 850, 850, 100, true},
		{"half", 425, 850, 50, true},
		{"dead", 0, 850, 0, true},
		{"almost dead", 8.5, 850, 1, true},
		// The bands end at 100, so overheal is clamped rather than left to fall
		// outside every one of them.
		{"overheal clamps", 900, 850, 100, true},
		{"negative health clamps", -5, 850, 0, true},
		// No maximum means no percentage: a division by zero would surface as
		// +Inf and paint a full green marker.
		{"no maximum", 96.5, 0, 0, false},
		{"negative maximum", 96.5, -850, 0, false},
		{"NaN maximum", 96.5, math.NaN(), 0, false},
		{"NaN health", math.NaN(), 850, 0, false},
		{"infinite health", math.Inf(1), 850, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := healthPercent(tc.health, tc.maxHealth)
			if ok != tc.wantOK || got != tc.want {
				t.Errorf("healthPercent(%v, %v) = %v, %v; want %v, %v",
					tc.health, tc.maxHealth, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// The two questions health can be asked about the golden player: the paginating
// one that carries MaxHealth, and the one-round-trip one that does not.
const (
	wantAllAttr = "GetAllAttr kittykat95"
	wantGetAttr = "GetAttr kittykat95 Health"
)

// TestHealthCommandSpendsTheCheapQuestion pins the cost model: the paginating
// command is only issued when the percentage cannot be computed without it.
func TestHealthCommandSpendsTheCheapQuestion(t *testing.T) {
	player := Player{Name: goldenName, AGID: goldenAGID, Dinosaur: "Hatzegopteryx", Growth: 1}

	cold := healthCommand(player, healthEntry{})
	if cold != wantAllAttr {
		t.Errorf("first sample asks %q, want the command that carries MaxHealth", cold)
	}

	warm := healthEntry{hasMax: true, maxHealth: 850, growthAtMax: 1, speciesAtMax: "Hatzegopteryx"}
	if got := healthCommand(player, warm); got != wantGetAttr {
		t.Errorf("cached MaxHealth still asks %q, want the one-round-trip command", got)
	}

	// Growth creeps continuously, so a hair of drift must not spend the
	// expensive command; real growth must.
	player.Growth = 1 + growthEpsilon/2
	if got := healthCommand(player, warm); got != wantGetAttr {
		t.Errorf("growth drift of %v re-read MaxHealth: %q", growthEpsilon/2, got)
	}
	player.Growth = 0.5
	if got := healthCommand(player, warm); got != wantAllAttr {
		t.Errorf("a grown player asks %q, want MaxHealth re-read", got)
	}

	// And the case growth alone would miss: a new body at the same growth has a
	// different maximum, so the cached one belongs to nobody.
	player.Growth = 1
	player.Dinosaur = "Tyrannosaurus"
	if got := healthCommand(player, warm); got != wantAllAttr {
		t.Errorf("a player who switched species asks %q, want MaxHealth re-read", got)
	}
}

// TestHealthKeyPrefersAGID matches the page, which keys its markers the same
// way, and covers the torn-record fallback.
func TestHealthKeyPrefersAGID(t *testing.T) {
	if got := healthKey(Player{Name: goldenName, AGID: goldenAGID}); got != goldenAGID {
		t.Errorf("healthKey = %q, want the AGID", got)
	}
	if got := healthKey(Player{Name: goldenName}); got != goldenName {
		t.Errorf("healthKey without an AGID = %q, want the name", got)
	}
}

// TestApplyHealthLeavesUnsampledPlayersUnknown is the rendering contract: a
// player nobody has asked about yet must reach the page as unknown, never as a
// zero the page would draw as an emergency.
func TestApplyHealthLeavesUnsampledPlayersUnknown(t *testing.T) {
	p := &Poller{healthCache: map[string]healthEntry{
		goldenAGID: {health: 96.534752, hasHealth: true, maxHealth: 850, hasMax: true, sampledAt: time.Now()},
		// Sampled, but the maximum never arrived: no percentage is computable,
		// so this player is unknown too rather than 96%.
		"111-222-333": {health: 40, hasHealth: true},
	}}
	list := []Player{
		{Name: goldenName, AGID: goldenAGID},
		{Name: "rex", AGID: "111-222-333"},
		{Name: "newcomer", AGID: "999-999-999"},
	}
	p.applyHealth(list, time.Now())

	for _, player := range list {
		if player.Name == goldenName {
			if !player.HasHealth || player.HealthPercent < 11 || player.HealthPercent > 12 {
				t.Errorf("sampled player = %+v", player)
			}
			continue
		}
		if player.HasHealth || player.Health != 0 || player.HealthPercent != 0 {
			t.Errorf("%s = %+v, want unknown health", player.Name, player)
		}
	}
}

// TestPruneHealthKeepsPartialRosters guards the expensive readings: a partial
// response is missing players who are still online, and evicting them would
// turn one lost page into a wave of paginating re-reads.
func TestPruneHealthKeepsPartialRosters(t *testing.T) {
	entries := func() map[string]healthEntry {
		return map[string]healthEntry{
			goldenAGID:    {hasHealth: true, hasMax: true},
			"111-222-333": {hasHealth: true, hasMax: true},
		}
	}
	list := []Player{{Name: goldenName, AGID: goldenAGID}}

	p := &Poller{healthCache: entries()}
	p.pruneHealth(list, false)
	if len(p.healthCache) != 2 {
		t.Errorf("an incomplete roster evicted %d entries", 2-len(p.healthCache))
	}

	p = &Poller{healthCache: entries()}
	p.pruneHealth(list, true)
	if len(p.healthCache) != 1 {
		t.Errorf("cache holds %d entries after a complete roster of 1", len(p.healthCache))
	}
	if _, ok := p.healthCache[goldenAGID]; !ok {
		t.Error("prune dropped a player who is still online")
	}
}

// TestHealthTargetsRotate is the fairness property the budget rests on: the
// least recently tried players go first, never-tried before everyone, and no
// more than the budget in one cycle.
func TestHealthTargetsRotate(t *testing.T) {
	now := time.Now()
	p := &Poller{healthPerPoll: 2, healthCache: map[string]healthEntry{
		"a": {triedAt: now},
		"b": {triedAt: now.Add(-time.Minute)},
		"c": {triedAt: now.Add(-time.Hour)},
	}}
	list := []Player{
		{Name: "a", AGID: "a"},
		{Name: "b", AGID: "b"},
		{Name: "c", AGID: "c"},
		{Name: "fresh", AGID: "fresh"},
		// No name, so no command can name them: they are not a target at all.
		{AGID: "nameless"},
	}

	got := p.healthTargets(list)
	if len(got) != 2 {
		t.Fatalf("picked %d targets, want the budget of 2", len(got))
	}
	if list[got[0]].Name != "fresh" {
		t.Errorf("first target is %q, want the never-sampled player", list[got[0]].Name)
	}
	if list[got[1]].Name != "c" {
		t.Errorf("second target is %q, want the oldest reading", list[got[1]].Name)
	}

	p.healthPerPoll = 10
	for _, i := range p.healthTargets(list) {
		if list[i].Name == "" {
			t.Error("a nameless player was picked; no command can ask about them")
		}
	}
}
