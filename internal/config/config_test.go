package config

import (
	"math"
	"strings"
	"testing"
	"time"
)

func valid() Config {
	return Config{
		LogLevel: LogLevelInfo,
		HTTP:     HTTP{Bind: "", Port: 8080},
		RCON:     RCON{Host: "127.0.0.1", Port: 7779, Password: "secret", TimeoutSeconds: 5, MaxConcurrent: 4},
		Map:      Map{Name: "gondwa", ImagePath: "gondwa.png"},
		Poller:   Poller{IntervalSeconds: 10, IdleAfterSeconds: 30},
	}
}

func TestValidateAcceptsValid(t *testing.T) {
	if err := valid().Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

// TestValidateRangeChecks covers the values a YAML file can carry that a narrower
// field type would silently wrap: 70000 truncating to 4464 and -1 to 65535 were
// both real, and neither is visible to a != 0 check.
func TestValidateRangeChecks(t *testing.T) {
	tests := []struct {
		name  string
		mutef func(*Config)
		want  string
	}{
		{"port too high", func(c *Config) { c.HTTP.Port = 70000 }, "http.port"},
		{"port negative", func(c *Config) { c.HTTP.Port = -1 }, "http.port"},
		{"port zero", func(c *Config) { c.HTTP.Port = 0 }, "http.port"},
		{"rcon port too high", func(c *Config) { c.RCON.Port = 65536 }, "rcon.port"},
		{"rcon port negative", func(c *Config) { c.RCON.Port = -8080 }, "rcon.port"},
		{"timeout zero", func(c *Config) { c.RCON.TimeoutSeconds = 0 }, "rcon.timeoutSeconds"},
		{"timeout negative", func(c *Config) { c.RCON.TimeoutSeconds = -1 }, "rcon.timeoutSeconds"},
		{"timeout too high", func(c *Config) { c.RCON.TimeoutSeconds = 65537 }, "rcon.timeoutSeconds"},
		{"no password", func(c *Config) { c.RCON.Password = "" }, "rcon.password"},
		{"no host", func(c *Config) { c.RCON.Host = "" }, "rcon.host"},
		{"bad log level", func(c *Config) { c.LogLevel = "silly" }, "logLevel"},
		{"no map name", func(c *Config) { c.Map.Name = "" }, "map.name"},
		{"no image path", func(c *Config) { c.Map.ImagePath = "" }, "map.imagePath"},
		{"unknown map without extents", func(c *Config) { c.Map.Name = "spiro" }, "map.name"},
		{"auto with extents", func(c *Config) {
			c.Map.Name = "auto"
			c.Map.HalfExtentX, c.Map.HalfExtentY = 100, 100
		}, "map.halfExtent"},
		{"one-sided extent", func(c *Config) { c.Map.HalfExtentX = 100 }, "map.halfExtentY"},
		{"negative extent", func(c *Config) { c.Map.HalfExtentX, c.Map.HalfExtentY = -1, 100 }, "map.halfExtentX"},
		{"NaN extent", func(c *Config) { c.Map.HalfExtentX, c.Map.HalfExtentY = math.NaN(), 100 }, "map.halfExtentX"},
		{"interval zero", func(c *Config) { c.Poller.IntervalSeconds = 0 }, "poller.intervalSeconds"},
		{"interval too high", func(c *Config) { c.Poller.IntervalSeconds = MaxPollIntervalSeconds + 1 }, "poller.intervalSeconds"},
		{"idle below interval", func(c *Config) { c.Poller.IdleAfterSeconds = 5 }, "poller.idleAfterSeconds"},
		{"idle too high", func(c *Config) { c.Poller.IdleAfterSeconds = MaxIdleAfterSeconds + 1 }, "poller.idleAfterSeconds"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid()
			tc.mutef(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// TestValidateReportsEveryProblem is the documented behaviour: a bad deployment
// should not have to be fixed one restart at a time.
func TestValidateReportsEveryProblem(t *testing.T) {
	cfg := Config{} // every field wrong at once
	err := cfg.Validate()
	if err == nil {
		t.Fatal("empty config was accepted")
	}
	for _, want := range []string{
		"logLevel", "http.port", "rcon.host", "rcon.port", "rcon.password", "rcon.timeoutSeconds",
		"map.name", "map.imagePath", "poller.intervalSeconds", "poller.idleAfterSeconds",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
}

func TestAddrAndTimeout(t *testing.T) {
	cfg := valid()

	if got := cfg.RCON.Addr(); got != "127.0.0.1:7779" {
		t.Errorf("RCON.Addr() = %q", got)
	}
	if got := cfg.RCON.Timeout(); got != 5*time.Second {
		t.Errorf("RCON.Timeout() = %s", got)
	}
	// An empty bind is the dual-stack default and must still form a valid address.
	if got := cfg.HTTP.Addr(); got != ":8080" {
		t.Errorf("HTTP.Addr() with empty bind = %q", got)
	}

	cfg.HTTP.Bind = "::1"
	if got := cfg.HTTP.Addr(); got != "[::1]:8080" {
		t.Errorf("HTTP.Addr() with IPv6 bind = %q, want brackets", got)
	}
}

// TestMapPresets pins the calibrated per-map constants: they come from
// MapBackground.AreaBounds in the game files and were confirmed by landmark
// overlay, so any drift here is a regression, not a tune.
func TestMapPresets(t *testing.T) {
	tests := []struct {
		name        string
		wantDisplay string
		wantX       float64
		wantY       float64
		wantImage   string
	}{
		{"gondwa", "Gondwa", 403446.75, 403857.03, "gondwa.png"},
		{"Gondwa", "Gondwa", 403446.75, 403857.03, "gondwa.png"},
		// Game.ini says ServerMap=Island; that spelling must work.
		{"island", "Gondwa", 403446.75, 403857.03, "gondwa.png"},
		{"ISLAND", "Gondwa", 403446.75, 403857.03, "gondwa.png"},
		{"panjura", "Panjura", 504000, 504000, "panjura.png"},
		{"riparia", "Riparia", 257650, 257650, "riparia.png"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := Map{Name: tc.name, ImagePath: "maps"}
			x, y := m.Extents()
			if x != tc.wantX || y != tc.wantY {
				t.Errorf("Extents() = %v, %v, want %v, %v", x, y, tc.wantX, tc.wantY)
			}
			if got := m.DisplayName(); got != tc.wantDisplay {
				t.Errorf("DisplayName() = %q, want %q", got, tc.wantDisplay)
			}
			if got := m.ImageFile(); got != tc.wantImage {
				t.Errorf("ImageFile() = %q, want %q", got, tc.wantImage)
			}
			cfg := valid()
			cfg.Map = Map{Name: tc.name, ImagePath: "maps"}
			if err := cfg.Validate(); err != nil {
				t.Errorf("preset name rejected: %v", err)
			}
		})
	}

	// Explicit overrides beat the preset, so a recalibration is a config
	// change rather than a release.
	m := Map{Name: "gondwa", ImagePath: "maps", HalfExtentX: 100, HalfExtentY: 200}
	if x, y := m.Extents(); x != 100 || y != 200 {
		t.Errorf("overridden Extents() = %v, %v, want 100, 200", x, y)
	}

	// A custom map is the override escape hatch: any name plus both extents.
	cfg := valid()
	cfg.Map = Map{Name: "Spiro", ImagePath: "maps", HalfExtentX: 100, HalfExtentY: 100}
	if err := cfg.Validate(); err != nil {
		t.Errorf("custom map with both extents rejected: %v", err)
	}
	if got := cfg.Map.DisplayName(); got != "Spiro" {
		t.Errorf("custom DisplayName() = %q, want the configured name verbatim", got)
	}
	if got := cfg.Map.ImageFile(); got != "spiro.png" {
		t.Errorf("custom ImageFile() = %q, want %q", got, "spiro.png")
	}

	// The default is detection, and it must validate out of the box.
	cfg = valid()
	cfg.Map = Map{Name: "auto", ImagePath: "maps"}
	if err := cfg.Validate(); err != nil {
		t.Errorf("auto rejected: %v", err)
	}
	if !cfg.Map.Auto() {
		t.Error("Auto() = false for name auto")
	}
	if valid().Map.Auto() {
		t.Error("Auto() = true for an explicit map name")
	}
}

func TestPollerDurations(t *testing.T) {
	cfg := valid()
	if got := cfg.Poller.Interval(); got != 10*time.Second {
		t.Errorf("Poller.Interval() = %s", got)
	}
	if got := cfg.Poller.IdleAfter(); got != 30*time.Second {
		t.Errorf("Poller.IdleAfter() = %s", got)
	}
}

// TestMaxTimeoutIsSane guards the coupling to the server's shutdown grace: the
// cap exists so a rolling restart cannot be held open arbitrarily long.
func TestMaxTimeoutIsSane(t *testing.T) {
	if MaxTimeoutSeconds < 1 || MaxTimeoutSeconds > 300 {
		t.Fatalf("MaxTimeoutSeconds = %d, outside a defensible range", MaxTimeoutSeconds)
	}
	cfg := valid()
	cfg.RCON.TimeoutSeconds = MaxTimeoutSeconds
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the cap itself was rejected: %v", err)
	}
}
