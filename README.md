[![Go Reference](https://pkg.go.dev/badge/github.com/MERKAT0R/go-rflink/rflink.svg)](https://pkg.go.dev/github.com/MERKAT0R/go-rflink/rflink)
[![CI](https://github.com/MERKAT0R/go-RFLink/actions/workflows/goRFLink.yml/badge.svg)](https://github.com/MERKAT0R/go-RFLink/actions/workflows/goRFLink.yml)
[![Go Version](https://img.shields.io/github/go-mod/go-version/MERKAT0R/go-RFLink)](https://github.com/MERKAT0R/go-RFLink)

# go-rflink

Bridge [RFLink](https://www.rflink.nl/) Gateway serial traffic to MQTT (MQTT 5 via [paho.golang/autopaho](https://github.com/eclipse-paho/paho.golang)).

Per-sensor topics keyed by **HWID** (`MODEL_ID`), optional Home Assistant discovery, command bridge with whitelist/rate-limit, health metrics, serial PING watchdog and debounce.

## Features

- MQTT 5 client with auto-reconnect
- Sensor topics by HWID; optional `PUBLISH_HWID_MAP` for stable canonical IDs
- Field topics + `sumJson` (includes `received_at`, `timestamp`, optional `friendly name`)
- Temperature units: `C` (default), `F`, or `RAW`
- Multi-button remotes: `SWITCH`+`CMD` → separate `sw_XX` binary channels
- Debounce: drops radio retransmits within `PUBLISH_DEDUP_WINDOW_MS` (ignores RFLink sequence)
- Pending latest-state buffer while MQTT is down (no `cmd`/`switch` replay)
- MQTT → RFLink commands with normalize, whitelist, rate-limit, `cmd/result` ack
- Home Assistant discovery: sensors, battery, per-button binary_sensors, optional custom buttons/switches with commands
- Status topics: app, serial, health, gateway info
- RFLink `10;PING;` keepalive + unresponsive detection
- Ignore-list for noisy models/IDs for ex.`"Keeloq"`
- TLS (CA file / insecure skip with warning)
- Graceful shutdown (`SIGINT` / `SIGTERM`, Windows + Linux)

## MQTT topic layout

Base topic default: `rflink`.

| Topic | Direction | Description |
|-------|-----------|-------------|
| `rflink/{HWID}/temp` | pub | Temperature |
| `rflink/{HWID}/hum` | pub | Humidity |
| `rflink/{HWID}/bat` | pub | Battery (`OK` / `LOW`) |
| `rflink/{HWID}/sw_{N}` | pub | Button/channel `N` state (`ON`/`OFF`) |
| `rflink/{HWID}/sumJson` | pub | Full sensor JSON |
| `rflink/send` | sub | Commands to RFLink (override with `PUBLISH_CMD_TOPIC`) |
| `rflink/cmd/result` | pub | Command outcome (`ok`, `rejected`, `rate_limited`, …) |
| `rflink/status` | pub | Process/MQTT availability (`online`/`offline`) |
| `rflink/serial/status` | pub | Serial port availability |
| `rflink/health` | pub | Metrics JSON |
| `rflink/gateway/info` | pub | Version / PONG |
| `rflink/admin/rediscover` | sub | Clear HA discovery cache (when HA discovery enabled) |

### sumJson example

```json
{
  "hwid": "EV1527_0B25F2",
  "model": "EV1527",
  "id": "0b25f2",
  "name": "goRFLink_EV1527 0b25f2",
  "switch": "04",
  "cmd": "ON",
  "timestamp": "2026-08-08T15:33:14.138+03:00",
  "received_at": "2026-08-08T15:33:14.138+03:00"
}
```

## Quick start

```bash
# optional: source env.sh.example after editing
export PUBLISH_HOST=192.168.0.1:1883
export SERIAL_DEVICE=/dev/ttyACM0
export PUBLISH_HOME_ASSISTANT_DISCOVERY=true

go run .
```

### Commands to RFLink

```bash
mosquitto_pub -t rflink/send -m "10;NewKaku;123456;1;OFF;"
# result on rflink/cmd/result
```

Commands are trimmed, `;` is appended if missing, max length enforced, optional prefix whitelist and rate-limit applied.

### Home Assistant

```bash
export PUBLISH_HOME_ASSISTANT_DISCOVERY=true
export PUBLISH_FRIENDLY_NAMES="EV1527_0B25F2:Front Door Remote"
export PUBLISH_HA_BUTTONS="Siren:10;NewKaku;123456;ON;"
export PUBLISH_HA_SWITCHES="Garden:10;NewKaku;AABB;ON;:10;NewKaku;AABB;OFF;"
```

- Device name defaults to `goRFLink_{MODEL} {ID}` if no friendly name is set
- Entity names are short (`Temperature`, `Button 04`) to avoid doubled HA entity_ids
- Rediscover: `mosquitto_pub -t rflink/admin/rediscover -m 1`

### TLS

```bash
export PUBLISH_SCHEME=ssl
export PUBLISH_HOST=mqtt.example.com:8883
export PUBLISH_TLS_CA_FILE=/path/to/ca.crt
```

### Docker

```bash
docker build -t go-rflink .
docker run --device=/dev/ttyACM0 \
  -e PUBLISH_HOST=192.168.0.1:1883 \
  -e SERIAL_DEVICE=/dev/ttyACM0 \
  -e PUBLISH_HOME_ASSISTANT_DISCOVERY=true \
  go-rflink:latest
```

### Docker Compose

Includes optional Mosquitto for local tests. Serial device path is configurable.

```bash
# optional: copy env sample
cp env.sh.example .env   # edit SERIAL_DEVICE / PUBLISH_* as needed

# build & run (default serial /dev/ttyUSB0, broker mosquitto:1883)
SERIAL_DEVICE=/dev/ttyACM0 docker compose up -d --build

# logs
docker compose logs -f go-rflink

# use an external broker instead of the bundled Mosquitto:
# set PUBLISH_HOST=192.168.0.1:1883 and remove/comment the mosquitto service
```

See [`docker-compose.yml`](docker-compose.yml) and [`deploy/mosquitto/mosquitto.conf`](deploy/mosquitto/mosquitto.conf).

## Configuration

Full sample: [`env.sh.example`](env.sh.example). Only override what you need.

### Global

| Variable | Default | Description                      |
|----------|---------|----------------------------------|
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `GORFLINK_DEBUG` | `false` | Force debug log level for tests  |

### MQTT publish

| Variable | Default | Description |
|----------|---------|-------------|
| `PUBLISH_HOST` | `localhost:1883` | Broker host:port |
| `PUBLISH_SCHEME` | `tcp` | `tcp`/`mqtt`, `ssl`/`tls`/`mqtts`, `ws`, `wss` |
| `PUBLISH_MQTT_USERNAME` | `username` | MQTT username |
| `PUBLISH_MQTT_PASSWORD` | `password` | MQTT password |
| `PUBLISH_CLIENT_ID` | `gorflink` | MQTT client id |
| `PUBLISH_TOPIC` | `rflink` | Base topic |
| `PUBLISH_CMD_TOPIC` | `{topic}/send` | Command subscribe topic |
| `PUBLISH_QOS` | `0` | QoS 0–2 |
| `PUBLISH_RETAIN` | `false` | Default retain (event fields force false) |
| `PUBLISH_CLEAN_SESSION` | `true` | Clean start on first connect |
| `PUBLISH_KEEPALIVE` | `30` | Keepalive seconds |
| `PUBLISH_SESSION_EXPIRY` | `0` | MQTT 5 session expiry seconds |
| `PUBLISH_INFINITY_RECONNECT` | `true` | Background reconnect |
| `PUBLISH_CONNECT_RETRY_INTERVAL` | `5` | Reconnect backoff base (seconds) |
| `PUBLISH_TLS_CA_FILE` | | CA certificate path |
| `PUBLISH_TLS_INSECURE_SKIP_VERIFY` | `false` | Skip TLS verify (warned) |
| `PUBLISH_LWT_*` | | Optional custom LWT (default: `{topic}/status` offline) |
| `PUBLISH_TEMPERATURE_UNIT` | `C` | `C`, `F`, or `RAW` |
| `PUBLISH_FRIENDLY_NAMES` | | `HWID:Name,HWID2:Name2` |
| `PUBLISH_HWID_MAP` | | `ID:CanonicalHWID,...` |
| `PUBLISH_IGNORE_LIST` |`Keeloq` | Comma-separated model/HWID/ID to drop |
| `PUBLISH_DEDUP_WINDOW_MS` | `3000` | Debounce window (`0` = off) |
| `PUBLISH_PENDING_TTL_SEC` | `60` | Latest-state buffer while MQTT down |
| `PUBLISH_HEALTH_LOG_INTERVAL_SEC` | `300` | Health log/publish interval (`0` = off) |
| `PUBLISH_COMMAND_WHITELIST` | | Allowed prefixes, e.g. `10;,11;` (empty = all) |
| `PUBLISH_COMMAND_RATE_LIMIT` | `5` | Max commands/sec to serial (`0` = unlimited) |
| `PUBLISH_COMMAND_MAX_LEN` | `256` | Max normalized command length |
| `PUBLISH_HOME_ASSISTANT_DISCOVERY` | `false` | Enable HA discovery |
| `PUBLISH_HA_DISCOVERY_PREFIX` | `homeassistant` | HA discovery prefix |
| `PUBLISH_HA_BUTTONS` | | `Label:10;cmd;,Label2:10;cmd2;` |
| `PUBLISH_HA_SWITCHES` | | `Label:on_cmd:off_cmd,...` |

### Serial

| Variable | Default | Description |
|----------|---------|-------------|
| `SERIAL_DEVICE` | `/dev/ttyUSB0` | Serial device path |
| `SERIAL_BAUD` | `57600` | Baud rate |
| `SERIAL_READ_TIMEOUT_SEC` | `10` | Read timeout |
| `SERIAL_RECONNECT_MIN_DELAY_SEC` | `3` | Min reconnect delay |
| `SERIAL_RECONNECT_MAX_DELAY_SEC` | `60` | Max reconnect delay |
| `SERIAL_WATCHDOG_SEC` | `120` | Warn if no serial data (`0` = off) |
| `SERIAL_PING_INTERVAL_SEC` | `60` | `10;PING;` interval (`0` = off) |
| `SERIAL_PING_TIMEOUT_SEC` | `5` | PONG wait timeout |
| `SERIAL_PING_FAIL_THRESHOLD` | `3` | Failures before `rflink_unresponsive` |

## Health payload (excerpt)

```json
{
  "version": "1.2.3",
  "git_sha": "abc1234",
  "started_at": "2026-08-08T12:00:00Z",
  "status": "online",
  "mqtt_connected": true,
  "serial_connected": true,
  "serial_stale": false,
  "rflink_unresponsive": false,
  "last_pong_at": "2026-08-08T12:05:00Z",
  "sensors_last_seen": { "EV1527_0B25F2": "2026-08-08T12:04:59Z" },
  "dedup_dropped": 3,
  "published": 120
}
```

## Roadmap / TODO

Ideas for further development (not scheduled; PRs welcome).

### High value

- [ ] **HTTP API** — local control without MQTT only
  - `GET /healthz` / `GET /readyz` for Docker/K8s probes
  - `GET /api/v1/health` — same JSON as MQTT health topic
  - `GET /api/v1/sensors` — last-seen map, optional recent samples
  - `GET /api/v1/rflink/raw` optional for debug without DEBUG logs
  - `POST /api/v1/command` — send RFLink command (same normalize/whitelist/rate-limit)
  - `POST /api/v1/ha/rediscover` — trigger discovery reset
- [ ] **Web GUI** — optional lightweight status UI (embedded)
  - Live serial/MQTT state, last sensors, ping/watchdog
  - Send test command, view recent raw lines
  - Edit friendly names / ignore list (persist to file or config API)
- [ ] **Prometheus metrics** — `/metrics` (published, dropped, ping fails, connected gauges)
- [ ] **Per-sensor throttle** — min publish interval per HWID for chatty devices

### Home Assistant / devices

- [ ] **Device triggers / events** for momentary RF buttons (alongside `off_delay` binary_sensors)
- [ ] **HA switch state feedback** when RFLink echoes RX after TX
- [ ] **Per-sensor availability** based on last-seen, not only global serial status
- [ ] **Rediscover + purge** helper docs/script for stale HA entities

### Reliability & serial
- [ ] **Multiple serial ports** / multiple RFLink gateways in one process (maybe someday - not priority)

### Security & ops

- [ ] **mTLS client certificates** for MQTT
- [ ] **HTTP API auth** (token / basic) when GUI/API is exposed (locked on no-GUI)
- [ ] **Structured audit log** of commands (who/when/result)


### Nice to have

- [ ] **Config file** (YAML/JSON) in addition to env
- [ ] **Hot-reload** of ignore list / friendly names / whitelist
- [ ] **Publish Coverage + integration tests** (fake serial + mock MQTT)
- [ ] **OpenSSF Scorecard** badge / workflow

## License

AGPL-3.0-or-later
