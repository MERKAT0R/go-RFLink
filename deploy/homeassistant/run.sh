#!/bin/sh
# Home Assistant add-on entry. Maps /data/options.json onto go-rflink's env.
set -eu
config=/data/options.json
if [ -f "$config" ]; then
  export SERIAL_DEVICE="$(jq -r '.serial_device // "/dev/ttyUSB0"' "$config")"
  export SERIAL_DEVICES="$(jq -r '.serial_devices // ""' "$config")"
  export SERIAL_COMMAND_DEVICES="$(jq -r '.serial_command_devices // ""' "$config")"
  host="$(jq -r '.mqtt_host // "core-mosquitto"' "$config")"
  port="$(jq -r '.mqtt_port // 1883' "$config")"
  case "$host" in
    \[*) ;;
    *:*) host="[$host]" ;;
  esac
  export PUBLISH_HOST="${host}:${port}"
  export PUBLISH_MQTT_USERNAME="$(jq -r '.mqtt_user // "username"' "$config")"
  export PUBLISH_MQTT_PASSWORD="$(jq -r '.mqtt_password // "password"' "$config")"
  export PUBLISH_SCHEME="$(jq -r '.mqtt_scheme // "tcp"' "$config")"
  export HTTP_LISTEN="$(jq -r '.http_listen // "0.0.0.0:8099"' "$config")"
  export HTTP_AUTH_TOKEN="$(jq -r '.http_auth_token // ""' "$config")"
  export PUBLISH_HOME_ASSISTANT_DISCOVERY="$(jq -r 'if has("ha_discovery") then (.ha_discovery | tostring) else "true" end' "$config")"
fi
exec /app/go-rflink
