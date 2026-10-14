#!/bin/sh
# Appends every event payload to a log file. Harmless example hook.
# Copy to ~/.siptooth/hooks/ and configure in ~/.siptooth/config.yaml:
#   hooks:
#     - id: log
#       events: [call.ringing, call.connected, call.ended, system.error]
#       command: "/home/pi/.siptooth/hooks/write-file.sh"
set -eu
LOG="${SIPTOOTH_HOOK_LOG:-$HOME/.siptooth/data/hook-events.jsonl}"
mkdir -p "$(dirname "$LOG")"
cat >> "$LOG"
echo "appended ${SIPTOOTH_EVENT:-unknown} for call ${SIPTOOTH_CALL_ID:-none} to $LOG"
