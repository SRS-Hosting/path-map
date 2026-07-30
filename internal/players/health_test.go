package players

import (
	"math"
	"testing"
	"time"
)

// goldenAllAttr is the GetAllAttr answer shape from a live server, cut from
// about 150 pairs down to the four that matter plus enough neighbours to prove
// the scan ignores what it does not know — including HealthRecoveryRate, which
// starts with a key we are looking for. The numbers are the real ones, and they
// are the point of this whole file: 96.5 health against 850 max is a player at
// 11%, not a player at 96%.
const goldenAllAttr = "(GetAllAttr kittykat95): LocomotionState=3.000000, Health=96.534752, " +
	"MaxHealth=850.000000, HealthRecoveryRate=1.900000, Hunger=412.000000, Thirst=380.500000, " +
	"Stamina=33.199955, MaxStamina=100.000000, Growth=1.000000, BleedingRate=0.000000"

// goldenGetAttr is the single-value shape. Note the lower-cased attribute name
// in the message: the game says "Property health is" for a "Health" query. The
// poller no longer asks this way, but the shape is verified protocol and the
// parser still accepts it.
const goldenGetAttr = "(GetAttr kittykat95 Health): Property health is 96.708755."

// The verified live player and her AGID, so every test that names one names the
// same one.
const (
	goldenName = "kittykat95"
	goldenAGID = "746-132-258"
)

// reading builds a complete vital, which is the only state a percentage can
// come out of.
func reading(value, maxValue float64) vital {
	return vital{value: value, max: maxValue, hasValue: true, hasMax: true}
}

func TestParseAttrsGetAllAttr(t *testing.T) {
	a := parseAttrs(goldenAllAttr)
	if !a.health.hasValue || a.health.value != 96.534752 {
		t.Errorf("health = %+v", a.health)
	}
	if !a.health.hasMax || a.health.max != 850 {
		t.Errorf("maxHealth = %+v", a.health)
	}
	// Both vitals come out of this one answer — that is the whole reason it is
	// the only command the poller spends.
	if !a.stamina.hasValue || a.stamina.value != 33.199955 {
		t.Errorf("stamina = %+v", a.stamina)
	}
	if !a.stamina.hasMax || a.stamina.max != 100 {
		t.Errorf("maxStamina = %+v", a.stamina)
	}
	if a.noPawn {
		t.Error("noPawn = true for a spawned player")
	}
}

func TestParseAttrsGetAttr(t *testing.T) {
	a := parseAttrs(goldenGetAttr)
	if !a.health.hasValue || a.health.value != 96.708755 {
		t.Errorf("health = %+v: the trailing period must stay out of the number", a.health)
	}
	// A Health query says nothing about the maximum, and pretending otherwise
	// would make the percentage a division by zero.
	if a.health.hasMax {
		t.Errorf("maxHealth = %v from a Health query", a.health.max)
	}
	if a.stamina.hasValue {
		t.Errorf("stamina = %v from a Health query", a.stamina.value)
	}

	// The MaxHealth and Stamina wordings are unverified, so every plausible
	// spelling of the property name must land.
	for raw, want := range map[string]vital{
		"(GetAttr kittykat95 MaxHealth): Property maxhealth is 850.000000.": {max: 850, hasMax: true},
		"(GetAttr kittykat95 MaxHealth): Property MaxHealth is 850.":        {max: 850, hasMax: true},
	} {
		if got := parseAttrs(raw).health; got != want {
			t.Errorf("parseAttrs(%q).health = %+v, want %+v", raw, got, want)
		}
	}
	if got := parseAttrs("(GetAttr kittykat95 Stamina): Property stamina is 33.199955.").stamina; !got.hasValue || got.value != 33.199955 {
		t.Errorf("stamina from a single-value answer = %+v", got)
	}
}

// TestParseAttrsNoPawn covers a player in the menus or freshly dead. It is a
// normal state, so it must read as "no vitals" and never as a failure.
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
		if a.health.hasValue || a.health.hasMax || a.stamina.hasValue || a.stamina.hasMax {
			t.Errorf("parseAttrs(%q) invented a reading: %+v", raw, a)
		}
	}
}

func TestParseAttrsGarbage(t *testing.T) {
	for _, raw := range []string{
		"",
		"That command does not exist",
		"(GetAttr kittykat95 Health): Property health is banana.",
		// A page seam landing mid-key, and keys with no values at all.
		"(GetAllAttr kittykat95): LocomotionState=3.000000, Hea",
		"(GetAllAttr kittykat95): Health=, MaxHealth=, Stamina=, MaxStamina=",
		"Health",
	} {
		a := parseAttrs(raw)
		if a.health.hasValue || a.health.hasMax || a.stamina.hasValue || a.stamina.hasMax {
			t.Errorf("parseAttrs(%q) = %+v, want no readings", raw, a)
		}
	}
}

// TestParseAttrsPartialAnswer is the per-field degradation rule: one vital
// missing from an answer must cost that vital only.
func TestParseAttrsPartialAnswer(t *testing.T) {
	a := parseAttrs("(GetAllAttr kittykat95): Health=96.534752, MaxHealth=850.000000")
	if !a.health.hasValue || !a.health.hasMax {
		t.Errorf("health = %+v, want the reading that did arrive", a.health)
	}
	if a.stamina.hasValue || a.stamina.hasMax {
		t.Errorf("stamina = %+v, want unknown", a.stamina)
	}

	// And the mirror: a build that reports stamina but not health.
	a = parseAttrs("(GetAllAttr kittykat95): Stamina=33.199955, MaxStamina=100.000000")
	if a.health.hasValue || a.health.hasMax {
		t.Errorf("health = %+v, want unknown", a.health)
	}
	if !a.stamina.hasValue || a.stamina.value != 33.199955 {
		t.Errorf("stamina = %+v", a.stamina)
	}
}

// TestPercentIsRelativeToMax is the trap this feature lives or dies on. Both
// vitals are absolute: the live health reading of 96.5 belongs to a player at
// 11% of an 850 maximum. Anything that treats the raw value as a percentage
// paints them bright green while they are one bite from dead, so this test fails
// loudly rather than subtly.
func TestPercentIsRelativeToMax(t *testing.T) {
	pct, ok := percentOf(96.534752, 850)
	if !ok {
		t.Fatal("percentOf refused the verified live reading")
	}
	if pct < 11 || pct > 12 {
		t.Errorf("percentOf(96.534752, 850) = %v, want ~11.4; health is absolute hit points, "+
			"not a percentage, and reading it as one shows a dying player as healthy", pct)
	}

	// The same trap one layer up: parsed straight from the wire, the reading
	// must still land in the red band rather than the green one.
	a := parseAttrs(goldenAllAttr)
	pct, ok = a.health.percent()
	if !ok || pct >= 33 {
		t.Errorf("parsed live health is %v%%, want under 33 (the red band)", pct)
	}
}

// TestStaminaPercentDoesNotAssumeAMaximumOfOneHundred is the same trap wearing a
// disguise. The server we watched reported MaxStamina=100, so stamina's absolute
// value and its percentage happen to be the same number and a missing division
// would look correct forever — until a build, a mod, or a species scales stamina
// differently. This pins the division itself.
func TestStaminaPercentDoesNotAssumeAMaximumOfOneHundred(t *testing.T) {
	a := parseAttrs("(GetAllAttr kittykat95): Stamina=33.199955, MaxStamina=250.000000")
	pct, ok := a.stamina.percent()
	if !ok {
		t.Fatal("no stamina percentage from a complete reading")
	}
	if pct < 13 || pct > 14 {
		t.Errorf("stamina 33.199955 of 250 = %v%%, want ~13.3; the raw value is not a percentage "+
			"just because the maximum happened to be 100 on one server", pct)
	}

	// And with the observed maximum, the coincidence holds — which is exactly
	// why it cannot be the only case tested.
	if pct, _ := parseAttrs(goldenAllAttr).stamina.percent(); pct < 33 || pct > 34 {
		t.Errorf("stamina 33.199955 of 100 = %v%%, want ~33.2", pct)
	}
}

func TestPercentOf(t *testing.T) {
	tests := []struct {
		name            string
		value, maxValue float64
		want            float64
		wantOK          bool
	}{
		{"full is exactly 100", 850, 850, 100, true},
		{"half", 425, 850, 50, true},
		{"empty", 0, 850, 0, true},
		{"almost empty", 8.5, 850, 1, true},
		// The bands end at 100, so overheal is clamped rather than left to fall
		// outside every one of them.
		{"overheal clamps", 900, 850, 100, true},
		{"negative clamps", -5, 850, 0, true},
		// No maximum means no percentage: a division by zero would surface as
		// +Inf and paint a full green marker.
		{"no maximum", 96.5, 0, 0, false},
		{"negative maximum", 96.5, -850, 0, false},
		{"NaN maximum", 96.5, math.NaN(), 0, false},
		{"NaN value", math.NaN(), 850, 0, false},
		{"infinite value", math.Inf(1), 850, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := percentOf(tc.value, tc.maxValue)
			if ok != tc.wantOK || got != tc.want {
				t.Errorf("percentOf(%v, %v) = %v, %v; want %v, %v",
					tc.value, tc.maxValue, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// TestVitalPercentNeedsBothHalves covers the states the wire can produce that
// arithmetic alone cannot judge: half a reading is not a reading.
func TestVitalPercentNeedsBothHalves(t *testing.T) {
	for name, v := range map[string]vital{
		"nothing at all":    {},
		"value without max": {value: 96.5, hasValue: true},
		"max without value": {max: 850, hasMax: true},
	} {
		if pct, ok := v.percent(); ok {
			t.Errorf("%s yielded %v%%; an incomplete reading must have no percentage", name, pct)
		}
	}
	if pct, ok := reading(96.534752, 850).percent(); !ok || pct < 11 || pct > 12 {
		t.Errorf("complete reading = %v, %v", pct, ok)
	}
}

// TestAdoptFallsBackToTheCachedMaximum covers the degradation path for an answer
// that carries a value without its maximum: the remembered maximum is better
// than no percentage at all, while the value itself is never inherited — a
// sample that came back empty is news, not a reason to keep showing old numbers.
func TestAdoptFallsBackToTheCachedMaximum(t *testing.T) {
	cached := reading(96.534752, 850)

	fresh := adopt(cached, vital{value: 42, hasValue: true})
	if !fresh.hasMax || fresh.max != 850 || fresh.value != 42 {
		t.Errorf("adopt = %+v, want the fresh value against the cached maximum", fresh)
	}

	// A fresh maximum always wins: this is what heals a stale one after a
	// rebalance or a species swap, with no invalidation rule to get wrong.
	fresh = adopt(cached, reading(42, 1200))
	if fresh.max != 1200 {
		t.Errorf("adopt kept the stale maximum %v", fresh.max)
	}

	// Nothing came back: the reading goes unknown and only the maximum survives.
	fresh = adopt(cached, vital{})
	if fresh.hasValue || !fresh.hasMax || fresh.max != 850 {
		t.Errorf("adopt = %+v, want unknown against the cached maximum", fresh)
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
// zero the page would draw as an emergency. It also covers the two vitals
// degrading independently.
func TestApplyHealthLeavesUnsampledPlayersUnknown(t *testing.T) {
	p := &Poller{healthCache: map[string]healthEntry{
		goldenAGID: {health: reading(96.534752, 850), stamina: reading(33.199955, 100), sampledAt: time.Now()},
		// Health arrived without its maximum and stamina arrived whole: no health
		// percentage is computable, so health is unknown while stamina is not.
		"111-222-333": {health: vital{value: 40, hasValue: true}, stamina: reading(50, 200), sampledAt: time.Now()},
	}}
	list := []Player{
		{Name: goldenName, AGID: goldenAGID},
		{Name: "rex", AGID: "111-222-333"},
		{Name: "newcomer", AGID: "999-999-999"},
	}
	p.applyHealth(list, time.Now())

	if len(list) != 3 {
		t.Fatalf("the fixture lost a player: %d", len(list))
	}
	kitty := list[0]
	if !kitty.HasHealth || kitty.HealthPercent < 11 || kitty.HealthPercent > 12 {
		t.Errorf("sampled health = %+v", kitty)
	}
	if !kitty.HasStamina || kitty.StaminaPercent < 33 || kitty.StaminaPercent > 34 {
		t.Errorf("sampled stamina = %+v", kitty)
	}

	rex := list[1]
	if rex.HasHealth || rex.Health != 0 || rex.HealthPercent != 0 {
		t.Errorf("rex health = %+v, want unknown without a maximum", rex)
	}
	if !rex.HasStamina || rex.StaminaPercent != 25 {
		t.Errorf("rex stamina = %+v, want 25%%: one vital missing must not suppress the other", rex)
	}

	newcomer := list[2]
	if newcomer.HasHealth || newcomer.HasStamina || newcomer.HealthAgeSeconds != 0 {
		t.Errorf("newcomer = %+v, want everything unknown", newcomer)
	}
}

// TestPruneHealthKeepsPartialRosters guards the readings: a partial response is
// missing players who are still online, and evicting them would turn one lost
// page into a wave of paginating re-reads.
func TestPruneHealthKeepsPartialRosters(t *testing.T) {
	entries := func() map[string]healthEntry {
		return map[string]healthEntry{
			goldenAGID:    {health: reading(96.5, 850)},
			"111-222-333": {health: reading(40, 850)},
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
