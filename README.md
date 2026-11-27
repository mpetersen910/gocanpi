# gocanpi

Two Linux SocketCAN programs:

- **colcan** — collector. Reads CAN / CAN-FD frames and exports Prometheus metrics on `:9100`.
- **gencan** — bench traffic generator: a periodic schedule (`standard`), pulses of one ID (`burst`), or an on-change sensor (`event`).

Cluster deploy and Pi bring-up live in the sister repo **can-gitops**.

## colcan

Linux only (`AF_CAN`). Needs `CAP_NET_RAW` (or root) and an up interface.

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o colcan ./cmd/colcan
./colcan -ifaces can0,can1 -catalog catalog.yaml
curl -s http://127.0.0.1:9100/metrics | grep '^can_'
```

| Flag | Default | |
|------|---------|-|
| `-ifaces` | `can1` | comma-separated SocketCAN ifaces |
| `-listen` | `:9100` | metrics / health bind |
| `-per-id` | `true` | per-CAN-ID series |
| `-max-ids` | `256` | cap, then `overflow` |
| `-no-fd` | `false` | classic frames only |
| `-no-errors` | `false` | skip SocketCAN error frames |
| `-catalog` | | YAML message catalog; see below |
| `-include-local` | `false` | also count frames sent from this machine |

`GET /metrics` · `GET /healthz`

### Wire only

colcan only counts what it hears on the wire, as a separate listener would. Linux gives every socket on an interface a copy of what other programs on the same machine send there, and marks those copies (`MSG_DONTROUTE`). colcan drops them and counts them in `can_frames_local_dropped_total{iface}`.

On a bench where the same machine also transmits, read every interface: each reader then hears the other node's frames from the wire, and every frame is counted exactly once. The `iface` label means "heard on", so sum across it (`max` for timestamps).

Arrival times come from the kernel (`SO_TIMESTAMPNS`), so they don't include colcan's own scheduling delay. Nothing has to be added to a payload.

### Catalog

What a spec sheet (a DBC file, the NMEA 2000 PGN list) says about each message. Every cataloged message gets checks for its type, and each metric below carries `{iface, can_id, message}`. Messages not in the catalog only get the generic per-ID metrics.

```yaml
messages:
  - id: "100"          # hex as candump prints it: up to 3 digits standard, more extended
    name: fast_counter
    type: periodic     # periodic or event
    period: 5ms        # periodic only
    counter: 0-3       # optional: big-endian rolling counter bytes
  - id: "400"
    name: cabin_temp
    type: event
    min_gap: 90ms      # event only, optional
    max_silence: 7.5s  # event only
    counter: 2-2
```

Loading is strict: an unknown key, a field from the other type, a missing required field, or a duplicate ID or name stops colcan at startup.

Every message:

- `can_frame_last_seen_timestamp_seconds` — arrival time of the latest frame. Starts at colcan's start time, so a message never heard still ages. Late and missed counts only move once a frame finally arrives; this shows a dead sender while it is still dead.

`periodic` — the interval since the previous frame, divided by `period`:

- `can_frame_interval_ratio` — histogram. 1 is on time, 2 is one frame missing.
- `can_frames_late_total` — ratio 1.25 to 1.5.
- `can_frames_missed_estimated_total` — ratio 1.5 or more adds `round(ratio) - 1`.

`event` — no period to be late against, so the checks are the sender's promises:

- `can_frame_interval_seconds` — histogram of the time between frames.
- `can_frames_too_soon_total` — a gap under `min_gap` (the sender is flooding).
- `can_frames_overdue_total` — a gap over `max_silence` (a missed heartbeat).

`counter` — exact drops from the message's own rolling counter. For an event message, whose timing promises nothing, this is the only way to see them.

- `can_frames_missed_total` — gaps in the counter.
- `can_sequence_restarts_total` — the counter went backwards, meaning the sender restarted. Never counted as missed.

Timing and counters disagree in predictable ways:

- Timing can't tell a frame delayed by more than half a period from a lost one; a counter can, because the late frame still carries the next number.
- Timing counts every expected frame that didn't arrive. A counter counts frames the sender numbered that never arrived. When the sender restarts, its counter starts again at 0, so the outage shows up as a restart instead of losses. Frames lost while the sender kept running (an unplugged bus, a full tx queue) are counted by both.

For a periodic message with a counter, the difference between the two missed counts shows these limits.

```promql
# liveness: seconds since cabin_temp was last heard (alert above max_silence)
time() - max by (can_id, message) (can_frame_last_seen_timestamp_seconds{message="cabin_temp"})
# event intervals (heatmap)
sum by (le) (increase(can_frame_interval_seconds_bucket{message="cabin_temp"}[10m]))
# flooding and missed heartbeats
sum by (message) (increase(can_frames_too_soon_total[5m]))
sum by (message) (increase(can_frames_overdue_total[5m]))
# periodic interval distribution (heatmap)
sum by (le) (increase(can_frame_interval_ratio_bucket{message="fast_counter"}[1m]))
# late frames per minute
sum by (message) (increase(can_frames_late_total[1m]))
# exact vs estimated misses
sum by (message) (increase(can_frames_missed_total[1m]))
sum by (message) (increase(can_frames_missed_estimated_total[1m]))
# sender restarts
sum by (message) (increase(can_sequence_restarts_total[1h]))
# message rate per ID
sum by (can_id) (rate(can_frames_received_total[1m]))
```

## gencan

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o gencan ./cmd/gencan
./gencan -mode standard -iface can0 -scale 0.5
./gencan -mode burst -iface can0 -burst-id 7FF -burst-count 200 -burst-every 60s
./gencan -mode event -iface can1 -event-id 400 -event-heartbeat 5s
```

| Flag | Default | |
|------|---------|-|
| `-mode` | `standard` | `standard`: periodic schedule; `burst`: pulses of one ID; `event`: on-change sensor |
| `-iface` | `can0` | interface to transmit on |
| `-quiet` | `false` | no 30 s summary, no per-frame event log |
| `-scale` | `1.0` | standard: period multiplier |
| `-burst-id` | `7FF` | burst: standard ID, hex |
| `-burst-count` | `200` | burst: frames per pulse |
| `-burst-every` | `60s` | burst: time between pulses; `0` is back to back |
| `-event-id` | `400` | event: standard ID, hex |
| `-event-deadband` | `0.5` | event: change in °C that makes a frame due |
| `-event-min-gap` | `100ms` | event: shortest spacing between frames |
| `-event-heartbeat` | `5s` | event: longest silence; `0` disables |
| `-event-seed` | `1` | event: random walk seed |

A flag that belongs to another mode is rejected.

Standard schedule at `-scale 1`: `100` every 10 ms (rolling counter in bytes 0–3), `200` every 100 ms, `300` every 50 ms (FD with BRS), `18DAF110` every 1 s. A frame the tx queue won't take is dropped. Burst retries on ENOBUFS, so every frame in a pulse reaches the bus.

Event mode measures a random-walk temperature every 20 ms (internal; never on the bus) and sends a frame when the reading has moved by the deadband, no sooner than the minimum gap after the previous frame, and at least once per heartbeat. Its frames therefore arrive at irregular intervals. Payload: bytes 0–1 temperature as a signed big-endian int16 in 0.1 °C, byte 2 an 8-bit rolling counter. Each frame is logged with its reason:

```
event: #5 22.4 C after 100ms (change, held 60ms by min gap)
event: #9 24.3 C after 5s (heartbeat)
```

Two senders must never share an ID, so keep the burst and event IDs out of the schedule and away from each other.

## Docker

```bash
docker buildx build --platform linux/arm64 -t gocanpi --load .
docker buildx build --platform linux/arm64 --build-arg TARGET=./cmd/gencan -t gencan --load .
docker run --network host --cap-add NET_RAW gocanpi -ifaces can1
```

Static binary, `scratch` image. Host must already have `can0` / `can1` up. `build.sh` builds both and pushes them to the bench registry.

## Tests

The packages only compile for Linux:

```bash
docker run --rm -v "$PWD":/src -w /src golang:1.26.6 go test ./...
```
