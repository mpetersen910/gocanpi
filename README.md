# gocanpi

Two Linux SocketCAN programs:

- **colcan** — collector. Reads CAN / CAN-FD frames and exports Prometheus metrics on `:9100`.
- **gencan** — bench traffic generator: a periodic schedule (`standard`) or pulses of one ID (`burst`).

Cluster deploy and Pi bring-up live in the sister repo **can-gitops**.

## colcan

Linux only (`AF_CAN`). Needs `CAP_NET_RAW` (or root) and an up interface.

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o colcan ./cmd/colcan
./colcan -ifaces can1 -periods 100:5ms,200:50ms -counters 100:0-3
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
| `-periods` | | expected period per ID, e.g. `100:5ms,18DAF110:500ms` |
| `-counters` | | rolling-counter bytes per ID, big-endian, e.g. `100:0-3` |

IDs are hex the way candump prints them: up to three digits is standard, more is extended.

`GET /metrics` · `GET /healthz`

### Timing and drops

colcan only observes; nothing has to be added to a payload. Arrival times come from the kernel (`SO_TIMESTAMPNS`), so they don't include colcan's own scheduling delay.

`-periods` — the interval since the previous frame of the same ID, divided by its expected period:

- `can_frame_interval_ratio` — histogram. 1 is on time, 2 is one frame missing.
- `can_frames_late_total` — ratio 1.25 to 1.5.
- `can_frames_missed_estimated_total` — ratio 1.5 or more adds `round(ratio) - 1`.

`-counters` — exact drops from the message's own rolling counter:

- `can_frames_missed_total` — gaps in the counter.
- `can_sequence_restarts_total` — the counter went backwards, meaning the sender restarted. Never counted as missed.

The two disagree in predictable ways:

- Timing can't tell a frame delayed by more than half a period from a lost one; a counter can, because the late frame still carries the next number.
- Timing counts every expected frame that didn't arrive. A counter counts frames the sender numbered that never arrived. When the sender restarts, its counter starts again at 0, so the outage shows up as a restart instead of losses. Frames lost while the sender kept running (an unplugged bus, a full tx queue) are counted by both.

For a message with both, the difference between the two missed counts shows these limits.

```promql
# interval distribution for 0x100 (heatmap)
sum by (le) (increase(can_frame_interval_ratio_bucket{can_id="100"}[1m]))
# late frames per minute
sum by (can_id) (increase(can_frames_late_total[1m]))
# exact vs estimated misses for 0x100
increase(can_frames_missed_total{can_id="100"}[1m])
increase(can_frames_missed_estimated_total{can_id="100"}[1m])
# sender restarts
increase(can_sequence_restarts_total[1h])
# message rate per ID
sum by (can_id) (rate(can_frames_received_total[1m]))
```

## gencan

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o gencan ./cmd/gencan
./gencan -mode standard -iface can0 -scale 0.5
./gencan -mode burst -iface can0 -burst-id 7FF -burst-count 200 -burst-every 60s
```

| Flag | Default | |
|------|---------|-|
| `-mode` | `standard` | `standard`: periodic schedule; `burst`: pulses of one ID |
| `-iface` | `can0` | interface to transmit on |
| `-quiet` | `false` | no 30 s summary |
| `-scale` | `1.0` | standard: period multiplier |
| `-burst-id` | `7FF` | burst: standard ID, hex |
| `-burst-count` | `200` | burst: frames per pulse |
| `-burst-every` | `60s` | burst: time between pulses; `0` is back to back |

A flag that belongs to the other mode is rejected.

Standard schedule at `-scale 1`: `100` every 10 ms (rolling counter in bytes 0–3), `200` every 100 ms, `300` every 50 ms (FD with BRS), `18DAF110` every 1 s. A frame the tx queue won't take is dropped. Burst retries on ENOBUFS, so every frame in a pulse reaches the bus. Two senders must never share an ID, so keep the burst ID out of the schedule.

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
