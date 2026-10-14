#!/bin/sh
# POSTs the payload as JSON to a webhook URL taken from an env file.
# Copy http-post.env.example to ~/.siptooth/hooks/http-post.env, set the URL, then
# chmod 0600 it.
set -eu
ENV_FILE="${SIPTOOTH_HOOK_ENV:-$HOME/.siptooth/hooks/http-post.env}"
if [ -r "$ENV_FILE" ]; then
  . "$ENV_FILE"
fi
: "${SIPTOOTH_WEBHOOK_URL:?set SIPTOOTH_WEBHOOK_URL in $ENV_FILE}"
curl --silent --show-error --fail --max-time 8 \
  -H "Content-Type: application/json" \
  -H "X-Siptooth-Event: ${SIPTOOTH_EVENT:-unknown}" \
  --data-binary @- "$SIPTOOTH_WEBHOOK_URL"
