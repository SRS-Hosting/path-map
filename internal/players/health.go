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

// Health is the one player attribute PlayerInfoAll does not carry: it only
// answers per player, through the attribute commands. Both shapes are verified
// against a live server:
//
//	(GetAllAttr kittykat95): LocomotionState=3.000000, Health=96.534752, MaxHealth=850.000000, HealthRecoveryRate=1.900000, ... Growth=1.000000, ...
//	(GetAttr kittykat95 Health): Property health is 96.708755.
//
// GetAllAttr answers about 4200 bytes — two RCON round trips once the client
// pages it — and carries MaxHealth alongside Health. GetAttr answers in one
// round trip with a single value, its name lower-cased in the message. That
// asymmetry is the whole reason MaxHealth is cached rather than re-read; see
// refreshHealth.
const (
	commandGetAllAttr = "GetAllAttr"
	commandGetAttr    = "GetAttr"
	attrHealth        = "Health"
)

// noPawnMarker is how the game answers for a player who has no body right now:
// sitting in a menu, or dead and not yet respawned. It is a normal state, not
// a failure, and must never be logged as one — otherwise every player who
// opens the map screen writes a warning line.
const noPawnMarker = "no player pawn"

// growthEpsilon is how far Growth may drift before a cached MaxHealth is
// re-read. MaxHealth is a function of species and growth, so it moves only as
// a player grows, and growth creeps continuously: an exact comparison would
// spend the expensive paginating command on every sample of every growing
// player. Half a percent of growth moves MaxHealth by far less than the width
// of one colour band on the page.
const growthEpsilon = 0.005

// attrs is what one attribute answer yielded. Every value carries its own
// "was it there" flag, because a missing key has to leave health unknown
// rather than zero: zero health rendered as 0% is an emergency the page would
// be inventing.
type attrs struct {
	health       float64
	hasHealth    bool
	maxHealth    float64
	hasMaxHealth bool
	// noPawn is the game reporting that the player is not spawned.
	noPawn bool
}

// healthEntry is one player's last known health. It deliberately outlives the
// poll that fetched it: only a few players are sampled per cycle, so every
// snapshot is decorated from this cache rather than from the answers of the
// cycle that published it.
type healthEntry struct {
	health    float64
	hasHealth bool
	maxHealth float64
	hasMax    bool
	// growthAtMax and speciesAtMax are the readings MaxHealth was sampled
	// against, and together they are what invalidates it: a maximum is a
	// function of species and growth. Watching growth alone would let a player
	// who swapped dinosaur at the same growth keep the maximum of the body they
	// left, and a wrong maximum is a wrong colour on the map.
	growthAtMax  float64
	speciesAtMax string
	// sampledAt is when health last had a value; triedAt is when it was last
	// asked for. They diverge while a player has no pawn or the game refuses to
	// answer, and keeping them apart does two jobs: rotation runs off triedAt,
	// so a player who can never be sampled cannot starve everyone behind them,
	// while the age the page shows runs off sampledAt and stays honest.
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
// an unrecognised layout yields no readings rather than a wrong one, because
// health the page marks as unknown is safe and health the page invents is not.
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
	// which two matter. Unknown keys are ignored so a game update adding
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

// set records one key if it is one of the two that matter. A value that does
// not parse leaves its flag false: an unreadable number is not a reading.
func (a *attrs) set(key, value string) {
	value = strings.TrimSuffix(strings.TrimSpace(value), ".")
	f, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return
	}
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "health":
		a.health, a.hasHealth = f, true
	case "maxhealth":
		a.maxHealth, a.hasMaxHealth = f, true
	}
}

// healthPercent converts an absolute health reading into 0–100 of maximum.
//
// This is the most dangerous number in the application to get wrong. A live
// server answered Health=96.534752 with MaxHealth=850: read as a percentage
// that player looks barely scratched, when they are in fact at 11% and one hit
// from dead. Health is therefore always divided by MaxHealth, and a MaxHealth
// that is missing, zero, or nonsense yields no percentage at all rather than a
// flattering one.
func healthPercent(health, maxHealth float64) (float64, bool) {
	if math.IsNaN(health) || math.IsInf(health, 0) ||
		math.IsNaN(maxHealth) || math.IsInf(maxHealth, 0) || maxHealth <= 0 {
		return 0, false
	}
	// Clamped so the page's colour bands stay total: they end at exactly 100,
	// and a server reporting overheal would otherwise land in no band at all.
	return min(100, max(0, health/maxHealth*100)), true
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

// refreshHealth samples health for a bounded slice of the roster and updates
// the cache. It is the answer to the one problem health poses on this stack:
// health only answers per player, so the obvious implementation issues one
// command per player per poll — at 100 players, 100 game-thread round trips
// every cycle, which is precisely the tax this whole application exists to
// avoid.
//
// The cost is therefore fixed rather than proportional. At most healthPerPoll
// players are sampled per poll, least recently tried first, so every player's
// health refreshes once every ceil(players/healthPerPoll) cycles: positions
// keep their full cadence and health ages gracefully, carrying its own age so
// nobody has to guess. On a server small enough for the budget to cover
// everyone, health is as fresh as position for free.
//
// Steady state is one GetAttr per sampled player, one round trip. GetAllAttr
// costs two because its answer pages, and is spent only when MaxHealth is
// unknown or the player has grown since it was read — without MaxHealth a
// health figure cannot be turned into a percentage at all.
//
// Failures are contained here and never reach the caller: the map must keep
// working exactly as it did before health existed.
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

		body, err := p.execute(ctx, healthCommand(player, entry))
		if err != nil {
			// An RCON-level failure here means the server is unreachable or
			// wedged, and the rest of the budget would spend one timeout each
			// discovering the same thing — delaying the next position poll for
			// no information. The entry keeps its last known value: failing to
			// ask is not evidence about the player.
			p.healthCache[key] = entry
			slog.Debug("health sampling stopped early", "player", player.Name, "error", err)
			break
		}

		a := parseAttrs(body)
		switch {
		case a.noPawn:
			// Not spawned. There is no health to report and nothing has gone
			// wrong; the reading goes back to unknown so the page stops showing
			// a figure from before they left their body.
			entry.hasHealth = false
			unknown++
		case a.hasHealth:
			entry.health, entry.hasHealth = a.health, true
			entry.sampledAt = now
			sampled++
		default:
			// An answer that carried no health at all: a build without the
			// command, or a layout change. Degrade to unknown, and stay at
			// debug — this repeats every cycle, so it must not be a warning.
			entry.hasHealth = false
			unknown++
			slog.Debug("health answer carried no reading", "player", player.Name, "bytes", len(body))
		}
		if a.hasMaxHealth {
			entry.maxHealth, entry.hasMax = a.maxHealth, true
			entry.growthAtMax, entry.speciesAtMax = player.Growth, player.Dinosaur
		}
		p.healthCache[key] = entry
	}

	slog.Debug("health sampled", "players", sampled, "unknown", unknown,
		"budget", p.healthPerPoll, "roster", len(list))
}

// healthCommand picks the cheap or the expensive question for one player. The
// expensive one is only worth it when the percentage could not be computed
// without it: no cached maximum, or one that belongs to a body this player no
// longer has.
func healthCommand(player Player, entry healthEntry) string {
	if !entry.hasMax || entry.speciesAtMax != player.Dinosaur ||
		math.Abs(player.Growth-entry.growthAtMax) > growthEpsilon {
		return commandGetAllAttr + " " + player.Name
	}
	return commandGetAttr + " " + player.Name + " " + attrHealth
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
// decorated, not only the ones sampled this cycle — health surviving between
// samples is the entire point of the cache. A player with nothing cached keeps
// HasHealth false, which the page renders as unknown rather than as 0%: an
// unsampled player must never look like a dying one.
func (p *Poller) applyHealth(list []Player, now time.Time) {
	for i := range list {
		entry, ok := p.healthCache[healthKey(list[i])]
		if !ok || !entry.hasHealth || !entry.hasMax {
			continue
		}
		pct, ok := healthPercent(entry.health, entry.maxHealth)
		if !ok {
			continue
		}
		list[i].Health = entry.health
		list[i].MaxHealth = entry.maxHealth
		list[i].HealthPercent = pct
		list[i].HasHealth = true
		list[i].HealthAgeSeconds = max(0, now.Sub(entry.sampledAt).Seconds())
	}
}

// pruneHealth forgets players who are no longer online, so a server up for
// months does not accumulate an entry per player who ever visited.
//
// Only a complete snapshot may prune. A partial response is missing players
// who are still there, and evicting them would throw away MaxHealth readings
// that each cost a paginating command to obtain — a page loss would quietly
// turn into a wave of expensive re-reads.
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
