package players

import (
	"context"
	"log/slog"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Health and stamina are the player attributes PlayerInfoAll does not carry:
// they only answer per player, through the attribute commands. Both answer
// shapes are verified against a live server:
//
//	(GetAllAttr kittykat95): LocomotionState=3.000000, Health=96.534752, MaxHealth=850.000000, HealthRecoveryRate=1.900000, ... Stamina=33.199955, MaxStamina=100.000000, ... Growth=1.000000, ...
//	(GetAttr kittykat95 Health): Property health is 96.708755.
//
// GetAllAttr answers about 4200 bytes — two RCON round trips once the client
// pages it — and carries every value and maximum at once. GetAttr answers in
// one round trip with a single value, its name lower-cased in the message.
// refreshHealth explains why one GetAllAttr beats a GetAttr per value.
const commandGetAllAttr = "GetAllAttr"

// noPawnMarker is how the game answers for a player who has no body right now:
// sitting in a menu, or dead and not yet respawned. It is a normal state, not
// a failure, and must never be logged as one — otherwise every player who
// opens the map screen writes a warning line.
const noPawnMarker = "no player pawn"

// vital is one paired reading: an absolute value and the maximum it is a
// fraction of. Health and stamina are the same shape, and holding them in the
// same type is what keeps the percentage rule — always value over max, never
// the raw value — from having to be restated once per vital and got wrong once
// per vital.
//
// Each half carries its own "was it there" flag, because a missing key has to
// leave the reading unknown rather than zero: zero health rendered as 0% is an
// emergency the page would be inventing.
type vital struct {
	value    float64
	max      float64
	hasValue bool
	hasMax   bool
}

// percent returns the reading as 0–100 of its maximum, and false when there is
// no honest answer — no value, no maximum, or a maximum that cannot be divided
// by.
func (v vital) percent() (float64, bool) {
	if !v.hasValue || !v.hasMax {
		return 0, false
	}
	return percentOf(v.value, v.max)
}

// adopt takes a fresh reading, falling back to the cached maximum when the
// answer carried a value but no maximum: a value without its maximum makes no
// percentage at all, and maxima barely move, so the remembered one beats
// nothing. The value itself is never inherited — we just asked, and what came
// back is the truth, including the truth that nothing came back.
func adopt(cached, fresh vital) vital {
	if !fresh.hasMax && cached.hasMax {
		fresh.max, fresh.hasMax = cached.max, true
	}
	return fresh
}

// attrs is what one attribute answer yielded.
type attrs struct {
	health  vital
	stamina vital
	// noPawn is the game reporting that the player is not spawned.
	noPawn bool
}

// healthEntry is one player's last known vitals. It deliberately outlives the
// poll that fetched it: only a few players are sampled per cycle, so every
// snapshot is decorated from this cache rather than from the answers of the
// cycle that published it.
type healthEntry struct {
	health  vital
	stamina vital
	// sampledAt is when the vitals last had a value; triedAt is when they were
	// last asked for. They diverge while a player has no pawn or the game
	// refuses to answer, and keeping them apart does two jobs: rotation runs off
	// triedAt, so a player who can never be sampled cannot starve everyone
	// behind them, while the age the page shows runs off sampledAt and stays
	// honest.
	sampledAt time.Time
	triedAt   time.Time
}

//nolint:gochecknoglobals // compiled once; the patterns are protocol constants
var (
	// The command echo prefix, stripped so the first "Key=Value" pair is not
	// glued to it.
	attrEchoRE = regexp.MustCompile(`^\([^)\n]*\):\s*`)
	// The single-value form. The game lower-cases the attribute name in the
	// message ("Property health is"), so the name is matched case-insensitively
	// and normalised before use, and the trailing sentence period is kept out
	// of the number.
	attrPropertyRE = regexp.MustCompile(`(?i)Property\s+([A-Za-z]+)\s+is\s+(-?[0-9]+(?:\.[0-9]+)?)`)
)

// parseAttrs reads either attribute answer shape. Like Parse, it never fails:
// an unrecognised layout yields no readings rather than a wrong one, because a
// vital the page marks as unknown is safe and one the page invents is not.
//
// The single-value form is still accepted even though the poller no longer
// asks that way. It is verified protocol, it costs one regex, and an operator
// pasting a GetAttr answer into a test — or a future cheap path — must not need
// the parser changed first.
func parseAttrs(raw string) attrs {
	var a attrs
	raw = strings.ReplaceAll(raw, "\r", "")
	if strings.Contains(strings.ToLower(raw), noPawnMarker) {
		a.noPawn = true
		return a
	}

	body := attrEchoRE.ReplaceAllString(raw, "")
	// The GetAttr form first: one sentence per value.
	for _, m := range attrPropertyRE.FindAllStringSubmatch(body, -1) {
		a.set(m[1], m[2])
	}
	// Then the GetAllAttr form: about 150 comma-separated Key=Value pairs, of
	// which four matter. Unknown keys are ignored so a game update adding
	// attributes — or a page seam eating one — costs nothing.
	for field := range strings.SplitSeq(body, ",") {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		a.set(key, value)
	}
	return a
}

// set records one key if it is one of the four that matter. A value that does
// not parse leaves its flag false: an unreadable number is not a reading.
func (a *attrs) set(key, value string) {
	value = strings.TrimSuffix(strings.TrimSpace(value), ".")
	f, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return
	}
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "health":
		a.health.value, a.health.hasValue = f, true
	case "maxhealth":
		a.health.max, a.health.hasMax = f, true
	case "stamina":
		a.stamina.value, a.stamina.hasValue = f, true
	case "maxstamina":
		a.stamina.max, a.stamina.hasMax = f, true
	}
}

// percentOf converts an absolute reading into 0–100 of its maximum.
//
// This is the most dangerous arithmetic in the application to get wrong. A live
// server answered Health=96.534752 with MaxHealth=850: read as a percentage
// that player looks barely scratched, when they are in fact at 11% and one hit
// from dead. Stamina hides the same trap behind a coincidence — its maximum was
// 100 on the server we watched, so the raw value and the percentage happened to
// agree, and a build that scales stamina differently would expose anyone who
// leaned on that. So every reading is divided by its own maximum, and a maximum
// that is missing, zero, or nonsense yields no percentage at all rather than a
// flattering one.
func percentOf(value, maxValue float64) (float64, bool) {
	if math.IsNaN(value) || math.IsInf(value, 0) ||
		math.IsNaN(maxValue) || math.IsInf(maxValue, 0) || maxValue <= 0 {
		return 0, false
	}
	// Clamped so the page's colour bands stay total: they end at exactly 100,
	// and a server reporting overheal would otherwise land in no band at all.
	return min(100, max(0, value/maxValue*100)), true
}

// healthKey identifies a player in the health cache: the AGID, which is the
// server's own stable per-player identifier, falling back to the name when a
// torn record cost us the AGID. The page keys its markers the same way.
func healthKey(p Player) string {
	if p.AGID != "" {
		return p.AGID
	}
	return p.Name
}

// refreshHealth samples the vitals of a bounded slice of the roster and updates
// the cache. It is the answer to the one problem health and stamina pose on
// this stack: they only answer per player, so the obvious implementation issues
// a command per player per poll — at 100 players, 100 game-thread round trips
// every cycle, which is precisely the tax this whole application exists to
// avoid.
//
// The cost is therefore fixed rather than proportional to population. Per poll
// it is at most healthPerPoll attribute commands — one GetAllAttr per sampled
// player, each on one connection with one authentication — and because that
// answer runs about 4200 bytes the client follows it with a Page: fetch on the
// same connection, so each sample is roughly two game-thread exchanges. At the
// default budget of 4: 4 connections, 4 gathers, ~8 exchanges per poll, whether
// 5 players are online or 500. Players are sampled least recently tried first,
// so everyone's vitals refresh once every ceil(players/healthPerPoll) cycles:
// positions keep their full cadence, vitals age gracefully and carry their own
// age so nobody has to guess. On a server small enough for the budget to cover
// everyone, vitals are as fresh as position for free.
//
// One GetAllAttr rather than a GetAttr per value, now that stamina doubles the
// values wanted:
//
//   - Two GetAttr calls are two connections, two authentications and two full
//     attribute gathers on the game thread, for the same two exchanges. The
//     Page: follow-up GetAllAttr needs is a read out of a buffer the game has
//     already built, on a connection that is already open — the cheap half of
//     the exchange, not a second gather.
//   - GetAllAttr carries both maxima every time, so a maximum can never sit
//     stale: growth, a species swap, a rebalance patch, all heal on the next
//     sample with no invalidation rule to get wrong. The health-only design
//     needed one and it was the subtlest code here.
//
// The command count per poll is therefore unchanged from the health-only
// design, whose cold path was already GetAllAttr; only the page follow-ups are
// new, which is why the budget's default did not have to move.
//
// Failures are contained here and never reach the caller: the map must keep
// working exactly as it did before vitals existed.
func (p *Poller) refreshHealth(ctx context.Context, list []Player) {
	if p.healthPerPoll <= 0 {
		return
	}

	now := time.Now()
	sampled, unknown := 0, 0
	for _, i := range p.healthTargets(list) {
		player := list[i]
		key := healthKey(player)
		entry := p.healthCache[key]
		entry.triedAt = now

		body, err := p.execute(ctx, commandGetAllAttr+" "+player.Name)
		if err != nil {
			// An RCON-level failure here means the server is unreachable or
			// wedged, and the rest of the budget would spend one timeout each
			// discovering the same thing — delaying the next position poll for
			// no information. The entry keeps its last known values: failing to
			// ask is not evidence about the player.
			p.healthCache[key] = entry
			slog.Debug("vitals sampling stopped early", "player", player.Name, "error", err)
			break
		}

		a := parseAttrs(body)
		switch {
		case a.noPawn:
			// Not spawned. There are no vitals to report and nothing has gone
			// wrong; the readings go back to unknown so the page stops showing
			// figures from before they left their body. The maxima are kept:
			// they describe the body they will spawn into.
			entry.health.hasValue, entry.stamina.hasValue = false, false
			unknown++
		case a.health.hasValue || a.stamina.hasValue:
			// Each vital is adopted independently, so an answer carrying health
			// but no stamina degrades stamina alone rather than both.
			entry.health = adopt(entry.health, a.health)
			entry.stamina = adopt(entry.stamina, a.stamina)
			entry.sampledAt = now
			sampled++
		default:
			// An answer that carried no vitals at all: a build without the
			// command, or a layout change. Degrade to unknown, and stay at
			// debug — this repeats every cycle, so it must not be a warning.
			entry.health.hasValue, entry.stamina.hasValue = false, false
			unknown++
			slog.Debug("vitals answer carried no reading", "player", player.Name, "bytes", len(body))
		}
		p.healthCache[key] = entry
	}

	slog.Debug("vitals sampled", "players", sampled, "unknown", unknown,
		"budget", p.healthPerPoll, "roster", len(list))
}

// healthTargets picks which players this cycle samples: the ones tried longest
// ago, never-tried first — their zero time sorts before every real one, so a
// player who just joined gets a reading on the next poll.
//
// Ordering by time rather than walking an index cursor is what keeps the
// rotation fair across joins and leaves. A cursor into a list that changed
// under it re-samples the same few players and starves the rest, and the list
// changes constantly.
func (p *Poller) healthTargets(list []Player) []int {
	order := make([]int, 0, len(list))
	for i := range list {
		// The commands name a player by username, so a record damaged badly
		// enough to lose the name has nothing that can be asked.
		if list[i].Name == "" {
			continue
		}
		order = append(order, i)
	}
	slices.SortStableFunc(order, func(a, b int) int {
		return p.healthCache[healthKey(list[a])].triedAt.Compare(
			p.healthCache[healthKey(list[b])].triedAt)
	})
	return order[:min(len(order), p.healthPerPoll)]
}

// applyHealth stamps the cache onto a fresh snapshot. Every player is
// decorated, not only the ones sampled this cycle — vitals surviving between
// samples is the entire point of the cache. A player with nothing cached keeps
// HasHealth and HasStamina false, which the page renders as unknown rather than
// as 0%: an unsampled player must never look like a dying one. The two vitals
// are stamped independently, so one of them missing never suppresses the other.
func (p *Poller) applyHealth(list []Player, now time.Time) {
	for i := range list {
		entry, ok := p.healthCache[healthKey(list[i])]
		if !ok {
			continue
		}
		known := false
		if pct, ok := entry.health.percent(); ok {
			list[i].Health = entry.health.value
			list[i].MaxHealth = entry.health.max
			list[i].HealthPercent = pct
			list[i].HasHealth = true
			known = true
		}
		if pct, ok := entry.stamina.percent(); ok {
			list[i].Stamina = entry.stamina.value
			list[i].MaxStamina = entry.stamina.max
			list[i].StaminaPercent = pct
			list[i].HasStamina = true
			known = true
		}
		if known {
			// One age for both: they come out of the same answer, taken at the
			// same instant.
			list[i].HealthAgeSeconds = max(0, now.Sub(entry.sampledAt).Seconds())
		}
	}
}

// pruneHealth forgets players who are no longer online, so a server up for
// months does not accumulate an entry per player who ever visited.
//
// Only a complete snapshot may prune. A partial response is missing players
// who are still there, and evicting them would throw away readings that each
// cost a paginating command to obtain — a page loss would quietly turn into a
// wave of re-reads.
func (p *Poller) pruneHealth(list []Player, complete bool) {
	if !complete {
		return
	}
	online := make(map[string]struct{}, len(list))
	for i := range list {
		online[healthKey(list[i])] = struct{}{}
	}
	for key := range p.healthCache {
		if _, ok := online[key]; !ok {
			delete(p.healthCache, key)
		}
	}
}
