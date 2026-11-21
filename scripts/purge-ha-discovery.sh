#!/bin/sh
# Clear one retained Home Assistant discovery topic that this process no longer
# remembers. Topics go-rflink itself published are removed with
# POST /api/v1/ha/purge (command scope).
#
# Usage:
#   PUBLISH_HOST=localhost:1883 ./scripts/purge-ha-discovery.sh \
#     homeassistant/sensor/gorflink_oregon_0001_temp/config
set -eu
topic="${1:-}"
if [ -z "$topic" ]; then
  echo "usage: $0 <discovery topic>" >&2
  exit 2
fi
host="${PUBLISH_HOST:-localhost:1883}"
mqtt_host="${host%%:*}"
mqtt_port="${host##*:}"
if [ "$mqtt_port" = "$host" ]; then
  mqtt_port=1883
fi
exec mosquitto_pub \
  -h "$mqtt_host" \
  -p "$mqtt_port" \
  -u "${PUBLISH_MQTT_USERNAME:-}" \
  -P "${PUBLISH_MQTT_PASSWORD:-}" \
  -t "$topic" \
  -r \
  -n
