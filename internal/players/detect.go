package players

import (
	"strings"
)

// Map detection. No RCON command reports which map the server runs — the
// closest, ServerInfo, gives name/UUID/time/weather only — but ListPOI
// returns the map's point-of-interest names, and those are map-specific.
// Matching the returned set against per-map reference lists identifies the
// map with a single cheap command.

// detectMinNames is the smallest response worth scoring: one or two names is
// a coincidence, not a fingerprint.
const detectMinNames = 3

// detectMinScore is the fraction of the response that must match the winning
// map. Reference lists come from community wikis and can lag the game, so
// this is a majority test rather than an exact one.
const detectMinScore = 0.5

// detectMinLead is how far ahead of the runner-up the winner must be. A few
// POI names are shared between maps (Snake Gully and Triad Falls exist on
// both Gondwa and Panjura); the lead requirement keeps a shared-name response
// from picking a map by accident. A wrong map would render every player in
// the wrong place, so refusing to choose is the honest failure mode.
const detectMinLead = 0.2

// fingerprints returns each official map's known point-of-interest names.
// Keys match config.MapPresets names; TestFingerprintKeys pins that.
//
// The lists are best-effort reference data, not verified against live
// servers (except Gondwa, whose ListPOI output the calibration work used):
//   - gondwa:  https://pathoftitans.wiki/maps/gondwa
//   - panjura: https://pathoftitans.wiki/maps/panjura
//   - riparia: https://nexlinkcore.com/guides/path-of-titans/maps-info/path-of-titans-riparia
//     (internal zone names), plus display names from
//     https://pathoftitans.com/blog/riparia-and-tylosaurus-released
//
// Spelling differences between sources (display "Dry Fang Canyon" versus
// internal "DryFangCanyon") wash out in normalizePOI, and overlap scoring
// tolerates missing or stale entries. A function rather than a package
// variable so nothing can mutate the table between callers.
func fingerprints() map[string][]string {
	return map[string][]string{
		"gondwa": {
			"Azure Shore", "Barrens", "Big Quill Lake", "Birchwoods",
			"Bleached Corals", "Broken Tooth Canyon", "Burned Forest",
			"Castaway Isle", "Dark Woods", "Deepsea Crags", "Deepsea Spires",
			"Desolate Pass", "Dried Lake", "Flyers Bluff", "Golden Kelp",
			"Golden Plateau", "Grand Plains", "Green Hills", "Green Valley",
			"Hoodoo Expanse", "Hot Springs", "Hunters Thicket", "Impact Crater",
			"Kelp Forest", "Lonely Isle", "The Mudflats", "Ocean Pillars",
			"Ocean Stacks", "Pebble Isle", "Rainbow Hills", "Red Island",
			"Red Kelp Forest", "Red Reef", "Ripple Beach", "Rockfall Hill",
			"Sanctuary Isle", "Sand Caverns", "Salt Flats", "Savanna Grassland",
			"Seagrass Bay", "Sharptooth Marsh", "Snake Gully", "Stego Mountain",
			"Sunken Hoodoos", "Sweetwater Shallows", "The Teeth", "Titan's Pass",
			"Triad Falls", "Volcano Bay", "Wilderness Peak", "Whistling Columns",
			"White Cliffs", "Young Grove",
		},
		"panjura": {
			"Grassland Crater", "Grassland Lake", "Swamp Reservoir",
			"Redwood Basin", "Green Plateau", "Rock Maze", "Hunter's Thicket",
			"Star Ravine", "Arc Mountain", "Rockden", "Cliffside Retreat",
			"Dropoff Pond", "Snake Gully", "The Mire", "The Dome", "Forest Rise",
			"Hoodoo Hills", "Deep Lake", "Swamp Home Cave", "Grass Gully",
			"South East Waystone", "Harf Heart Canyon", "Mossy Cavern",
			"The Bend", "Triad Falls", "Crater Pond", "Traveler's Basin",
			"Swampy Pit", "Redwood Wind", "Sinkhole", "Grassland",
			"Broadleaf Forest", "World Edge Falls", "Two Falls Hollow",
			"Blackwater Bayou", "Littlebone Canyon", "Talons Point",
			"Corpse Cove", "Tyrants Gorge",
		},
		"riparia": {
			"FloodedCave", "DryFangCanyon", "EastPassage", "CliffEdgeFalls",
			"BigTreeOverlook", "CragBluffs", "BlackFernHills", "CedrusForest",
			"AbyssalDepths", "KelpVale", "Redwoods", "VolcanoIslands",
			"TwistedForest", "Mudflats", "HollowHills", "WollemiForest",
			"StillwaterBog", "WindTunnels", "StonebedShoal", "CoastlandSwamp",
			"PalmIslands", "TallBrushCoasts", "CoastalBluffs", "Steeprun",
			"Three Horn Peak", "Redwood Lake", "Black Fern Forest",
		},
	}
}

// normalizePOI collapses a name to lower-case alphanumerics so display
// spellings ("Dry Fang Canyon", "Titan's Pass") and internal ones
// ("DryFangCanyon") fingerprint identically — sources disagree on which form
// ListPOI reports, and nothing here should depend on the answer.
func normalizePOI(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Detect identifies the map from a ListPOI response body. ok is false when
// the response does not clearly match exactly one known map; the caller
// should retry later rather than guess.
func Detect(listPOI string) (string, bool) {
	body := strings.TrimSpace(listPOI)
	// Responses echo the command, "(ListPOI): name, ...". The echo is
	// dropped so its text cannot dilute the score.
	if strings.HasPrefix(body, "(") {
		if _, after, ok := strings.Cut(body, "):"); ok {
			body = after
		}
	}

	seen := make(map[string]struct{})
	for name := range strings.SplitSeq(body, ",") {
		if n := normalizePOI(name); n != "" {
			seen[n] = struct{}{}
		}
	}
	if len(seen) < detectMinNames {
		return "", false
	}

	best, second := "", 0.0
	bestScore := 0.0
	for mapName, names := range fingerprints() {
		refs := make(map[string]struct{}, len(names))
		for _, n := range names {
			refs[normalizePOI(n)] = struct{}{}
		}
		matches := 0
		for n := range seen {
			if _, ok := refs[n]; ok {
				matches++
			}
		}
		score := float64(matches) / float64(len(seen))
		switch {
		case score > bestScore:
			best, second = mapName, bestScore
			bestScore = score
		case score > second:
			second = score
		}
	}

	if bestScore < detectMinScore || bestScore-second < detectMinLead {
		return "", false
	}
	return best, true
}
