package config

import (
	"errors"
	"fmt"
	"math"
	"net"
	"strconv"
	"strings"
	"time"
)

// LogLevel selects the verbosity of the structured logger.
type LogLevel string

const (
	LogLevelDebug LogLevel = "debug"
	LogLevelInfo  LogLevel = "info"
	LogLevelWarn  LogLevel = "warn"
	LogLevelError LogLevel = "error"
)

// Config is the root configuration.
type Config struct {
	LogLevel LogLevel `name:"logLevel" default:"info" description:"log verbosity: debug, info, warn, or error"`
	HTTP     HTTP     `name:"http" description:""`
	RCON     RCON     `name:"rcon" description:"Source RCON server settings"`
	Map      Map      `name:"map" description:"map identity, image, and world-coordinate calibration"`
	Poller   Poller   `name:"poller" description:"background player-position polling"`
}

// MaxTimeoutSeconds bounds rcon.timeoutSeconds. The page is meant to report a
// dead server quickly, and the shutdown grace is derived from this, so an
// unbounded value would make a rolling restart arbitrarily slow.
const MaxTimeoutSeconds = 60

// MaxConcurrentLimit bounds rcon.maxConcurrent. Anything near this is already
// far past what a Source server tolerates; the ceiling exists to catch a typo,
// not to describe a workable setting.
const MaxConcurrentLimit = 64

const maxPort = 65535

// HTTP configures the HTTP listener
type HTTP struct {
	Bind string `name:"bind" default:"" description:"address to listen on; empty listens on all interfaces over both IPv4 and IPv6"`
	// Ports and timeouts are plain ints rather than sized types: configulator
	// assigns YAML numbers through reflection without a range check, so a
	// narrower field would silently wrap 70000 to 4464 and -1 to 65535 where an
	// int lets Validate reject both.
	Port int `name:"port" default:"8080" description:""`
}

// Addr returns the listen address in host:port form.
func (h HTTP) Addr() string {
	return net.JoinHostPort(h.Bind, strconv.Itoa(h.Port))
}

// RCON configures the upstream Source RCON server.
type RCON struct {
	Host string `name:"host" default:"127.0.0.1" description:"hostname or IP of the Source RCON server"`
	// 7779 rather than the generic Source default 27015: this tool only talks
	// to Path of Titans servers, and that is the port their RCON listens on.
	Port     int    `name:"port" default:"7779" description:"TCP port of the Source RCON server"`
	Password string `name:"password" description:"RCON password (required)"`
	// Expressed in seconds rather than as a time.Duration because configulator
	// parses integer fields with strconv, so a "5s" default would not load.
	TimeoutSeconds int `name:"timeoutSeconds" default:"5" description:"deadline in seconds covering a whole RCON exchange: connect, authenticate, command, response"`
	// Source servers handle RCON on the main thread and ban clients that pile on
	// connections, so the useful value here is small. The poller runs one
	// command at a time and waits out a busy slot; this is headroom, not a
	// throughput knob.
	MaxConcurrent int `name:"maxConcurrent" default:"4" description:"maximum RCON commands in flight at once"`
}

// Addr returns the RCON server address in host:port form.
func (r RCON) Addr() string {
	return net.JoinHostPort(r.Host, strconv.Itoa(r.Port))
}

// Timeout returns the per-command deadline.
func (r RCON) Timeout() time.Duration {
	return time.Duration(r.TimeoutSeconds) * time.Second
}

// MapNameAuto asks the poller to identify the map over RCON instead of naming
// it here. No RCON command reports the current map, so detection fingerprints
// ListPOI output against the official maps' point-of-interest names.
const MapNameAuto = "auto"

// Map identifies the world the players move in and where its image lives.
type Map struct {
	Name string `name:"name" default:"auto" description:"map the server runs: auto (detect over RCON), gondwa (aka island), panjura, riparia, or a custom name with both half extents set"`
	// The image is loaded at runtime rather than embedded because it is
	// extracted from Alderon's game files: their artwork must not be baked
	// into a public repository or container image.
	ImagePath string `name:"imagePath" description:"map background image: a PNG file, or a directory holding <map>.png per map (required)"`
	// Half extents are the world-space distance from the origin to each edge
	// in Unreal units. Per-axis because Gondwa's X and Y differ by 0.1%, and
	// configurable because the official maps differ by 2x. Zero means "use the
	// named map's calibrated value"; setting both overrides the preset, which
	// is what makes a custom map renderable.
	HalfExtentX float64 `name:"halfExtentX" default:"0" description:"world half extent on the X axis in Unreal units; 0 uses the named map's calibrated value"`
	HalfExtentY float64 `name:"halfExtentY" default:"0" description:"world half extent on the Y axis in Unreal units; 0 uses the named map's calibrated value"`
}

// MapPreset is one official map's calibrated identity.
type MapPreset struct {
	// Name is the canonical lower-case key; Detect in the players package
	// returns these same names.
	Name        string
	DisplayName string
	// ImageFile is the basename looked up when map.imagePath is a directory.
	ImageFile   string
	HalfExtentX float64
	HalfExtentY float64
}

// MapPresets lists the official maps. The half extents come from
// MapBackground.AreaBounds in the game files, confirmed empirically by
// overlaying 63 named landmarks onto the map image; they are data, not
// tunables. A function rather than a package variable so nothing can mutate
// the table between callers.
func MapPresets() []MapPreset {
	return []MapPreset{
		{Name: "gondwa", DisplayName: "Gondwa", ImageFile: "gondwa.png", HalfExtentX: 403446.75, HalfExtentY: 403857.03},
		{Name: "panjura", DisplayName: "Panjura", ImageFile: "panjura.png", HalfExtentX: 504000, HalfExtentY: 504000},
		{Name: "riparia", DisplayName: "Riparia", ImageFile: "riparia.png", HalfExtentX: 257650, HalfExtentY: 257650},
	}
}

// MapPresetByName resolves name to an official map. Case does not matter, and
// "island" is accepted for Gondwa: it is the map's internal name, and Game.ini
// says ServerMap=Island, so it is what an operator reading their own server
// config would type.
func MapPresetByName(name string) (MapPreset, bool) {
	key := strings.ToLower(name)
	if key == "island" {
		key = "gondwa"
	}
	for _, p := range MapPresets() {
		if p.Name == key {
			return p, true
		}
	}
	return MapPreset{}, false
}

// knownMapNames is the vocabulary quoted when map.name is rejected.
func knownMapNames() string {
	names := make([]string, 0, len(MapPresets()))
	for _, p := range MapPresets() {
		names = append(names, p.Name)
	}
	return MapNameAuto + ", " + strings.Join(names, ", ") + ", or island"
}

// Auto reports whether the map should be detected over RCON.
func (m Map) Auto() bool { return strings.EqualFold(m.Name, MapNameAuto) }

// overridden reports whether both half extents are explicitly set.
func (m Map) overridden() bool { return m.HalfExtentX > 0 && m.HalfExtentY > 0 }

// Extents returns the world half extents for an explicitly named map.
// Overrides win over the preset so a recalibration never has to wait for a
// release. In auto mode the extents are unknown until detection and this
// returns zeros.
func (m Map) Extents() (x, y float64) {
	if m.overridden() {
		return m.HalfExtentX, m.HalfExtentY
	}
	if p, ok := MapPresetByName(m.Name); ok {
		return p.HalfExtentX, p.HalfExtentY
	}
	return 0, 0
}

// DisplayName returns the name shown on the page: the preset's spelling for
// official maps, the configured name verbatim for custom ones.
func (m Map) DisplayName() string {
	if p, ok := MapPresetByName(m.Name); ok {
		return p.DisplayName
	}
	return m.Name
}

// ImageFile returns the basename loaded when imagePath is a directory.
func (m Map) ImageFile() string {
	if p, ok := MapPresetByName(m.Name); ok {
		return p.ImageFile
	}
	return strings.ToLower(m.Name) + ".png"
}

// MaxPollIntervalSeconds bounds poller.intervalSeconds. A map refreshing
// slower than an hour is indistinguishable from a broken one; the ceiling
// catches a typo, not a workable setting.
const MaxPollIntervalSeconds = 3600

// MaxIdleAfterSeconds bounds poller.idleAfterSeconds the same way.
const MaxIdleAfterSeconds = 86400

// Poller controls how often the map asks the game for player positions. RCON
// commands execute on the game thread, so this cadence is a direct tax on the
// server's tick budget; polling stops entirely while nobody is watching.
type Poller struct {
	// Expressed in seconds rather than as a time.Duration because configulator
	// parses integer fields with strconv, so a "10s" default would not load.
	IntervalSeconds int `name:"intervalSeconds" default:"10" description:"seconds between player polls while the map has viewers"`
	// Compared against the time of the last browser request. Hidden tabs stop
	// requesting, so this is what turns "nobody is watching" into zero RCON
	// traffic.
	IdleAfterSeconds int `name:"idleAfterSeconds" default:"30" description:"seconds without a browser request after which polling stops"`
}

// Interval returns the gap between polls while the map has viewers.
func (p Poller) Interval() time.Duration {
	return time.Duration(p.IntervalSeconds) * time.Second
}

// IdleAfter returns how long after the last browser request polling stops.
func (p Poller) IdleAfter() time.Duration {
	return time.Duration(p.IdleAfterSeconds) * time.Second
}

// Validate reports every problem with the configuration at once, so a bad
// deployment does not have to be fixed one restart at a time.
func (c Config) Validate() error {
	var errs []error

	switch c.LogLevel {
	case LogLevelDebug, LogLevelInfo, LogLevelWarn, LogLevelError:
	default:
		errs = append(errs, fmt.Errorf("logLevel %q must be one of debug, info, warn, error", c.LogLevel))
	}
	if c.HTTP.Port < 1 || c.HTTP.Port > maxPort {
		errs = append(errs, fmt.Errorf("http.port %d must be between 1 and %d", c.HTTP.Port, maxPort))
	}
	if c.RCON.Host == "" {
		errs = append(errs, errors.New("rcon.host must not be empty"))
	}
	if c.RCON.Port < 1 || c.RCON.Port > maxPort {
		errs = append(errs, fmt.Errorf("rcon.port %d must be between 1 and %d", c.RCON.Port, maxPort))
	}
	if c.RCON.Password == "" {
		errs = append(errs, errors.New("rcon.password must not be empty"))
	}
	if c.RCON.TimeoutSeconds < 1 || c.RCON.TimeoutSeconds > MaxTimeoutSeconds {
		errs = append(errs, fmt.Errorf("rcon.timeoutSeconds %d must be between 1 and %d",
			c.RCON.TimeoutSeconds, MaxTimeoutSeconds))
	}
	if c.RCON.MaxConcurrent < 1 || c.RCON.MaxConcurrent > MaxConcurrentLimit {
		errs = append(errs, fmt.Errorf("rcon.maxConcurrent %d must be between 1 and %d",
			c.RCON.MaxConcurrent, MaxConcurrentLimit))
	}
	if c.Map.Name == "" {
		errs = append(errs, errors.New("map.name must not be empty"))
	}
	if c.Map.ImagePath == "" {
		errs = append(errs, errors.New("map.imagePath must not be empty"))
	}
	switch {
	case c.Map.HalfExtentX == 0 && c.Map.HalfExtentY == 0:
		// No overrides: the name alone must identify a calibrated map, either
		// directly or by asking for detection.
		if c.Map.Name != "" && !c.Map.Auto() {
			if _, ok := MapPresetByName(c.Map.Name); !ok {
				errs = append(errs, fmt.Errorf("map.name %q is not a known map (%s); a custom map needs both half extents set",
					c.Map.Name, knownMapNames()))
			}
		}
	case math.IsNaN(c.Map.HalfExtentX) || c.Map.HalfExtentX <= 0 ||
		math.IsNaN(c.Map.HalfExtentY) || c.Map.HalfExtentY <= 0:
		// NaN is checked explicitly: it compares false against everything, so
		// a plain <= 0 would wave it through. A one-sided override lands here
		// too: it would silently distort the aspect ratio.
		errs = append(errs, fmt.Errorf("map.halfExtentX %v and map.halfExtentY %v must both be positive when either is set",
			c.Map.HalfExtentX, c.Map.HalfExtentY))
	case c.Map.Auto():
		// Overrides mean the operator already knows the map, and detection
		// could contradict them; make them name it instead.
		errs = append(errs, errors.New("map.halfExtentX/map.halfExtentY cannot be combined with map.name auto; name the map they belong to"))
	}
	if c.Poller.IntervalSeconds < 1 || c.Poller.IntervalSeconds > MaxPollIntervalSeconds {
		errs = append(errs, fmt.Errorf("poller.intervalSeconds %d must be between 1 and %d",
			c.Poller.IntervalSeconds, MaxPollIntervalSeconds))
	}
	// The idle window must outlast an interval: demand is stamped by browser
	// requests that arrive at most one interval apart, so a shorter window
	// would judge an active viewer idle between their own polls.
	if c.Poller.IdleAfterSeconds < 1 || c.Poller.IdleAfterSeconds < c.Poller.IntervalSeconds ||
		c.Poller.IdleAfterSeconds > MaxIdleAfterSeconds {
		errs = append(errs, fmt.Errorf("poller.idleAfterSeconds %d must be between poller.intervalSeconds (%d) and %d",
			c.Poller.IdleAfterSeconds, c.Poller.IntervalSeconds, MaxIdleAfterSeconds))
	}

	if len(errs) > 0 {
		return fmt.Errorf("invalid configuration: %w", errors.Join(errs...))
	}
	return nil
}
