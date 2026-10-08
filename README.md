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

Configuration comes from `config.yaml` (see
[`config.example.yaml`](config.example.yaml)), environment variables (`_`
joins nesting), or flags. `rcon.password` is required; everything else has a
default.

<!-- configulator:begin -->

| Key                       | Type    | Default     | Environment               | Flag                        | Description                                                                                                                      |
|---------------------------|---------|-------------|---------------------------|-----------------------------|----------------------------------------------------------------------------------------------------------------------------------|
| `logLevel`                | string  | `info`      | `LOGLEVEL`                | `--logLevel`                | log verbosity: debug, info, warn, or error                                                                                       |
| `http.bind`               | string  |             | `HTTP_BIND`               | `--http.bind`               | address to listen on; empty listens on all interfaces over both IPv4 and IPv6                                                    |
| `http.port`               | integer | `8080`      | `HTTP_PORT`               | `--http.port`               | TCP port to listen on                                                                                                            |
| `rcon.host`               | string  | `127.0.0.1` | `RCON_HOST`               | `--rcon.host`               | hostname or IP of the Source RCON server                                                                                         |
| `rcon.port`               | integer | `7779`      | `RCON_PORT`               | `--rcon.port`               | TCP port of the Source RCON server                                                                                               |
| `rcon.password`           | string  |             | `RCON_PASSWORD`           | `--rcon.password`           | RCON password (required) (secret)                                                                                                |
| `rcon.timeoutSeconds`     | integer | `5`         | `RCON_TIMEOUTSECONDS`     | `--rcon.timeoutSeconds`     | deadline in seconds covering a whole RCON exchange: connect, authenticate, command, response                                     |
| `rcon.maxConcurrent`      | integer | `4`         | `RCON_MAXCONCURRENT`      | `--rcon.maxConcurrent`      | maximum RCON commands in flight at once                                                                                          |
| `map.name`                | string  | `auto`      | `MAP_NAME`                | `--map.name`                | map the server runs: auto (detect over RCON), gondwa (aka island), panjura, riparia, or a custom name with both half extents set |
| `map.imagePath`           | string  |             | `MAP_IMAGEPATH`           | `--map.imagePath`           | map background image: a PNG file, or a directory holding <map>.png per map (required)                                            |
| `map.halfExtentX`         | number  | `0`         | `MAP_HALFEXTENTX`         | `--map.halfExtentX`         | world half extent on the X axis in Unreal units; 0 uses the named map's calibrated value                                         |
| `map.halfExtentY`         | number  | `0`         | `MAP_HALFEXTENTY`         | `--map.halfExtentY`         | world half extent on the Y axis in Unreal units; 0 uses the named map's calibrated value                                         |
| `poller.intervalSeconds`  | integer | `10`        | `POLLER_INTERVALSECONDS`  | `--poller.intervalSeconds`  | seconds between player polls while the map has viewers                                                                           |
| `poller.idleAfterSeconds` | integer | `30`        | `POLLER_IDLEAFTERSECONDS` | `--poller.idleAfterSeconds` | seconds without a browser request after which polling stops                                                                      |
| `poller.health`           | boolean | `true`      | `POLLER_HEALTH`           | `--poller.health`           | sample player vitals (health and stamina) while the map has viewers                                                              |
| `poller.healthPerPoll`    | integer | `4`         | `POLLER_HEALTHPERPOLL`    | `--poller.healthPerPoll`    | players whose vitals are sampled per poll; vitals age between samples, positions do not                                          |

<!-- configulator:end -->

- The map is auto-detected by default; set `map.name` to pin it. The
  official maps have calibrated world-to-image coordinates built in
  (`island` is accepted for Gondwa — it is the `ServerMap` name in
  `Game.ini`). A custom or modded map works with any name plus **both**
  `map.halfExtent*` values.

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
