#!/usr/bin/env bash
set -euo pipefail

TARGET="auto"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --target)
      TARGET="$2"
      shift 2
      ;;
    -h|--help)
      cat <<USAGE
Usage: uninstall-daemon.sh [--target <auto|systemd|launchd>]
USAGE
      exit 0
      ;;
    *)
      echo "unknown arg: $1" >&2
      exit 2
      ;;
  esac
done

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
  UNIT_PATH="${HOME}/.config/systemd/user/codexlb-proxy.service"
  systemctl --user disable --now codexlb-proxy.service >/dev/null 2>&1 || true
  systemctl --user daemon-reload >/dev/null 2>&1 || true
  rm -f "$UNIT_PATH"
  echo "removed systemd user service: ${UNIT_PATH}"
  exit 0
fi

if [[ "$TARGET" == "launchd" ]]; then
  PLIST_PATH="${HOME}/Library/LaunchAgents/com.codexlb.proxy.plist"
  launchctl bootout "gui/${UID}" "$PLIST_PATH" >/dev/null 2>&1 || true
  rm -f "$PLIST_PATH"
  echo "removed launchd agent: ${PLIST_PATH}"
  exit 0
fi

echo "unknown target: $TARGET" >&2
exit 1
