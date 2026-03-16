# codexlb

`codexlb` is a Go load-balancing proxy and wrapper for Codex CLI.

It lets you run Codex through a local proxy that can switch between multiple authenticated accounts, handle failover, and expose live status.

## Features

- Multi-account enrollment (`auth.json` per alias)
- Dockerized browser login flow for importing host Codex credentials
- Per-request account selection (`usage_balanced`, `sticky`, `round_robin`)
- Automatic failover/cooldown on `429` and `5xx`
- Automatic disable on auth errors (`401`, `403`)
- Wrapper execution via local proxy (`OPENAI_BASE_URL`)
- Structured logs under `~/.codex-lb/logs`
- Tunable runtime config in `~/.codex-lb/config.toml`
- Config hot reload while proxy is running

## Install

Build locally:

```bash
go build ./cmd/codexlb
```

With Make:

```bash
make build
make install
```

User-local install example:

```bash
make install PREFIX=$HOME/.local
```

## Quick Start

1. Add accounts:

```bash
./codexlb account login alice
./codexlb account login bob

# Or import an existing Codex home:
./codexlb account import --from ~/.agentlb/sessions/alice alice

# Or bootstrap a host CODEX_HOME through a Dockerized Chrome login:
printf '%s\n' "$CHATGPT_PASSWORD" | ./codexlb login-with work --username you@example.com --password-stdin --docker-network vpn_net
```

2. Start proxy:

```bash
./codexlb proxy --listen 127.0.0.1:8765 --upstream https://chatgpt.com/backend-api
```

3. Check state:

```bash
./codexlb status
./codexlb status --short

# Pin the proxy to a specific account while it remains healthy:
./codexlb account pin alice
```

4. Run Codex through proxy:

```bash
./codexlb run
./codexlb run exec --json "fix this"
```

## Docker

Build and run with Compose:

```bash
docker compose up -d --build
```

Stop:

```bash
docker compose down
```

Defaults:

- Proxy listens on `0.0.0.0:8765` in container and is published as `127.0.0.1:8765` on host.
- Host `~/.codex-lb` is bind-mounted to `/data` in container.
- Container runs `codexlb proxy --root /data`.

Mounted data under `/data` includes:

- `store.json`
- `config.toml`
- `accounts/`
- `runtime/`
- `logs/`

Optional environment overrides:

- `CODEXLB_ROOT_DIR` (host path to mount instead of `~/.codex-lb`)
- `CODEXLB_UPSTREAM`
- `CODEXLB_BIND_HOST`
- `CODEXLB_PORT`
- `UID` / `GID` (container runtime user)

CLI environment overrides:

- `CODEXLB_ROOT` sets the default `--root` for commands that operate on the local store.
- `CODEXLB_PROXY_URL` sets the default `--proxy-url` for commands that talk to a proxy or remote admin API.

## Paths

Default root is `~/.codex-lb`.

| Path | Purpose |
|---|---|
| `~/.codex-lb/store.json` | Runtime state (accounts, quotas, active account) |
| `~/.codex-lb/config.toml` | Tunable settings |
| `~/.codex-lb/accounts/<alias>/auth.json` | Per-account auth |
| `~/.codex-lb/runtime/` | Wrapper runtime `CODEX_HOME` |
| `~/.codex-lb/logs/proxy.current.jsonl` | Proxy event log |
| `~/.codex-lb/logs/launchd.stdout.log` | launchd stdout (if installed) |
| `~/.codex-lb/logs/launchd.stderr.log` | launchd stderr (if installed) |

## Configuration (`config.toml`)

`codexlb` creates `~/.codex-lb/config.toml` on first run.
This file is the source of truth for settings.

```toml
[proxy]
listen = "127.0.0.1:8765"
upstream_base_url = "https://chatgpt.com/backend-api"
# Global default URL used by commands that talk to the proxy
# (`run`, `status`, and `proxy logs`) when --proxy-url is not provided.
# If empty, falls back to "http://<proxy.listen>".
proxy_url = ""
max_attempts = 3
usage_timeout_ms = 30000
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
# Base command for `codexlb account login`.
login = ["login"]

# Prefix prepended to args passed to `codexlb run`.
# Example: run Codex in yolo mode by default.
run = ["exec", "--yolo"]

[run]
# Run codex via the current shell (`$SHELL -lc ...`).
inherit_shell = true
```

Hot reload behavior:

- Proxy polls `config.toml` and reloads updates automatically (default: every 1s)
- `proxy.listen` changes are detected but require proxy restart to take effect
- CLI flags on `codexlb proxy` override settings for that process only (not persisted)

## Account Selection Algorithm

Selection happens per request:

1. Refresh runtime state (expire cooldowns, maybe refresh quotas).
2. Build healthy account set:
   - `enabled = true`
   - no `disabled_reason`
   - `cooldown_until_ms` in the past
3. Select by policy.

If no healthy account exists, proxy returns `503`.

### Score (`usage_balanced`)

For each account:

- `daily_remaining = clamp((daily_limit - daily_used) / daily_limit, 0..1)`
- `weekly_remaining = clamp((weekly_limit - weekly_used) / weekly_limit, 0..1)`
- Unknown window defaults to `0.30`
- Score uses normalized `[policy.weights]`

### Policy Modes

- `usage_balanced`: choose highest score with hysteresis `delta_percent`
- `sticky`: keep active while healthy, else fallback to first healthy
- `round_robin`: rotate through healthy accounts
- Pinned account (if set and healthy) overrides normal policy

### Failover

Per request attempt (up to `proxy.max_attempts`):

- `2xx`: success
- `429`/`5xx`: cooldown, then retry with another healthy account
- `401`/`403`: disable account, retry with another account
- transport error: default cooldown, then retry

After attempts are exhausted, proxy returns last upstream response (or `503`).

## CLI Reference

### `codexlb proxy`

Run local LB proxy.

Usage:

```bash
codexlb proxy [flags]
```

To fetch logs from a running proxy instance:

```bash
codexlb proxy logs [flags]
```

Key flags:

| Flag | Description |
|---|---|
| `--root` | State directory (default `~/.codex-lb`) |
| `--listen` | Listen address (example `127.0.0.1:8765`) |
| `--upstream` | Upstream base URL |
| `--max-attempts` | Retry attempts per request |
| `--usage-timeout-ms` | Usage API timeout |
| `--cooldown-default-seconds` | Fallback cooldown |
| `--quota-refresh-minutes` | Time-based quota refresh interval |
| `--quota-refresh-messages` | Message-count quota refresh interval |
| `--quota-cache-ttl-minutes` | Quota cache TTL |

Examples:

```bash
codexlb proxy
codexlb proxy --listen 127.0.0.1:9000 --upstream https://chatgpt.com/backend-api
CODEXLB_ROOT=/tmp/codexlb codexlb proxy
```

### `codexlb proxy logs`

Fetch or tail proxy event logs over HTTP from a running proxy.

Usage:

```bash
codexlb proxy logs [--root DIR] [--proxy-url URL] [--tail 100] [--offset N] [--limit 500] [--follow] [--interval 2s]
```

Notes:

- Use `--proxy-url` for remote instances.
- `--follow` polls `/logs` with byte offsets and prints only new lines.
- `CODEXLB_ROOT` and `CODEXLB_PROXY_URL` provide the default values for `--root` and `--proxy-url`.

### `codexlb account login`

Create/use `~/.codex-lb/accounts/<alias>` as `CODEX_HOME` and execute login command.

Usage:

```bash
codexlb account login [--root DIR] [--proxy-url URL] [--codex-bin PATH] <alias> [-- <extra-login-args...>]
```

Notes:

- `commands.login` is prepended before extra args.
- With `--proxy-url`, runs login locally and uploads the resulting account data to the remote proxy.

### `codexlb account import`

Import an existing Codex home auth.

Usage:

```bash
codexlb account import [--root DIR] [--proxy-url URL] --from <CODEX_HOME> <alias>
```

Notes:

- With `--proxy-url`, the local `auth.json` and optional `config.toml` are uploaded to the proxy.

### `codexlb account list`

List enrolled accounts and health/state summary.

Usage:

```bash
codexlb account list [--root DIR] [--proxy-url URL]
```

### `codexlb account rm`

Remove account and its stored account directory.

Usage:

```bash
codexlb account rm [--root DIR] [--proxy-url URL] <alias>
```

### `codexlb account pin`

Pin selection to a specific account alias.

Usage:

```bash
codexlb account pin [--root DIR] [--proxy-url URL] <alias>
```

### `codexlb account unpin`

Clear pinned account selection.

Usage:

```bash
codexlb account unpin [--root DIR] [--proxy-url URL]
```

### `codexlb login-with`

Run `codex login` inside a published Docker image, complete the OpenAI login flow in Chromium, and import the resulting credentials back into the host `CODEX_HOME` and a named codexlb account alias.

Usage:

```bash
codexlb login-with [--root DIR] <alias> --username <email> (--password <password> | --password-stdin) [--codex-home DIR] [--docker-network NAME] [--docker-image TAG] [--timeout 10m]
```

Notes:

- The first positional argument is the codexlb account alias that will receive the imported auth under `~/.codex-lb/accounts/<alias>` (or the chosen `--root`).
- The container is attached to the Docker network selected by `--docker-network`, so you can point it at a VPN-enabled network namespace when needed.
- Credentials are also written back into host `CODEX_HOME` (`$CODEX_HOME` when set, otherwise `~/.codex`).
- By default the command uses the published image `ghcr.io/gngeorgiev/agent-lb-proxy-login:latest`; `--docker-image` overrides it.
- `--password-stdin` avoids leaking the password into shell history.
- The automation is designed for username/password sign-in. If OpenAI prompts for CAPTCHA, MFA, or another interactive checkpoint, the containerized flow may still require manual handling.

### CLI Env Vars

For local CLI overrides without repeating flags:

```bash
export CODEXLB_ROOT=/path/to/state
export CODEXLB_PROXY_URL=http://127.0.0.1:9000
```

These act as defaults for `--root` and `--proxy-url`. An explicit flag still wins over the environment.

### Proxy Admin API

When `--proxy-url` is used for account commands, `codexlb` calls these proxy endpoints:

- `GET /admin/accounts`
- `POST /admin/account/import`
- `POST /admin/account/rm`
- `POST /admin/account/pin`
- `POST /admin/account/unpin`
- `GET /admin/runtime-auth`

Security note:

- Admin API is currently unauthenticated; expose it only on trusted networks (or behind your own auth/TLS layer).
- `GET /admin/runtime-auth` returns the selected account's raw `auth.json` payload for runtime bootstrapping; treat it as highly sensitive credential material.

### `codexlb run`

Run Codex with proxy env wiring.

Usage:

```bash
codexlb run [--root DIR] [--proxy-url URL] [--codex-bin PATH] [--codex-home DIR] [--command] [<codex-args...>]
```

Flags:

| Flag | Description |
|---|---|
| `--root` | State directory |
| `--proxy-url` | Override proxy URL (default `proxy.proxy_url`, else `http://<listen-from-store>`) |
| `--codex-bin` | Codex executable path |
| `--codex-home` | Wrapper runtime `CODEX_HOME` |
| `--command` | Print wrapped command and exit (do not execute) |

Runtime env behavior:

- Sets `OPENAI_BASE_URL` to proxy URL
- Sets `OPENAI_API_KEY=codex-lb-local-key` if missing
- Uses `CODEX_HOME` from `--codex-home` or `~/.codex-lb/runtime`
- Runs through `$SHELL -lc` when `run.inherit_shell = true` (default)
- If runtime `auth.json` is missing/invalid, seeds from an enrolled account when available
- If no local accounts are enrolled, attempts to fetch runtime auth from remote proxy `GET /admin/runtime-auth`
- If remote auth is unavailable, writes a local proxy-only stub auth (includes both `access_token` and `id_token`)
- Prepends `commands.run` to passed args

Examples:

```bash
codexlb run
codexlb run --command exec --json "ping"
codexlb run -- --json "ping" # pass flags that start with '-'
```

### `codexlb status`

Query `GET /status` from proxy.

Default output renders a table with active/pin markers, account identity, health/cooldown state, quota percentages, score, and last-switch metadata.

Usage:

```bash
codexlb status [--root DIR] [--proxy-url URL] [--timeout 3s] [--short | --json]
```

Flags:

| Flag | Description |
|---|---|
| `--root` | State directory (for default proxy URL resolution) |
| `--proxy-url` | Explicit proxy URL (default `proxy.proxy_url`, else `http://<listen-from-store>`) |
| `--timeout` | HTTP timeout (default `3s`) |
| `--short` | One-line output for status bars |
| `--json` | Raw JSON output |

`--short` format:

```text
lb=<alias> reason=<switch-reason> mode=<policy-mode>
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

Configurable variables:

- `ROOT` (default `~/.codex-lb`)
- `BINARY_WORKDIR` (directory containing the daemon binary, default current repo dir)
- `LISTEN` (default `127.0.0.1:8765`)
- `UPSTREAM` (default `https://chatgpt.com/backend-api`)
- `PREFIX` (default `/usr/local`)
- `BINDIR` (default `$(PREFIX)/bin`)
- `DESTDIR` (optional staging root)

## Daemon Installation

### Linux (`systemd --user`)

```bash
make install-systemd
systemctl --user status codexlb-proxy.service
```

Use a binary from a separate workdir:

```bash
make install-systemd BINARY_WORKDIR=/path/to/workdir
```

The installed daemon runs `codexlb proxy --root <ROOT>` and reads listen/upstream from
`config.toml`, so service restarts do not overwrite runtime config values.

Unit path: `~/.config/systemd/user/codexlb-proxy.service`

### macOS (`launchd`)

```bash
make install-launchd
launchctl print gui/$(id -u)/com.codexlb.proxy
```

Plist path: `~/Library/LaunchAgents/com.codexlb.proxy.plist`

### Auto-detect target OS

```bash
make install-daemons
make uninstall-daemons
```

## Logging

Event examples in `~/.codex-lb/logs/proxy.current.jsonl`:

- `request.received`
- `request.account_selected`
- `request.switched`
- `account.cooldown`
- `account.disabled`
- `quota.refreshed`
- `config.reloaded`
- `config.reload_failed`

## Tests

Standard suite:

```bash
go test ./...
```

With real Codex override check:

```bash
CODEXLB_RUN_REAL_CODEX_TEST=1 go test ./...
```

## Codex URL Override Finding

Verified in this environment (`codex-cli 0.107.0`):

- Codex honors `OPENAI_BASE_URL`
- Traffic is sent to `<OPENAI_BASE_URL>/responses` (WebSocket first, HTTPS fallback)

Real check command:

```bash
CODEXLB_RUN_REAL_CODEX_TEST=1 go test ./internal/lb -run TestRealCodexUsesOPENAI_BASE_URL -v
```

## CLI Help

```bash
codexlb --help
codexlb proxy --help
codexlb account login --help
codexlb account pin --help
codexlb run --help
codexlb status --help
```
