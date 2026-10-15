#!/bin/sh
# Appends every received SMS (time, sender, text) to a private log file. Example hook for sms.received.
# Copy to ~/.siptooth/hooks/ and configure in ~/.siptooth/config.yaml (needs `sms.enabled: true`):
#   hooks:
#     - id: sms-log
#       events: [sms.received]
#       command: "/home/pi/.siptooth/hooks/sms-log.sh"
# With jq installed the line is "<time> <sender>: <text>" (newlines in the text become spaces);
# without jq the raw JSON payload is appended instead, so nothing is lost.
# siptooth does not log what a hook prints for sms.received, and neither does this script.
set -eu
umask 077
LOG="${SIPTOOTH_SMS_LOG:-$HOME/.siptooth/data/sms.log}"
mkdir -p "$(dirname "$LOG")"
if command -v jq >/dev/null 2>&1; then
	jq -r '"\(.occurred_at) \(.sms.from // "unknown"): \(.sms.text | gsub("[\r\n]+"; " "))"' >> "$LOG"
else
	cat >> "$LOG"
fi
