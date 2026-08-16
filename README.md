# gocanpi

Linux SocketCAN collector. Reads CAN / CAN-FD frames and exports Prometheus metrics on `:9100`.

Cluster deploy and Pi bring-up live in the sister repo **can-gitops**.

## Run

Linux only (`AF_CAN`). Needs `CAP_NET_RAW` (or root) and an up interface.

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o gocanpi .
./gocanpi -ifaces can1
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

`GET /metrics` · `GET /healthz`

## Docker

```bash
docker buildx build --platform linux/arm64 -t gocanpi --load .
docker run --network host --cap-add NET_RAW gocanpi -ifaces can1
```

Static binary, `scratch` image. Host must already have `can0` / `can1` up.
