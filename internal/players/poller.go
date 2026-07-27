package players

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/USA-RedDragon/rcon"
)

// commandPlayerInfoAll is the data source. ListPlayerPositions would be
// lighter, but its populated output shape has never been observed — only its
// empty-server header is verified — and PlayerInfoAll carries the roster data
// the sidebar wants anyway.
const commandPlayerInfoAll = "PlayerInfoAll"

// commandListPOI feeds map detection; see detect.go.
const commandListPOI = "ListPOI"

// Poller polls the game while anyone is watching and caches the result. It
// never lets a poll failure erase the last good snapshot: a map frozen with a
// visible "stale" label is more useful during a server hiccup than one that
// blanks.
type Poller struct {
	client    *rcon.Client
	interval  time.Duration
	idleAfter time.Duration
	// fixed pins the map when the operator named it; nil means detect. lookup
	// resolves a detected canonical name to its calibration and is what keeps
	// this package from owning the preset table.
	fixed  *MapInfo
	lookup func(name string) (MapInfo, bool)

	// wake lets the first viewer after an idle stretch trigger a poll
	// immediately instead of waiting out the current tick. Capacity 1: a
	// wake-up is a level, not a count.
	wake chan struct{}

	mu         sync.Mutex
	snap       *Snapshot // nil until the first successful poll
	resolved   *MapInfo  // nil while the map is undetected
	errMsg     string    // last failure, cleared on success
	lastDemand time.Time
	// failed records an RCON-level failure. In auto mode the next poll drops
	// the map resolution first: an outage is the signature of a server
	// restart, and a restart is the only way ServerMap changes.
	failed bool
}

// NewPoller builds a Poller. fixed non-nil pins the map and disables
// detection; otherwise lookup resolves detected map names.
func NewPoller(client *rcon.Client, interval, idleAfter time.Duration, fixed *MapInfo, lookup func(string) (MapInfo, bool)) *Poller {
	return &Poller{
		client:    client,
		interval:  interval,
		idleAfter: idleAfter,
		fixed:     fixed,
		lookup:    lookup,
		wake:      make(chan struct{}, 1),
		resolved:  fixed,
	}
}

// Observe returns the cached snapshot, the resolved map, and the last error
// message, and records that a viewer asked — which is what keeps the poll
// loop running. It never blocks on RCON.
func (p *Poller) Observe() (*Snapshot, *MapInfo, string) {
	p.mu.Lock()
	p.lastDemand = time.Now()
	snap, resolved, errMsg := p.snap, p.resolved, p.errMsg
	p.mu.Unlock()

	if snap == nil || time.Since(snap.GeneratedAt) > p.interval {
		select {
		case p.wake <- struct{}{}:
		default:
		}
	}
	return snap, resolved, errMsg
}

// Resolved returns the current map without registering viewer demand — for
// callers like the image handler that follow a page load rather than drive
// polling.
func (p *Poller) Resolved() *MapInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.resolved
}

// Run polls until ctx is cancelled. It is the only writer of the snapshot.
func (p *Poller) Run(ctx context.Context) {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.mu.Lock()
			idle := time.Since(p.lastDemand) > p.idleAfter
			p.mu.Unlock()
			if idle {
				// This branch is the whole point of demand-driven polling:
				// RCON runs on the game thread, and a map nobody is looking
				// at should cost the game nothing.
				slog.Debug("poll skipped, no recent viewers")
				continue
			}
			p.poll(ctx)
		case <-p.wake:
			p.mu.Lock()
			fresh := p.snap != nil && time.Since(p.snap.GeneratedAt) <= p.interval
			p.mu.Unlock()
			if fresh {
				// Rate-limits wake storms: many viewers arriving at once get
				// the snapshot the first one already triggered.
				continue
			}
			p.poll(ctx)
			// A wake poll replaces the imminent tick rather than stacking
			// onto it.
			ticker.Reset(p.interval)
		}
	}
}

// poll runs one cycle: resolve the map if needed, then fetch and publish the
// player snapshot.
func (p *Poller) poll(ctx context.Context) {
	info, ok := p.resolveMap(ctx)
	if !ok {
		return
	}

	out, err := p.execute(ctx, commandPlayerInfoAll)
	if err != nil && out == "" {
		p.fail("player poll failed", err)
		return
	}
	if err != nil {
		// A partial response: pages lost to expiry or a mid-series stall.
		// What arrived is real — on a full server, page 1 alone is ~26
		// players — so it is published as a fresh, visibly incomplete
		// snapshot rather than freezing the map on an ever-older complete
		// one. The next poll retries the full series from scratch anyway.
		slog.Warn("player poll returned a partial response", "error", err)
	}

	snap := Parse(out)
	for i := range snap.Players {
		if snap.Players[i].HasPosition {
			snap.Players[i].U, snap.Players[i].V = uv(
				snap.Players[i].X, snap.Players[i].Y, info.HalfExtentX, info.HalfExtentY)
		}
	}
	snap.GeneratedAt = time.Now()

	switch {
	case err != nil:
		// Already warned above; the integrity shortfall is expected here.
	case snap.Complete:
		slog.Debug("polled", "players", len(snap.Players))
	default:
		// Loudly: with a clean transport this is the "parser broke or a page
		// went missing" signal the Total Players header exists to provide.
		slog.Warn("player list incomplete", "parsed", len(snap.Players), "total", snap.Total)
	}

	p.mu.Lock()
	p.snap = &snap
	if err != nil {
		p.errMsg = publicMessage(err)
	} else {
		p.errMsg = ""
	}
	// The server answered — a partial answer is still an answer — so this is
	// not an outage and must not trigger re-detection.
	p.failed = false
	p.mu.Unlock()
}

// resolveMap returns the map to project onto, detecting it first when needed.
func (p *Poller) resolveMap(ctx context.Context) (MapInfo, bool) {
	p.mu.Lock()
	if p.failed && p.fixed == nil {
		p.resolved = nil
	}
	resolved := p.resolved
	p.mu.Unlock()
	if resolved != nil {
		return *resolved, true
	}

	out, err := p.execute(ctx, commandListPOI)
	if err != nil {
		p.fail("map detection failed", err)
		return MapInfo{}, false
	}

	name, ok := Detect(out)
	if !ok {
		slog.Warn("map detection ambiguous", "bytes", len(out))
		p.setError("could not identify the map from ListPOI; set map.name explicitly if this persists")
		return MapInfo{}, false
	}
	info, ok := p.lookup(name)
	if !ok {
		// Detect only returns fingerprint keys, so this means the fingerprint
		// and preset tables drifted apart — a bug, not a runtime condition,
		// but surfaced rather than panicking.
		slog.Warn("detected map has no preset", "map", name)
		p.setError("detected unknown map " + name)
		return MapInfo{}, false
	}

	slog.Info("map detected", "map", name)
	p.mu.Lock()
	p.resolved = &info
	// Detection succeeding proves the server is back; without this a
	// pre-outage failure flag would throw away the resolution we just made.
	p.failed = false
	p.mu.Unlock()
	return info, true
}

// execute runs command, waiting out the client's busy backpressure bounded by
// the client's own timeout. The client fails fast on a busy slot and releases
// slots from a goroutine that can lose a race against an immediately
// following claim, so the detect-then-poll sequence would otherwise trip over
// its own heels on a capacity-1 client — and genuine contention is worth
// waiting out rather than skipping a poll over.
func (p *Poller) execute(ctx context.Context, command string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, p.client.Timeout())
	defer cancel()

	for {
		body, err := p.client.Execute(ctx, command)
		if !errors.Is(err, rcon.ErrBusy) {
			return body, err
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("rcon: %s: %w", p.client.Addr(), ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
}

// fail records an RCON-level failure: the signature of an unreachable or
// restarted server.
func (p *Poller) fail(msg string, err error) {
	slog.Warn(msg, "error", err)
	p.mu.Lock()
	p.errMsg = publicMessage(err)
	p.failed = true
	p.mu.Unlock()
}

// publicMessage is the error text the page may show. The raw error goes to
// the log for the operator; this string reaches every viewer's browser, so it
// names the situation without naming the RCON address or other internals.
func publicMessage(err error) string {
	var timeout *rcon.TimeoutError
	switch {
	case errors.As(err, &timeout):
		return "the game server did not respond"
	case errors.Is(err, rcon.ErrTruncated):
		return "received a partial response from the game server"
	case errors.Is(err, rcon.ErrAuthFailed):
		return "RCON authentication failed; check the configured password"
	default:
		return "cannot reach the game server"
	}
}

func (p *Poller) setError(msg string) {
	p.mu.Lock()
	p.errMsg = msg
	p.mu.Unlock()
}
