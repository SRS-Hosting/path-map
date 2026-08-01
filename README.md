# path-map

[![Release](https://github.com/SRS-Hosting/path-map/actions/workflows/release.yaml/badge.svg)](https://github.com/SRS-Hosting/path-map/actions/workflows/release.yaml) [![License](https://badgen.net/github/license/SRS-Hosting/path-map)](https://github.com/SRS-Hosting/path-map/blob/main/LICENSE) [![Version](https://img.shields.io/github/release/SRS-Hosting/path-map.svg)](https://github.com/SRS-Hosting/path-map/releases/) [![Coverage](.github/badges/coverage.svg)](https://github.com/SRS-Hosting/path-map/actions/workflows/test.yaml)

A live, browser-viewable player map for a self-hosted
[Path of Titans](https://pathoftitans.com) dedicated server, read over RCON.
Everyone online appears on the map with species, growth, health, and stamina —
no client mods, no players pasting `/mapbug` coordinates.

Single static binary, `FROM scratch` container. Polling is demand-driven:
while nobody has the map open, your game server hears nothing.

## Setup

1. **Get a map image.** It is not bundled: the map art belongs to Alderon,
   so it has to come from your own copy of the game — export the minimap
   texture from the client's pak files with an Unreal asset tool, or crop a
   full-screen in-game map capture square. Save it as `gondwa.png`,
   `panjura.png`, or `riparia.png`.
2. **Run path-map**, pointing it at the image and your server's RCON:

   ```sh
   path-map --rcon.host=my-server --rcon.password=secret --map.imagePath=/maps
   ```

   or with Docker:

   ```sh
   docker run -p 8080:8080 \
     -v /srv/pot-maps:/maps:ro \
     -e RCON_HOST=my-server -e RCON_PASSWORD=secret -e MAP_IMAGEPATH=/maps \
     ghcr.io/srs-hosting/path-map:latest
   ```

3. Open `http://localhost:8080`.

`map.imagePath` can be a single image file, or a directory holding one
`<map>.png` per map — the directory layout is what map auto-detection needs.

## Configuration

Configuration comes from `config.yaml`, environment variables (`_` joins
nesting), or flags. `rcon.password` is required; everything else has a
default.

| Key | Env | Default | |
|---|---|---|---|
| `logLevel` | `LOGLEVEL` | `info` | debug, info, warn, error |
| `http.bind` | `HTTP_BIND` | *(all interfaces)* | |
| `http.port` | `HTTP_PORT` | `8080` | |
| `rcon.host` | `RCON_HOST` | `127.0.0.1` | |
| `rcon.port` | `RCON_PORT` | `7779` | Path of Titans' RCON port |
| `rcon.password` | `RCON_PASSWORD` | — | required |
| `rcon.timeoutSeconds` | `RCON_TIMEOUTSECONDS` | `5` | whole-exchange deadline |
| `rcon.maxConcurrent` | `RCON_MAXCONCURRENT` | `4` | |
| `map.name` | `MAP_NAME` | `auto` | auto, gondwa/island, panjura, riparia, or custom |
| `map.imagePath` | `MAP_IMAGEPATH` | — | required; PNG file or per-map directory |
| `map.halfExtentX` | `MAP_HALFEXTENTX` | `0` | override; 0 = use the map's calibrated value |
| `map.halfExtentY` | `MAP_HALFEXTENTY` | `0` | |
| `poller.intervalSeconds` | `POLLER_INTERVALSECONDS` | `10` | |
| `poller.idleAfterSeconds` | `POLLER_IDLEAFTERSECONDS` | `30` | |
| `poller.health` | `POLLER_HEALTH` | `true` | sample player vitals (health and stamina) |
| `poller.healthPerPoll` | `POLLER_HEALTHPERPOLL` | `4` | players whose vitals are sampled per poll |

- The map is auto-detected by default; set `map.name` to pin it. The
  official maps have calibrated world-to-image coordinates built in
  (`island` is accepted for Gondwa — it is the `ServerMap` name in
  `Game.ini`). A custom or modded map works with any name plus **both**
  `map.halfExtent*` values.
- YAML caveat: the half extents are floats, and a YAML **integer** is
  rejected — write `504000.0`, not `504000`. Environment variables and flags
  are unaffected.

## Behaviour worth knowing

- Polling runs only while a browser asked for the map recently — hidden tabs
  do not count, and any number of viewers share one poll. RCON runs on the
  game thread, so this is what keeps the map from taxing your server.
- If the server hiccups, the map keeps showing the last good snapshot with a
  visible stale label; a partial response renders what arrived, marked
  incomplete. It never blanks and never fails silently.
- Health and stamina are per-player over RCON — the game will not report them
  for everyone at once — so they are budgeted instead of scraped: each poll asks
  at most `poller.healthPerPoll` players, least recently asked first, and every
  player refreshes every `ceil(players / healthPerPoll)` polls. One command
  carries both vitals and both maxima, so the cost does not depend on how many
  values are shown, and it never depends on how many players are online.
  Positions keep their full cadence; vitals age, and the roster says how old
  each reading is. Markers fill by **health** and colour by band (red, yellow,
  green, blue at full) — stamina is roster text only, so one glance still
  answers "who is dying". A player nobody has sampled yet, or who has no pawn,
  shows grey and reads "unknown" rather than pretending to be at 0%.
  `poller.health: false` turns both off and costs the game nothing.
- The web surface is read-only and unauthenticated. If player positions
  should not be public, put it behind your ingress's authentication.
