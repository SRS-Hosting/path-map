// Package players turns Path of Titans RCON output into positioned players
// and keeps the latest snapshot warm for the web layer.
package players

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Player is one in-game player as reported by PlayerInfoAll.
type Player struct {
	Name     string `json:"name"`
	AGID     string `json:"agid"`
	Dinosaur string `json:"dinosaur"`
	Role     string `json:"role"`
	// Marks is in-game currency. Not position data, but the roster shows it
	// because it is free: it is already in every record.
	Marks  int     `json:"marks"`
	Growth float64 `json:"growth"`
	// X, Y, Z are Unreal world units (centimetres).
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
	// U, V are the position projected onto the unit square of the map image.
	// They can fall slightly outside [0, 1]: players can stand past the
	// painted edge, and the page clamps only for display.
	U float64 `json:"u"`
	V float64 `json:"v"`
	// HasPosition is false when the record's Location could not be parsed;
	// the player is still listed, just not drawn.
	HasPosition bool `json:"hasPosition"`
	// Health and MaxHealth are absolute hit points, never percentages. A live
	// server answered Health=96.534752 with MaxHealth=850: that player was at
	// 11%, not 96%. They come from the per-player attribute commands rather
	// than PlayerInfoAll, a few players per poll, so they are older than the
	// position beside them by design; see health.go.
	Health    float64 `json:"health"`
	MaxHealth float64 `json:"maxHealth"`
	// HealthPercent is Health/MaxHealth as 0–100, and is the only one of the
	// three figures anything may be coloured or sized by.
	HealthPercent float64 `json:"healthPercent"`
	// HasHealth is false when health has not been sampled yet, when the player
	// has no pawn, and when health sampling is switched off. All three render
	// as unknown: a zero shown as 0% paints a healthy player as dying.
	HasHealth bool `json:"hasHealth"`
	// HealthAgeSeconds is how old the health reading was when the snapshot was
	// built. Health refreshes a few players at a time, so its age is not the
	// snapshot's age, and this field is the only thing that knows the
	// difference. Omitted when there is no reading to age.
	HealthAgeSeconds float64 `json:"healthAgeSeconds,omitempty"`
}

// Snapshot is one complete poll result.
type Snapshot struct {
	GeneratedAt time.Time `json:"generatedAt"`
	// Total is the game's own "Total Players: N." count, -1 when the header
	// was missing. It is the parser's integrity check: without it, a format
	// change would read as an empty server instead of a broken parser.
	Total    int      `json:"total"`
	Complete bool     `json:"complete"`
	Players  []Player `json:"players"`
}

// MapInfo identifies the resolved map: what the page shows and how world
// coordinates project onto its image.
type MapInfo struct {
	Name        string  `json:"name"`
	DisplayName string  `json:"displayName"`
	HalfExtentX float64 `json:"-"`
	HalfExtentY float64 `json:"-"`
	// ImageFile is the basename served when the image path is a directory.
	ImageFile string `json:"-"`
}

// The verified record shapes, from a live server. Single-player PlayerInfo
// echoes the command per record:
//
//	(PlayerInfo kittykat95): Name: kittykat95 / AGID: 746-132-258 / Dinosaur: Hatzegopteryx / Role: None / Marks: 2715 / Growth: 1 / Location: (X=-67904.590 Y=-237666.790 Z=-297.420)
//
// PlayerInfoAll echoes once — "(PlayerInfoAll): Total Players: N. " on the
// first line — and then emits each record bare, starting at "Name: " on its
// own line. The "(PlayerInfo name):" prefix was always the command echo, not
// part of the record, which is why matching it alone found nothing in
// PlayerInfoAll output. Records are located by either opener; byte-exact
// page reassembly preserves the game's line structure, so a record torn
// across a page seam still starts where the game started it.
//
//nolint:gochecknoglobals // compiled once; the patterns are protocol constants
var (
	recordStartRE = regexp.MustCompile(`(?m)\(PlayerInfo |^Name: `)
	recordEchoRE  = regexp.MustCompile(`^\(PlayerInfo (.+?)\): ?`)
	totalRE       = regexp.MustCompile(`Total Players:\s*(\d+)`)
	// The location is searched for by its coordinate shape anywhere in the
	// record rather than through the "Location:" field label, so a label torn
	// across a page seam still yields a position.
	locationRE = regexp.MustCompile(`X=(-?[0-9.]+)\s+Y=(-?[0-9.]+)\s+Z=(-?[0-9.]+)`)
)

// Parse extracts players from PlayerInfoAll output. It never fails: a layout
// surprise degrades to fewer parsed fields and Complete=false, because a map
// that renders what it could parse beats one that blanks on a format change.
func Parse(raw string) Snapshot {
	raw = strings.ReplaceAll(raw, "\r", "")
	// Players is non-nil from the start so an empty server serialises as []
	// rather than null.
	s := Snapshot{Total: -1, Players: []Player{}}

	if m := totalRE.FindStringSubmatch(raw); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil {
			s.Total = n
		}
	}

	starts := recordStartRE.FindAllStringIndex(raw, -1)
	for i, loc := range starts {
		end := len(raw)
		if i+1 < len(starts) {
			end = starts[i+1][0]
		}
		s.Players = append(s.Players, parseRecord(raw[loc[0]:end]))
	}

	s.Complete = s.Total >= 0 && len(s.Players) == s.Total
	return s
}

// parseRecord reads one "(PlayerInfo ...)" record. Field damage is contained
// per field: an unparsable value leaves its zero value rather than dropping
// the record, and unknown keys are ignored so a game update adding a field
// does not break anyone.
func parseRecord(record string) Player {
	var p Player

	rest := record
	if m := recordEchoRE.FindStringSubmatch(record); m != nil {
		// The echo prefix names the player too; keep it as a fallback in case
		// the Name field itself was damaged.
		p.Name = m[1]
		rest = record[len(m[0]):]
	}

	for field := range strings.SplitSeq(rest, " / ") {
		key, value, ok := strings.Cut(field, ": ")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		switch key {
		case "Name":
			if value != "" {
				p.Name = value
			}
		case "AGID":
			p.AGID = value
		case "Dinosaur":
			p.Dinosaur = value
		case "Role":
			p.Role = value
		case "Marks":
			if n, err := strconv.Atoi(value); err == nil {
				p.Marks = n
			}
		case "Growth":
			if f, err := strconv.ParseFloat(value, 64); err == nil {
				p.Growth = f
			}
		default:
			// Location is handled below by shape, and future fields are
			// deliberately ignored.
		}
	}

	if m := locationRE.FindStringSubmatch(record); m != nil {
		x, errX := strconv.ParseFloat(m[1], 64)
		y, errY := strconv.ParseFloat(m[2], 64)
		z, errZ := strconv.ParseFloat(m[3], 64)
		if errX == nil && errY == nil && errZ == nil {
			p.X, p.Y, p.Z = x, y, z
			p.HasPosition = true
		}
	}

	return p
}

// uv maps a world coordinate onto the unit square of the map image:
// u=(X+halfX)/(2*halfX) and v likewise for Y. The world origin sits at the
// image centre and the half extents are the per-axis distances to the edges,
// with no axis swap, flip, or rotation — verified empirically by overlaying
// 63 named landmarks onto the Gondwa image. Out-of-range results are
// preserved, not clamped: players past the painted edge are real.
func uv(x, y, halfX, halfY float64) (u, v float64) {
	return (x + halfX) / (2 * halfX), (y + halfY) / (2 * halfY)
}
