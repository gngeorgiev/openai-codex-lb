# codexlb

`codexlb` is a Go load-balancing proxy + wrapper for Codex CLI.

It provides:

- multi-account enrollment (`auth.json` per alias)
- per-request account selection (usage-balanced/sticky/round-robin)
- failover/cooldown handling on `429`/`5xx`
- automatic disable on bad auth (`401`/`403`)
- wrapper execution that points Codex to local proxy (`OPENAI_BASE_URL`)
- structured request/switch/quota logs under `~/.codex-lb/logs`
- tunable runtime config via `~/.codex-lb/config.toml`

## Build

```bash
go build ./cmd/codexlb
```

## Quick Start

1) Add accounts:

```bash
./codexlb account login alice
./codexlb account login bob
# or import existing homes:
./codexlb account import --from ~/.agentlb/sessions/alice alice
```

2) Start proxy:

```bash
./codexlb proxy --listen 127.0.0.1:8765 --upstream https://chatgpt.com/backend-api
```

3) Check live proxy/account state:

```bash
./codexlb status
```

4) Run codex through proxy:

```bash
./codexlb run
# or
./codexlb run exec --json "fix this"
```

## Logging

Default root: `~/.codex-lb`

- request/switch/quota events: `~/.codex-lb/logs/proxy.current.jsonl`
- launchd stdout/stderr (if installed via daemon target):
  - `~/.codex-lb/logs/launchd.stdout.log`
  - `~/.codex-lb/logs/launchd.stderr.log`

Example event types:

- `request.received`
- `request.account_selected`
- `request.switched`
- `account.cooldown`
- `account.disabled`
- `quota.refreshed`
- `config.reloaded`
- `config.reload_failed`

## `config.toml` Tuning

`codexlb` automatically creates `~/.codex-lb/config.toml` on first run.

Use it to tune behavior without editing code/state JSON.

Example:

```toml
[proxy]
listen = "127.0.0.1:8765"
upstream_base_url = "https://chatgpt.com/backend-api"
max_attempts = 3
usage_timeout_ms = 15000
cooldown_default_seconds = 5

[policy]
mode = "usage_balanced" # usage_balanced | sticky | round_robin
delta_percent = 10

[policy.weights]
daily = 60
weekly = 40

[quota]
refresh_interval_minutes = 10
refresh_interval_messages = 10
cache_ttl_minutes = 30

[commands]
# Base command used by `codexlb account login` (CLI args are appended).
login = ["login"]
# Prefix inserted before args passed to `codexlb run`.
# Example enables yolo by default:
run = ["exec", "--yolo"]
```

Notes:

- Startup loads settings from `config.toml` into runtime state.
- While `codexlb proxy` is running, it polls `config.toml` and reloads changes automatically (default every 1s).
- `codexlb proxy` CLI flags override settings for that run and persist the new values back to `config.toml`.
- Changing `proxy.listen` in `config.toml` is detected but does not rebind the running server socket; restart proxy to apply that field.

## Account Selection Algorithm

Selection happens per request in this order:

1. Refresh runtime state:
   - expired cooldowns are cleared
   - quota refresh may run (based on `quota.refresh_interval_minutes` and `quota.refresh_interval_messages`)
2. Build `healthy` candidates:
   - account must be `enabled = true`
   - account must not have `disabled_reason`
   - account `cooldown_until_ms` must be in the past
3. Choose account by policy mode.

If no healthy account exists, request fails with `503`.

### Score (`usage_balanced`)

Each account gets a score in `[0,1]`:

- `daily_remaining = clamp((daily_limit - daily_used) / daily_limit, 0..1)`
- `weekly_remaining = clamp((weekly_limit - weekly_used) / weekly_limit, 0..1)`
- if a limit is unknown, remaining defaults to `0.30` for that window
- weighted score:
  - `score = w_daily * daily_remaining + w_weekly * weekly_remaining`
  - weights are normalized from `[policy.weights]`

### Policy behavior

- `usage_balanced`:
  - pick best healthy score
  - hysteresis uses `policy.delta_percent`
  - switch only when `current_score < best_score - delta_percent/100`
  - otherwise stay on current active account
- `sticky`:
  - keep active account while healthy
  - fallback to first healthy account only when current becomes unhealthy
- `round_robin`:
  - rotate to next healthy account each selection
  - fallback to first healthy account if active is not healthy
- pin (internal state support):
  - if `pinned_account_id` is set and healthy, it overrides normal policy

### Failover and account state updates

For each request attempt (up to `proxy.max_attempts`):

- `2xx`: request succeeds; selected account becomes active
- `429` or `5xx`:
  - account enters cooldown
  - cooldown from `Retry-After` when available, else fallback
  - proxy retries with another healthy account
- `401` or `403`:
  - account is disabled (`disabled_reason` set)
  - proxy retries with another account
- transport/proxy errors:
  - account gets default cooldown
  - proxy retries

After exhausting attempts, last upstream response (or `503`) is returned.

## CLI Reference

### `codexlb proxy`

Runs the local load-balancing proxy.

Flags:

- `--root` state directory (default: `~/.codex-lb`)
- `--listen` listen address (example: `127.0.0.1:8765`)
- `--upstream` upstream base URL (example: `https://chatgpt.com/backend-api`)
- `--max-attempts` retry attempts per request
- `--usage-timeout-ms` timeout for usage API refresh calls
- `--cooldown-default-seconds` fallback cooldown when retry hints are missing
- `--quota-refresh-minutes` usage refresh interval by time
- `--quota-refresh-messages` usage refresh interval by successful message count
- `--quota-cache-ttl-minutes` quota cache TTL used by freshness checks

Runtime behavior:

- `config.toml` changes are hot-reloaded while proxy runs.
- `proxy.listen` changes are logged but require restart to take effect.

Examples:

```bash
codexlb proxy
codexlb proxy --listen 127.0.0.1:9000 --upstream https://chatgpt.com/backend-api
codexlb proxy --max-attempts 4 --quota-refresh-minutes 5
```

### `codexlb account login`

Creates/uses `~/.codex-lb/accounts/<alias>` as `CODEX_HOME` and runs `codex login`.

Flags:

- `--root` state directory
- `--codex-bin` codex executable path

`[commands.login]` values are prepended before any extra `<codex-login-args...>`.

Usage:

```bash
codexlb account login [--root DIR] [--codex-bin PATH] <alias> [-- <codex-login-args...>]
```

### `codexlb account import`

Imports `auth.json` from an existing Codex home.

Flags:

- `--root` state directory
- `--from` source `CODEX_HOME` path (required)

Usage:

```bash
codexlb account import [--root DIR] --from <CODEX_HOME> <alias>
```

### `codexlb account list`

Lists enrolled accounts and status.

Flags:

- `--root` state directory

### `codexlb account rm`

Removes an account and its stored account directory.

Flags:

- `--root` state directory

Usage:

```bash
codexlb account rm [--root DIR] <alias>
```

### `codexlb run`

Runs codex with proxy env wiring.

Environment set by wrapper:

- `OPENAI_BASE_URL` -> local proxy URL
- `OPENAI_API_KEY` -> `codex-lb-local-key` if not already set
- `CODEX_HOME` -> wrapper runtime home (`~/.codex-lb/runtime` by default)
- if `CODEX_HOME/auth.json` is missing, wrapper seeds it from an enrolled account to avoid interactive login prompts

Flags:

- `--root` state directory
- `--proxy-url` override proxy URL (default: `http://<listen-from-store>`)
- `--codex-bin` codex executable path
- `--codex-home` runtime `CODEX_HOME` used for wrapped codex process
- `--command` print wrapped command and exit without executing codex

`[commands.run]` values are prepended before CLI args (useful for `--yolo`).

Usage:

```bash
codexlb run [--root DIR] [--proxy-url URL] [--codex-bin PATH] [--codex-home DIR] [--command] [<codex-args...>]
```

### `codexlb status`

Queries `GET /status` from the running proxy and renders a table similar to `agentlb status`.

Table columns include:

- active marker (`*`) and pin marker (`P`)
- alias/id/email
- status (`ready`, `cooldown(...)`, `disabled(...)`)
- daily/weekly left percentages
- computed score
- last switch reason and quota source

Flags:

- `--root` state directory (used to resolve default proxy URL)
- `--proxy-url` explicit proxy URL override
- `--timeout` HTTP timeout for status request (default `3s`)
- `--json` print raw JSON instead of table

Usage:

```bash
codexlb status [--root DIR] [--proxy-url URL] [--timeout 3s] [--json]
```

## Make Targets

```bash
make help
make build
make install
make test
make test-real
make fmt
make run-proxy
make status
make install-daemons
make uninstall-daemons
make install-systemd
make install-launchd
```

Configurable make variables:

- `ROOT` (default `~/.codex-lb`)
- `LISTEN` (default `127.0.0.1:8765`)
- `UPSTREAM` (default `https://chatgpt.com/backend-api`)
- `PREFIX` (default `/usr/local`)
- `BINDIR` (default `$(PREFIX)/bin`)
- `DESTDIR` (default empty; optional staging root for package installs)

Example:

```bash
make run-proxy LISTEN=127.0.0.1:9000
make install PREFIX=$HOME/.local
```

## Daemon Installation

### Linux (`systemd --user`)

```bash
make install-systemd
systemctl --user status codexlb-proxy.service
```

Unit file path:

- `~/.config/systemd/user/codexlb-proxy.service`

### macOS (`launchd`)

```bash
make install-launchd
launchctl print gui/$(id -u)/com.codexlb.proxy
```

Plist path:

- `~/Library/LaunchAgents/com.codexlb.proxy.plist`

Cross-platform auto-select:

```bash
make install-daemons
make uninstall-daemons
```

## Tests

Run standard suite:

```bash
go test ./...
```

Run suite + real Codex override check:

```bash
CODEXLB_RUN_REAL_CODEX_TEST=1 go test ./...
```

## Codex URL Override Finding

Verified in this environment (`codex-cli 0.107.0`):

- Codex honors `OPENAI_BASE_URL`
- traffic is sent to `<OPENAI_BASE_URL>/responses` (WebSocket first, then HTTPS fallback)

Real check test:

```bash
CODEXLB_RUN_REAL_CODEX_TEST=1 go test ./internal/lb -run TestRealCodexUsesOPENAI_BASE_URL -v
```

## CLI Help

For built-in per-command flag docs:

```bash
codexlb --help
codexlb proxy --help
codexlb account login --help
codexlb run --help
```
