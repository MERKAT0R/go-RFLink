# go-rflink HTTP API, Web GUI & Webhooks

Local control layer (optional). MQTT path is unchanged when HTTP is disabled (`HTTP_LISTEN` empty).

## Bootstrap

```bash
export HTTP_LISTEN=127.0.0.1:8080
export HTTP_AUTH_TOKEN='use-a-long-random-secret-16+'
export HTTP_GUI=true
# optional machine tokens / runtime overlay
# export API_AUTH_TOKEN='ha:secret:never:read+command'
# export RUNTIME_CONFIG_FILE=./data/runtime.json
```

Open `http://127.0.0.1:8080/`, log in with `HTTP_AUTH_TOKEN`.

| Condition | Behaviour |
|-----------|-----------|
| No `HTTP_AUTH_TOKEN` | **GUI locked** — no admin, raw, WebSocket, token/webhook management |
| No session and no API Bearer | Protected `/api/v1/*` → **401** (no open mode) |
| Non-loopback listen without credentials | Startup **warning** |

Prefer bind `127.0.0.1` and expose only via reverse proxy + TLS.

## Who can call what

| Client | Auth | Use |
|--------|------|-----|
| **Probes** | none | `GET /healthz`, `GET /readyz` |
| **Meta / CSRF** | none | `GET /api/v1/meta`, `GET /api/v1/csrf` |
| **GUI** | session cookie after `POST /api/v1/session` | Full UI; CSRF required on POST/PUT/DELETE |
| **API client** | `Authorization: Bearer` or `X-API-Token` | Automation; **scopes** apply; no CSRF |
| **WebSocket** | session cookie **or** API token (`Authorization` / `?token=`) | Live raw lines |

### API token scopes (default: `read,command`)

| Scope | Access |
|-------|--------|
| `read` | health, sensors, raw, serial, rate_limit, config GET, backups |
| `command` | `POST /command`, `POST /ha/rediscover` |
| `admin` | config write/restore (tokens/sessions/webhooks still need **GUI session**) |

Env format: `name:TOKEN[:expire[:scopes]]`  
Example: `ha:s3cret:never:read+command,backup:s3cret2:24h:read`

GUI-created tokens default to `read,command`. Names must be **unique** (case-insensitive). Secret is shown **once**.

Optional: `API_TOKEN_PEPPER` mixed into hashes on disk.

## Routes

| Method | Path | Auth |
|--------|------|------|
| GET | `/healthz`, `/readyz` | public |
| GET | `/api/v1/meta`, `/api/v1/csrf` | public |
| POST / DELETE | `/api/v1/session` | GUI password / session |
| GET | `/api/v1/health`, `/sensors`, `/rflink/raw`, `/serial`, `/rate_limit` | session or API `read` |
| GET/WS | `/api/v1/rflink/raw/ws` | session or API token |
| POST | `/api/v1/command`, `/ha/rediscover` | session or API `command` |
| GET/PUT | `/api/v1/config` | session or API `read` / `admin` |
| POST | `/api/v1/config/restore` | session or API `admin` |
| GET/POST/DELETE | `/api/v1/tokens` | **GUI session only** |
| GET/DELETE | `/api/v1/sessions` | **GUI session only** |
| GET/POST/PUT/DELETE | `/api/v1/webhooks` | **GUI session only** |

### RFLink commands

Same path as MQTT: normalize → format check → whitelist → rate-limit → serial.

Valid examples (see [RFLink protocol](https://rflink.nl/protref.php)):

- `10;PING;`
- `10;NewKaku;0cac142;3;ON;`
- `11;20;01;LacrosseV4;ID=0002;TEMP=00cd;` (echo/resend)

Invalid: `44444mmm;`, empty node-only `10;`.

## Runtime config overlay

| Layer | Role |
|-------|------|
| **Env** | Deploy defaults |
| **`data/runtime.json`** | GUI/API overlay — **present keys fully override env** |

- First save of a key materializes it into the file (`first_time_keys` / `warning` in response)
- Backups: last 3 (`runtime.json.bak.1` … `.3`); restore via API/GUI
- Hot-reload ~3s mtime poll
- Dir `0700`, file `0600`, atomic write
- Editable from GUI: friendly names, ignore list, HA discovery/buttons/switches, API tokens, webhooks  
- **HWID map** is env/API-only (intentionally not in GUI)

## Sessions (GUI)

- Cookie: `HttpOnly`, `SameSite=Strict`, `Secure` when TLS
- TTL **72h**, max **16** sessions (oldest evicted)
- Login rate-limit: **5 fails / 5 min** → **15 min** lockout per IP
- Successful login **rotates CSRF**
- List / revoke one / revoke all others from GUI

## Webhooks

Outbound integrations (app initiates the connection). Max **8**.

| Kind | Behaviour |
|------|-----------|
| **poll** | GET/POST URL every `interval_sec` (≥5s); first line of body → `ExecuteCommand` |
| **push** | On events: payload `raw` or `sumjson` |

**POST push** — JSON body.  
**GET push** — substitutes `%payload%` / `%raw%` / `%json%` / `{payload}` (URL-encoded), or auto-appends `?raw=` / `?json=` if no placeholder.

Security:

- HTTPS preferred; **http** for private IPs or lab TLDs (`.local`, `.lan`, `.loc`, …)
- Loopback, link-local, cloud metadata blocked (config + **DNS resolve** at request time)
- Redirects re-validated (max 3)
- Timeouts; poll max response size; push concurrency limit **8** (excess dropped)
- Header **values** never returned by API (names only)
- Unique names; enable/disable from GUI

```bash
export WEBHOOKS_JSON='[{"name":"cmd","enabled":true,"kind":"poll","url":"https://example.com/cmd","method":"GET","interval_sec":60}]'
```

## Web GUI tabs

| Tab | Content |
|-----|---------|
| **Main** | Health, command, sensors, rate-limit, config, sessions, raw (WS) |
| **API tokens** | Create/list/delete machine tokens |
| **Webhooks** | Create/list/enable/disable/delete hooks |

## HTTP env reference

| Variable | Default | Description |
|----------|---------|-------------|
| `HTTP_LISTEN` | _(empty)_ | e.g. `127.0.0.1:8080`; empty disables HTTP |
| `HTTP_AUTH_TOKEN` | | GUI password (≥16 chars recommended) |
| `HTTP_GUI` | `true` | Serve embedded UI at `/` |
| `HTTP_READ_ONLY` | `false` | Reject mutations |
| `HTTP_CORS_ORIGINS` | | Comma-separated Origins (empty = off) |
| `HTTP_TRUSTED_PROXIES` | | CIDRs trusted for `X-Forwarded-For` |
| `HTTP_RAW_BUFFER_SIZE` | `100` | Recent serial lines ring |
| `HTTP_SENSOR_BUFFER_SIZE` | `50` | Recent sensor samples |
| `RUNTIME_CONFIG_FILE` | `data/runtime.json` | Overlay path |
| `API_AUTH_TOKEN` | | `name:TOKEN[:expire[:scopes]],…` |
| `API_TOKEN_PEPPER` | | Optional hash pepper |
| `WEBHOOKS_JSON` | | JSON array of webhooks |

## Middleware / security notes

- Recovery (no panic crash), Request-ID (UUID), RealIP, access log (important routes at info)
- Security headers: nosniff, frame deny, referrer, Permissions-Policy, COOP/CORP, CSP on GUI
- CSRF double-submit **fail-closed** when a GUI session is present
- API Bearer skips CSRF
- Command path never bypasses whitelist/rate-limit
