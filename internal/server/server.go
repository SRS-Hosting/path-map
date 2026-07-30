// Package server serves the live player map and the JSON snapshot behind it.
package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/USA-RedDragon/path-map/internal/config"
	"github.com/USA-RedDragon/path-map/internal/players"
	"github.com/USA-RedDragon/rcon"
)

//go:embed assets/index.html
var assets embed.FS

// shutdownGrace is how long in-flight work gets to finish after a signal.
// It is derived from the RCON timeout rather than fixed, so a poll already in
// flight can always run to completion or time out on its own terms: a
// hardcoded grace shorter than the configured timeout would cut it off and
// exit non-zero on every rolling restart.
func shutdownGrace(rconTimeout time.Duration) time.Duration {
	return rconTimeout + 5*time.Second
}

// Server owns the HTTP listeners, the poller, and the map imagery.
type Server struct {
	http   *http.Server
	rcon   *rcon.Client
	poller *players.Poller
	images *imageStore
	page   []byte
	bind   string
	port   int
}

// New builds a Server for cfg. It fails when the embedded page cannot be
// rendered or the configured map image cannot be read: both mean a broken
// build or deployment, not a runtime condition — with one deliberate
// exception. In auto-detect mode with an image directory, individual map
// images load lazily once the map is known, because which one is needed is
// not knowable at startup.
func New(cfg *config.Config) (*Server, error) {
	client := rcon.New(cfg.RCON.Addr(), cfg.RCON.Password,
		rcon.WithTimeout(cfg.RCON.Timeout()),
		rcon.WithMaxConcurrent(cfg.RCON.MaxConcurrent),
	)

	images, err := newImageStore(cfg.Map)
	if err != nil {
		return nil, err
	}

	var fixed *players.MapInfo
	if !cfg.Map.Auto() {
		info := fixedMapInfo(cfg.Map)
		fixed = &info
	}
	poller := players.NewPoller(client, cfg.Poller.Interval(), cfg.Poller.IdleAfter(), fixed, lookupPreset,
		players.WithHealth(cfg.Poller.HealthBudget()))
	page, err := renderPage(cfg.Poller.IntervalSeconds, cfg.RCON.TimeoutSeconds)
	if err != nil {
		return nil, err
	}

	s := &Server{
		rcon:   client,
		poller: poller,
		images: images,
		page:   page,
		bind:   cfg.HTTP.Bind,
		port:   cfg.HTTP.Port,
	}

	mux := http.NewServeMux()
	// "{$}" matches the root path exactly, so anything else falls through to
	// the mux's own 404 instead of being served the page.
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /api/players", s.handlePlayers)
	mux.HandleFunc("GET /map.png", s.handleMapImage)

	s.http = &http.Server{
		Addr:              cfg.HTTP.Addr(),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		// No handler blocks on RCON — the poller runs in the background and
		// requests serve from cache — but deriving the write deadline from the
		// RCON timeout keeps the two coupled in one place should that change.
		WriteTimeout: cfg.RCON.Timeout() + 10*time.Second,
		IdleTimeout:  60 * time.Second,
		ErrorLog:     slog.NewLogLogger(slog.Default().Handler(), slog.LevelWarn),
	}

	return s, nil
}

// fixedMapInfo converts an explicitly named map into the poller's resolved
// form, with overrides already applied by Extents.
func fixedMapInfo(m config.Map) players.MapInfo {
	name := strings.ToLower(m.Name)
	if p, ok := config.MapPresetByName(m.Name); ok {
		// "island" and case variants collapse to the canonical name.
		name = p.Name
	}
	x, y := m.Extents()
	return players.MapInfo{
		Name:        name,
		DisplayName: m.DisplayName(),
		HalfExtentX: x,
		HalfExtentY: y,
		ImageFile:   m.ImageFile(),
	}
}

// lookupPreset resolves a detected map name to its calibration. Detection
// only ever names official maps, so the preset table is the whole answer.
func lookupPreset(name string) (players.MapInfo, bool) {
	p, ok := config.MapPresetByName(name)
	if !ok {
		return players.MapInfo{}, false
	}
	return players.MapInfo{
		Name:        p.Name,
		DisplayName: p.DisplayName,
		HalfExtentX: p.HalfExtentX,
		HalfExtentY: p.HalfExtentY,
		ImageFile:   p.ImageFile,
	}, true
}

// renderPage renders the map page once at startup. The page only depends on
// configuration, which does not change while the process runs; everything
// runtime-dependent (map name, players) arrives through the API instead.
// Deliberately absent: the RCON address. The page is viewer-facing, and where
// the game server lives is the operator's business.
func renderPage(intervalSeconds, timeoutSeconds int) ([]byte, error) {
	tmpl, err := template.ParseFS(assets, "assets/index.html")
	if err != nil {
		return nil, fmt.Errorf("parse embedded page: %w", err)
	}

	var buf bytes.Buffer
	data := struct {
		IntervalSeconds int
		TimeoutSeconds  int
	}{
		IntervalSeconds: intervalSeconds,
		TimeoutSeconds:  timeoutSeconds,
	}
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("render embedded page: %w", err)
	}
	return buf.Bytes(), nil
}

// mapImage is one loaded background image.
type mapImage struct {
	bytes   []byte
	modTime time.Time
	etag    string
}

// imageStore hands out map background images. In single-file mode one image
// serves whatever map is current; in directory mode images are keyed by the
// per-map basename and loaded on first use.
type imageStore struct {
	// dir is empty in single-file mode.
	dir    string
	single *mapImage

	mu     sync.Mutex
	byFile map[string]*mapImage
}

// newImageStore stats the configured path once and decides the mode. Failure
// behaviour is deliberately asymmetric: anything knowable at startup fails
// fast (missing path, unreadable single file, an explicit map's image missing
// from the directory, an auto directory with no known images at all), while
// an individual image in auto mode is left for first use.
func newImageStore(m config.Map) (*imageStore, error) {
	info, err := os.Stat(m.ImagePath)
	if err != nil {
		return nil, fmt.Errorf("map image %s: %w", m.ImagePath, err)
	}

	if !info.IsDir() {
		img, err := loadImage(m.ImagePath)
		if err != nil {
			return nil, err
		}
		return &imageStore{single: img}, nil
	}

	st := &imageStore{dir: m.ImagePath, byFile: map[string]*mapImage{}}
	if !m.Auto() {
		if _, err := st.get(m.ImageFile()); err != nil {
			return nil, err
		}
		return st, nil
	}
	for _, p := range config.MapPresets() {
		if _, err := os.Stat(filepath.Join(st.dir, p.ImageFile)); err == nil {
			return st, nil
		}
	}
	return nil, fmt.Errorf("map image directory %s holds none of the known map images", m.ImagePath)
}

// get returns the image for file, loading and caching it on first use.
// Single-file mode ignores file: the operator promised one image for
// whatever map is current.
func (st *imageStore) get(file string) (*mapImage, error) {
	if st.dir == "" {
		return st.single, nil
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	if img, ok := st.byFile[file]; ok {
		return img, nil
	}
	img, err := loadImage(filepath.Join(st.dir, file))
	if err != nil {
		return nil, err
	}
	st.byFile[file] = img
	return img, nil
}

func loadImage(path string) (*mapImage, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("map image %s: %w", path, err)
	}
	//nolint:gosec // the path comes from the operator's own configuration
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("map image %s: %w", path, err)
	}
	sum := sha256.Sum256(data)
	return &mapImage{
		bytes:   data,
		modTime: info.ModTime(),
		etag:    `"` + hex.EncodeToString(sum[:8]) + `"`,
	}, nil
}

// listen opens the listeners for the configured bind address.
//
// A wildcard bind takes one listener per family rather than relying on a single
// dual-stack socket: Go sets IPV6_V6ONLY on a "tcp6" listener, so the two can
// share a port, and IPv4 reachability stops depending on the host's
// net.ipv6.bindv6only setting.
//
// Both families are attempted, but only one has to succeed. A host with a stack
// disabled still gets a working service on the other, with a warning naming what
// was lost; the "listening" lines that follow say exactly which families are up.
// Note that this treats a port already taken on one family the same as a missing
// stack, so it can degrade to half-reachable instead of reporting the conflict.
func (s *Server) listen(ctx context.Context) ([]net.Listener, error) {
	var lc net.ListenConfig
	port := strconv.Itoa(s.port)

	if s.bind != "" && s.bind != "*" {
		// An explicit address already picks its own family, and there is no
		// second one to fall back to.
		addr := net.JoinHostPort(s.bind, port)
		ln, err := lc.Listen(ctx, "tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("listen on %s: %w", addr, err)
		}
		return []net.Listener{ln}, nil
	}

	families := []struct{ network, host string }{
		{"tcp4", "0.0.0.0"},
		{"tcp6", "::"},
	}
	listeners := make([]net.Listener, 0, len(families))
	errs := make([]error, 0, len(families))

	for _, family := range families {
		ln, err := lc.Listen(ctx, family.network, net.JoinHostPort(family.host, port))
		if err != nil {
			slog.Warn("could not listen on this address family, continuing without it",
				"network", family.network, "port", port, "error", err)
			errs = append(errs, fmt.Errorf("%s: %w", family.network, err))
			continue
		}
		listeners = append(listeners, ln)
	}

	if len(listeners) == 0 {
		return nil, fmt.Errorf("listen on port %s: %w", port, errors.Join(errs...))
	}
	return listeners, nil
}

// Run serves until ctx is cancelled, then drains in-flight requests.
func (s *Server) Run(ctx context.Context) error {
	listeners, err := s.listen(ctx)
	if err != nil {
		return err
	}

	// The poller lives exactly as long as the listeners. Its context is
	// derived so the serve-error path below — where ctx itself was never
	// cancelled — still stops it, and the deferred receive keeps Run from
	// returning while a poll goroutine could still touch the RCON client.
	// LIFO order matters: stopPoller runs before the receive it unblocks.
	pollDone := make(chan struct{})
	defer func() { <-pollDone }()
	pollCtx, stopPoller := context.WithCancel(ctx)
	defer stopPoller()
	go func() {
		s.poller.Run(pollCtx)
		close(pollDone)
	}()

	// Buffered for every listener so each goroutine can exit even when shutdown
	// wins the race below.
	serveErr := make(chan error, len(listeners))
	for _, ln := range listeners {
		slog.Info("listening", "addr", ln.Addr().String(), "network", ln.Addr().Network(), "rcon", s.rcon.Addr())
		go func(ln net.Listener) {
			err := s.http.Serve(ln)
			if errors.Is(err, http.ErrServerClosed) {
				err = nil
			}
			serveErr <- err
		}(ln)
	}

	select {
	case err := <-serveErr:
		// Failing to open a family is tolerated above, but a listener that dies
		// after it started accepting is unexpected, so give up entirely and let
		// the supervisor restart us. Close() rather than leaking the sibling
		// listener, which would otherwise keep accepting into a dead process.
		if cerr := s.http.Close(); cerr != nil {
			slog.Debug("close after serve error", "error", cerr)
		}
		if err != nil {
			return fmt.Errorf("serve: %w", err)
		}
		return nil
	case <-ctx.Done():
	}

	grace := shutdownGrace(s.rcon.Timeout())
	slog.Info("shutting down", "grace", grace)

	// WithoutCancel keeps ctx's values but drops its cancellation: ctx has
	// already fired, and the point of this context is to give open requests time
	// to land rather than to cut them off.
	drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), grace)
	defer cancel()

	if err := s.http.Shutdown(drainCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	// Collect from every listener so none is left mid-flight when we return.
	for range listeners {
		if err := <-serveErr; err != nil {
			return fmt.Errorf("serve: %w", err)
		}
	}

	slog.Info("stopped")
	return nil
}

func (s *Server) handleIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	// The page is self-contained except for its own map image, so this policy
	// is both exact and a runtime guarantee of the offline requirement:
	// anything that later reached for a CDN would be blocked rather than
	// silently working in development. img-src 'self' is the one grant beyond
	// rcon-web's policy, for /map.png.
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; img-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; "+
			"connect-src 'self'; form-action 'none'; frame-ancestors 'none'; base-uri 'none'")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if _, err := w.Write(s.page); err != nil {
		slog.Debug("write page", "error", err)
	}
}

// playersResponse is the snapshot the page polls for. There is no POST
// surface and every endpoint is read-only and side-effect-free, so the
// content-type CSRF guard rcon-web needs has nothing to protect here.
type playersResponse struct {
	// Map is null until the map is known — the page shows "detecting" off
	// this field.
	Map *players.MapInfo `json:"map"`
	// GeneratedAt is null before the first successful poll.
	GeneratedAt *time.Time `json:"generatedAt"`
	// AgeSeconds is how old the snapshot was when this response was built,
	// measured on the server's clock. The page schedules its fetches and
	// ticks its "updated Ns ago" from this, so browser clock skew cannot
	// distort either.
	AgeSeconds *float64         `json:"ageSeconds,omitempty"`
	Total      int              `json:"total"`
	Complete   bool             `json:"complete"`
	Error      string           `json:"error,omitempty"`
	Players    []players.Player `json:"players"`
}

// handlePlayers serves the cached snapshot. Always 200: a stale or pending
// map is page content the browser must keep rendering, and a 5xx here would
// make every proxy and monitor in between report the service itself as down.
// It never touches RCON — the Observe call reads cache and records demand.
func (s *Server) handlePlayers(w http.ResponseWriter, _ *http.Request) {
	snap, info, errMsg := s.poller.Observe()

	resp := playersResponse{
		Map:     info,
		Error:   errMsg,
		Players: []players.Player{},
	}
	if snap != nil {
		resp.GeneratedAt = &snap.GeneratedAt
		age := time.Since(snap.GeneratedAt).Seconds()
		resp.AgeSeconds = &age
		resp.Total = snap.Total
		resp.Complete = snap.Complete
		resp.Players = snap.Players
	}

	// no-store rather than no-cache: the payload changes every poll and is
	// tiny, so revalidation would cost more than it saves.
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, resp)
}

// handleMapImage serves the current map's background. ServeContent supplies
// Content-Type from the name and handles If-None-Match and Range for free.
func (s *Server) handleMapImage(w http.ResponseWriter, r *http.Request) {
	info := s.poller.Resolved()
	if info == nil && s.images.dir != "" {
		// Which image to serve is not yet knowable. The page waits for the
		// API's map field before loading the image, so a viewer only sees
		// this by asking early.
		w.Header().Set("Cache-Control", "no-store")
		s.writeJSON(w, http.StatusServiceUnavailable, playersResponse{Error: "map not yet detected"})
		return
	}

	file := ""
	if info != nil {
		file = info.ImageFile
	}
	img, err := s.images.get(file)
	if err != nil {
		slog.Warn("map image unavailable", "file", file, "error", err)
		w.Header().Set("Cache-Control", "no-store")
		s.writeJSON(w, http.StatusServiceUnavailable, playersResponse{Error: "map image unavailable"})
		return
	}

	// no-cache, not immutable: a redeployed image propagates on the next
	// load, and unchanged ones cost a 304.
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("ETag", img.etag)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, "map.png", img.modTime, bytes.NewReader(img.bytes))
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, body playersResponse) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Debug("write response", "error", err)
	}
}
