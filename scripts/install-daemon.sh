#!/usr/bin/env bash
set -euo pipefail

TARGET="auto"
BINARY=""
ROOT_DIR="${HOME}/.codex-lb"
LISTEN="127.0.0.1:8765"
UPSTREAM="https://chatgpt.com/backend-api"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --target)
      TARGET="$2"
      shift 2
      ;;
    --binary)
      BINARY="$2"
      shift 2
      ;;
    --root)
      ROOT_DIR="$2"
      shift 2
      ;;
    --listen)
      LISTEN="$2"
      shift 2
      ;;
    --upstream)
      UPSTREAM="$2"
      shift 2
      ;;
    -h|--help)
      cat <<USAGE
Usage: install-daemon.sh [options]

Options:
  --target <auto|systemd|launchd>
  --binary <path-to-codexlb>
  --root <state-dir>
  --listen <addr>
  --upstream <url>
USAGE
      exit 0
      ;;
    *)
      echo "unknown arg: $1" >&2
      exit 2
      ;;
  esac
done

if [[ -z "$BINARY" ]]; then
  BINARY="$(pwd)/codexlb"
fi

if [[ ! -x "$BINARY" ]]; then
  echo "binary not found/executable: $BINARY" >&2
  exit 1
fi

if [[ "$TARGET" == "auto" ]]; then
  case "$(uname -s)" in
    Linux) TARGET="systemd" ;;
    Darwin) TARGET="launchd" ;;
    *)
      echo "unsupported OS for auto target: $(uname -s)" >&2
      exit 1
      ;;
  esac
fi

if [[ "$TARGET" == "systemd" ]]; then
  UNIT_DIR="${HOME}/.config/systemd/user"
  UNIT_PATH="${UNIT_DIR}/codexlb-proxy.service"
  mkdir -p "$UNIT_DIR"
  cat > "$UNIT_PATH" <<EOF_UNIT
[Unit]
Description=codexlb proxy
After=network-online.target

[Service]
Type=simple
ExecStart=${BINARY} proxy --root ${ROOT_DIR} --listen ${LISTEN} --upstream ${UPSTREAM}
Restart=always
RestartSec=2
Environment=HOME=${HOME}

[Install]
WantedBy=default.target
EOF_UNIT

  systemctl --user daemon-reload
  systemctl --user enable --now codexlb-proxy.service
  echo "installed systemd user service: ${UNIT_PATH}"
  echo "status: systemctl --user status codexlb-proxy.service"
  exit 0
fi

if [[ "$TARGET" == "launchd" ]]; then
  AGENT_DIR="${HOME}/Library/LaunchAgents"
  PLIST_PATH="${AGENT_DIR}/com.codexlb.proxy.plist"
  mkdir -p "$AGENT_DIR"
  cat > "$PLIST_PATH" <<EOF_PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.codexlb.proxy</string>
  <key>ProgramArguments</key>
  <array>
    <string>${BINARY}</string>
    <string>proxy</string>
    <string>--root</string>
    <string>${ROOT_DIR}</string>
    <string>--listen</string>
    <string>${LISTEN}</string>
    <string>--upstream</string>
    <string>${UPSTREAM}</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>StandardOutPath</key>
  <string>${ROOT_DIR}/logs/launchd.stdout.log</string>
  <key>StandardErrorPath</key>
  <string>${ROOT_DIR}/logs/launchd.stderr.log</string>
  <key>EnvironmentVariables</key>
  <dict>
    <key>HOME</key>
    <string>${HOME}</string>
  </dict>
</dict>
</plist>
EOF_PLIST

  mkdir -p "${ROOT_DIR}/logs"

  launchctl bootout "gui/${UID}" "$PLIST_PATH" >/dev/null 2>&1 || true
  launchctl bootstrap "gui/${UID}" "$PLIST_PATH"
  launchctl enable "gui/${UID}/com.codexlb.proxy"
  launchctl kickstart -k "gui/${UID}/com.codexlb.proxy"

  echo "installed launchd agent: ${PLIST_PATH}"
  echo "status: launchctl print gui/${UID}/com.codexlb.proxy"
  exit 0
fi

echo "unknown target: $TARGET" >&2
exit 1
