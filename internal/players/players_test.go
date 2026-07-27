package players

import (
	"strings"
	"testing"
)

// goldenRecord is the one player record shape verified against a live server;
// every parser test builds on it.
const goldenRecord = "(PlayerInfo kittykat95): Name: kittykat95 / AGID: 746-132-258 / Dinosaur: Hatzegopteryx / Role: None / Marks: 2715 / Growth: 1 / Location: (X=-67904.590 Y=-237666.790 Z=-297.420)"

func assertGolden(t *testing.T, p Player) {
	t.Helper()
	if p.Name != "kittykat95" {
		t.Errorf("Name = %q", p.Name)
	}
	if p.AGID != "746-132-258" {
		t.Errorf("AGID = %q", p.AGID)
	}
	if p.Dinosaur != "Hatzegopteryx" {
		t.Errorf("Dinosaur = %q", p.Dinosaur)
	}
	if p.Role != "None" {
		t.Errorf("Role = %q", p.Role)
	}
	if p.Marks != 2715 {
		t.Errorf("Marks = %d", p.Marks)
	}
	if p.Growth != 1 {
		t.Errorf("Growth = %v", p.Growth)
	}
	if !p.HasPosition {
		t.Fatal("HasPosition = false")
	}
	if p.X != -67904.590 || p.Y != -237666.790 || p.Z != -297.420 {
		t.Errorf("position = (%v, %v, %v)", p.X, p.Y, p.Z)
	}
}

func TestParseGoldenRecord(t *testing.T) {
	s := Parse("Total Players: 1.\n" + goldenRecord)
	if s.Total != 1 {
		t.Errorf("Total = %d", s.Total)
	}
	if !s.Complete {
		t.Error("Complete = false")
	}
	if len(s.Players) != 1 {
		t.Fatalf("parsed %d players", len(s.Players))
	}
	assertGolden(t, s.Players[0])
}

const secondRecord = "(PlayerInfo rex): Name: rex / AGID: 111-222-333 / Dinosaur: Tyrannosaurus / Role: Moderator / Marks: 10 / Growth: 0.75 / Location: (X=1000.0 Y=-2000.0 Z=30.0)"

// TestParseRecordSeparators is the point of the index-based splitter: the
// multi-player layout of PlayerInfoAll is unverified, so newline-separated,
// glued, and header-after-records layouts must all parse identically.
func TestParseRecordSeparators(t *testing.T) {
	layouts := map[string]string{
		"newline separated": "Total Players: 2.\n" + goldenRecord + "\n" + secondRecord,
		"glued":             "Total Players: 2.\n" + goldenRecord + secondRecord,
		"header last":       goldenRecord + "\n" + secondRecord + "\nTotal Players: 2.",
	}
	for name, raw := range layouts {
		t.Run(name, func(t *testing.T) {
			s := Parse(raw)
			if len(s.Players) != 2 {
				t.Fatalf("parsed %d players, want 2", len(s.Players))
			}
			if !s.Complete {
				t.Errorf("Complete = false (total %d)", s.Total)
			}
			assertGolden(t, s.Players[0])
			if s.Players[1].Name != "rex" || s.Players[1].Growth != 0.75 {
				t.Errorf("second player = %+v", s.Players[1])
			}
		})
	}
}

// TestParseTornField reproduces a page seam landing inside the "Location"
// label. The field key is destroyed but the coordinates survive, and the
// shape-based location search must still find them.
func TestParseTornField(t *testing.T) {
	torn := "(PlayerInfo rex): Name: rex / AGID: 111-222-333 / Loc\nation: (X=1000.0 Y=-2000.0 Z=30.0)"
	s := Parse("Total Players: 1.\n" + torn)
	if len(s.Players) != 1 {
		t.Fatalf("parsed %d players", len(s.Players))
	}
	p := s.Players[0]
	if !p.HasPosition {
		t.Fatal("HasPosition = false for a torn Location label")
	}
	if p.X != 1000.0 || p.Y != -2000.0 || p.Z != 30.0 {
		t.Errorf("position = (%v, %v, %v)", p.X, p.Y, p.Z)
	}
}

func TestParseTotalMismatch(t *testing.T) {
	s := Parse("Total Players: 3.\n" + goldenRecord)
	if s.Complete {
		t.Error("Complete = true with 1 of 3 records parsed")
	}
	if s.Total != 3 || len(s.Players) != 1 {
		t.Errorf("Total = %d, players = %d", s.Total, len(s.Players))
	}
}

func TestParseNoHeader(t *testing.T) {
	s := Parse(goldenRecord)
	if s.Total != -1 {
		t.Errorf("Total = %d, want -1 for a missing header", s.Total)
	}
	if s.Complete {
		t.Error("Complete = true without an integrity header")
	}
	if len(s.Players) != 1 {
		t.Errorf("parsed %d players", len(s.Players))
	}
}

func TestParseUnknownFieldsIgnored(t *testing.T) {
	raw := "Total Players: 1.\n(PlayerInfo rex): Name: rex / Hunger: 55 / Thirst: 90 / Dinosaur: Tyrannosaurus / Location: (X=1.0 Y=2.0 Z=3.0)"
	s := Parse(raw)
	if len(s.Players) != 1 {
		t.Fatalf("parsed %d players", len(s.Players))
	}
	if s.Players[0].Dinosaur != "Tyrannosaurus" {
		t.Errorf("Dinosaur = %q: unknown fields disturbed known ones", s.Players[0].Dinosaur)
	}
}

func TestParseMissingLocation(t *testing.T) {
	raw := "Total Players: 1.\n(PlayerInfo rex): Name: rex / Dinosaur: Tyrannosaurus"
	s := Parse(raw)
	if len(s.Players) != 1 {
		t.Fatalf("parsed %d players: positionless players must still be listed", len(s.Players))
	}
	if s.Players[0].HasPosition {
		t.Error("HasPosition = true without a location")
	}
}

func TestParseGarbage(t *testing.T) {
	for _, raw := range []string{"", "That command does not exist", "Total Players: 0."} {
		s := Parse(raw)
		if len(s.Players) != 0 {
			t.Errorf("Parse(%q) found %d players", raw, len(s.Players))
		}
		if s.Players == nil {
			t.Errorf("Parse(%q).Players is nil; it must serialise as [], not null", raw)
		}
	}
	if s := Parse("Total Players: 0."); !s.Complete {
		t.Error("an empty server with a matching header is Complete")
	}
}

func TestParseNameWithSpaces(t *testing.T) {
	raw := "Total Players: 1.\n(PlayerInfo Big Chungus): Name: Big Chungus / Dinosaur: Stegosaurus / Location: (X=1.0 Y=2.0 Z=3.0)"
	s := Parse(raw)
	if len(s.Players) != 1 || s.Players[0].Name != "Big Chungus" {
		t.Errorf("players = %+v", s.Players)
	}
}

func TestUV(t *testing.T) {
	maps := []struct {
		name         string
		halfX, halfY float64
	}{
		{"gondwa", 403446.75, 403857.03},
		{"panjura", 504000, 504000},
		{"riparia", 257650, 257650},
	}
	for _, m := range maps {
		t.Run(m.name, func(t *testing.T) {
			// The world origin is the image centre.
			if u, v := uv(0, 0, m.halfX, m.halfY); u != 0.5 || v != 0.5 {
				t.Errorf("uv(origin) = (%v, %v), want (0.5, 0.5)", u, v)
			}
			// The extents are the edges: min maps to 0, max to 1, on each
			// axis independently.
			if u, v := uv(-m.halfX, -m.halfY, m.halfX, m.halfY); u != 0 || v != 0 {
				t.Errorf("uv(min corner) = (%v, %v), want (0, 0)", u, v)
			}
			if u, v := uv(m.halfX, m.halfY, m.halfX, m.halfY); u != 1 || v != 1 {
				t.Errorf("uv(max corner) = (%v, %v), want (1, 1)", u, v)
			}
			// Out-of-bounds positions are preserved, not clamped.
			if u, _ := uv(-m.halfX-1000, 0, m.halfX, m.halfY); u >= 0 {
				t.Errorf("uv past the west edge = %v, want negative", u)
			}
		})
	}

	// The verified player: west of centre, well north. Loose bounds rather
	// than hand-computed decimals; the exact formula is pinned by the
	// identities above.
	u, v := uv(-67904.590, -237666.790, 403446.75, 403857.03)
	if u < 0.41 || u > 0.42 {
		t.Errorf("kittykat95 u = %v, want ~0.416", u)
	}
	if v < 0.20 || v > 0.21 {
		t.Errorf("kittykat95 v = %v, want ~0.206", v)
	}
}

func TestFingerprintKeys(t *testing.T) {
	// Detect returns these keys and the server resolves them through
	// config.MapPresetByName; the two tables drifting apart would make
	// detection succeed and resolution fail.
	want := map[string]bool{"gondwa": true, "panjura": true, "riparia": true}
	got := fingerprints()
	if len(got) != len(want) {
		t.Fatalf("fingerprints has %d maps, want %d", len(got), len(want))
	}
	for name, names := range got {
		if !want[name] {
			t.Errorf("unexpected fingerprint key %q", name)
		}
		if len(names) < detectMinNames {
			t.Errorf("map %q has only %d reference names", name, len(names))
		}
	}
}

func TestDetect(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantMap string
		wantOK  bool
	}{
		{
			"gondwa display names with echo",
			"(ListPOI): Impact Crater, Grand Plains, Titan's Pass, Savanna Grassland, Salt Flats, Burned Forest, Red Island, Stego Mountain",
			"gondwa", true,
		},
		{
			"riparia internal names",
			"DryFangCanyon, CliffEdgeFalls, WollemiForest, TwistedForest, VolcanoIslands, WindTunnels",
			"riparia", true,
		},
		{
			"panjura display names",
			"Grassland Crater, Arc Mountain, The Mire, Blackwater Bayou, Tyrants Gorge, Star Ravine",
			"panjura", true,
		},
		{
			// Snake Gully, Triad Falls and Hunter(')s Thicket exist on both
			// Gondwa and Panjura; a response of only shared names must not
			// pick a side.
			"names shared between maps",
			"Snake Gully, Triad Falls, Hunters Thicket",
			"", false,
		},
		{"garbage", "That command does not exist", "", false},
		{"empty", "", "", false},
		{"too few names", "Impact Crater, Grand Plains", "", false},
		{
			"unknown map",
			"Somewhere New, Another Place, Third Location, Fourth Spot",
			"", false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Detect(tc.body)
			if ok != tc.wantOK || got != tc.wantMap {
				t.Errorf("Detect = %q, %v, want %q, %v", got, ok, tc.wantMap, tc.wantOK)
			}
		})
	}
}

func TestNormalizePOI(t *testing.T) {
	// Display and internal spellings of the same place must collide.
	pairs := [][2]string{
		{"Dry Fang Canyon", "DryFangCanyon"},
		{"Titan's Pass", "TitansPass"},
		{"Hunter's Thicket", "Hunters Thicket"},
	}
	for _, pair := range pairs {
		if a, b := normalizePOI(pair[0]), normalizePOI(pair[1]); a != b || a == "" {
			t.Errorf("normalizePOI(%q) = %q, normalizePOI(%q) = %q; want equal and non-empty",
				pair[0], a, pair[1], b)
		}
	}
	if got := normalizePOI(" The Mudflats "); got != "themudflats" {
		t.Errorf("normalizePOI = %q", got)
	}
	if !strings.Contains(normalizePOI("Area 51"), "51") {
		t.Error("digits must survive normalization")
	}
}
