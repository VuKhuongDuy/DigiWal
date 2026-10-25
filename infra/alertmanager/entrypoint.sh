#!/bin/sh
# Renders Alertmanager's configuration from the environment, then runs it. Alertmanager does not expand variables in its
# config file, and the bot token must not live in the repository.
#   TELEGRAM_BOT_TOKEN, TELEGRAM_CHAT_ID  set: alerts go to that Telegram chat
#                                          unset: alerts are only held (see them on the Alertmanager page)
set -eu
cfg=/tmp/alertmanager.yml
{
	cat <<'YAML'
route:
  receiver: ops
  group_by: [alertname, service]
  group_wait: 30s      # let alerts that fire together arrive as one message
  group_interval: 5m
  repeat_interval: 4h  # still firing: remind every 4 hours
receivers:
YAML
	if [ -n "${TELEGRAM_BOT_TOKEN:-}" ] && [ -n "${TELEGRAM_CHAT_ID:-}" ]; then
		printf %s "$TELEGRAM_BOT_TOKEN" > /tmp/telegram_token
		cat <<YAML
  - name: ops
    telegram_configs:
      - bot_token_file: /tmp/telegram_token
        chat_id: $TELEGRAM_CHAT_ID
        send_resolved: true
        message: |-
          {{ range .Alerts }}{{ if eq .Status "firing" }}🔴 {{ .Labels.severity }}{{ else }}✅ resolved{{ end }}: {{ .Annotations.summary }}
          {{ .Annotations.description }}
          {{ end }}
YAML
	else
		echo "  - name: ops"
		echo "alertmanager: no TELEGRAM_BOT_TOKEN / TELEGRAM_CHAT_ID, alerts are held and not sent anywhere" >&2
	fi
} > "$cfg"
exec /bin/alertmanager --config.file="$cfg" --storage.path=/alertmanager "$@"
