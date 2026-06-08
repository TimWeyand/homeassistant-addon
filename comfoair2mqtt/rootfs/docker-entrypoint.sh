#!/usr/bin/env bashio

bashio::log.info "Comfoair2MQTT Addon"
bashio::log.info "Preparing to start ... "

# Check if HA supervisor started
bashio::config.require 'serial'

bashio::log.info "Generating Configuration 'config.ini' from HomeAssistant Addon Settings"

# Expose addon configuration through environment variables.
function export_config() {
    local key=${1}
    local subkey

    if bashio::config.is_empty "${key}"; then
        return
    fi

    for subkey in $(bashio::jq "$(bashio::config "${key}")" 'keys[]'); do
        export "COMFOAIR2MQTT_CONFIG_$(bashio::string.upper "${key}")_$(bashio::string.upper "${subkey}")=$(bashio::config "${key}.${subkey}")"
    done
}

export_config 'mqtt'

if bashio::config.is_empty 'mqtt' && bashio::var.has_value "$(bashio::services 'mqtt')"; then
    export COMFOAIR2MQTT_CONFIG_MQTT_HOST=$(bashio::services 'mqtt' 'host')
    export COMFOAIR2MQTT_CONFIG_MQTT_PORT=$(bashio::services 'mqtt' 'port')
    export COMFOAIR2MQTT_CONFIG_MQTT_USER="$(bashio::services 'mqtt' 'username')"
    export COMFOAIR2MQTT_CONFIG_MQTT_PASSWORD="$(bashio::services 'mqtt' 'password')"
    export COMFOAIR2MQTT_CONFIG_MQTT_KEEPALIVE="45"
fi

if bashio::var.true $(bashio::config 'RS485_protocol'); then
  export COMFOAIR2MQTT_CONFIG_RS485="True";
else
  export COMFOAIR2MQTT_CONFIG_RS485="False";
fi

if bashio::var.true $(bashio::config 'enablePcMode'); then
  export COMFOAIR2MQTT_CONFIG_PCMode="True";
else
  export COMFOAIR2MQTT_CONFIG_PCMode="False";
fi

if bashio::var.true $(bashio::config 'debug'); then
  export COMFOAIR2MQTT_CONFIG_debug="True";
else
  export COMFOAIR2MQTT_CONFIG_debug="False";
fi

if bashio::var.true $(bashio::config 'HAEnableAutoDiscoverySensors'); then
  export COMFOAIR2MQTT_CONFIG_DiscoverySensor="True";
else
  export COMFOAIR2MQTT_CONFIG_DiscoverySensor="False";
fi

if bashio::var.true $(bashio::config 'HAEnableAutoDiscoveryClimate'); then
  export COMFOAIR2MQTT_CONFIG_DiscoveryClimate="True";
else
  export COMFOAIR2MQTT_CONFIG_DiscoveryClimate="False";
fi

# Fan levels -> [DEVICE] section. Upstream ca350.py reads config['DEVICE'][...] at
# module load time; without this section the script crashes immediately with
# KeyError: 'DEVICE' (this is exactly what broke control after the upstream pulled
# in the fan-level feature). Each value falls back to the upstream default if unset.
fan_cfg() {
  local val
  val=$(bashio::config "fan_levels.${1}")
  if [ -z "${val}" ] || [ "${val}" = "null" ]; then
    echo "${2}"
  else
    echo "${val}"
  fi
}
COMFOAIR2MQTT_FAN_OUT_ABSENT=$(fan_cfg 'out_absent' 15)
COMFOAIR2MQTT_FAN_OUT_LOW=$(fan_cfg 'out_low' 25)
COMFOAIR2MQTT_FAN_OUT_MID=$(fan_cfg 'out_mid' 40)
COMFOAIR2MQTT_FAN_OUT_HIGH=$(fan_cfg 'out_high' 70)
COMFOAIR2MQTT_FAN_IN_ABSENT=$(fan_cfg 'in_absent' 15)
COMFOAIR2MQTT_FAN_IN_LOW=$(fan_cfg 'in_low' 25)
COMFOAIR2MQTT_FAN_IN_MID=$(fan_cfg 'in_mid' 40)
COMFOAIR2MQTT_FAN_IN_HIGH=$(fan_cfg 'in_high' 70)

if bashio::var.true $(bashio::config 'setup_fan_levels_at_start'); then
  COMFOAIR2MQTT_SETUP_FAN_LEVELS_AT_START="True";
else
  COMFOAIR2MQTT_SETUP_FAN_LEVELS_AT_START="False";
fi

# MQTT broker defaults for the manual `mqtt:` override path. `export_config 'mqtt'`
# only exports the sub-keys the user actually set, and the service-discovery
# fallback above runs ONLY when `mqtt` is left empty. So a user who points the
# add-on at an external broker but omits port/keepalive (common — most only set
# host/user/password) leaves these unset. Upstream ca350.py runs int(MQTTPort)/
# int(MQTTKeepalive) at import time, so an empty value becomes int('') ->
# ValueError -> immediate crash (masked as a restart loop by the supervisor
# below) — the same failure class as the original KeyError: 'DEVICE'. Fill the
# same defaults the discovery path uses. (`:=` triggers on unset OR empty and
# `:` is errexit-safe.)
: "${COMFOAIR2MQTT_CONFIG_MQTT_PORT:=1883}"
: "${COMFOAIR2MQTT_CONFIG_MQTT_KEEPALIVE:=45}"
if ! bashio::config.is_empty 'mqtt' && [ -z "${COMFOAIR2MQTT_CONFIG_MQTT_HOST}" ]; then
  bashio::log.warning "Manual 'mqtt' config has no host set — MQTT connection will fail. Set mqtt.host or clear the mqtt option to use HA's MQTT service."
fi

cat <<EOF > "/opt/hacomfoairmqtt/src/config.ini"
[DEFAULT]
SerialPort=$(bashio::config 'serial')
RS485_protocol=$COMFOAIR2MQTT_CONFIG_RS485
refresh_interval=$(bashio::config 'refresh_interval')
enablePcMode=$COMFOAIR2MQTT_CONFIG_PCMode
debug=$COMFOAIR2MQTT_CONFIG_debug

[DEVICE]
FanOutAbsent=$COMFOAIR2MQTT_FAN_OUT_ABSENT
FanOutLow=$COMFOAIR2MQTT_FAN_OUT_LOW
FanOutMid=$COMFOAIR2MQTT_FAN_OUT_MID
FanOutHigh=$COMFOAIR2MQTT_FAN_OUT_HIGH
FanInAbsent=$COMFOAIR2MQTT_FAN_IN_ABSENT
FanInLow=$COMFOAIR2MQTT_FAN_IN_LOW
FanInMid=$COMFOAIR2MQTT_FAN_IN_MID
FanInHigh=$COMFOAIR2MQTT_FAN_IN_HIGH
SetUpFanLevelsAtStart=$COMFOAIR2MQTT_SETUP_FAN_LEVELS_AT_START

[MQTT]
MQTTServer=$COMFOAIR2MQTT_CONFIG_MQTT_HOST
MQTTPort=$COMFOAIR2MQTT_CONFIG_MQTT_PORT
MQTTUser=$COMFOAIR2MQTT_CONFIG_MQTT_USER
MQTTPassword=$COMFOAIR2MQTT_CONFIG_MQTT_PASSWORD
MQTTKeepalive=$COMFOAIR2MQTT_CONFIG_MQTT_KEEPALIVE

[HA]
HAEnableAutoDiscoverySensors=$COMFOAIR2MQTT_CONFIG_DiscoverySensor
HAEnableAutoDiscoveryClimate=$COMFOAIR2MQTT_CONFIG_DiscoveryClimate
HAAutoDiscoveryDeviceId=$(bashio::config 'HAAutoDiscoveryDeviceId')
HAAutoDiscoveryDeviceName=$(bashio::config 'HAAutoDiscoveryDeviceName')
HAAutoDiscoveryDeviceManufacturer=$(bashio::config 'HAAutoDiscoveryDeviceManufacturer')
HAAutoDiscoveryDeviceModel=$(bashio::config 'HAAutoDiscoveryDeviceModel')
EOF

# With debug enabled, echo the generated config.ini so a "settings not applied"
# problem (empty/missing values like the MQTT or DEVICE crashes) is visible
# straight from the add-on log without shelling into the container. The MQTT
# password is masked so credentials never land in the log.
if bashio::var.true "$(bashio::config 'debug')"; then
  bashio::log.info "Generated config.ini:"
  while IFS= read -r line; do
    case "${line}" in
      MQTTPassword=?*) bashio::log.info "MQTTPassword=***" ;;
      *) bashio::log.info "${line}" ;;
    esac
  done < "/opt/hacomfoairmqtt/src/config.ini"
fi

bashio::log.info "Startup hacomfoairmqtt"

# --- Self-healing supervisor -------------------------------------------------
# ca350.py is the upstream script (left untouched). If it ever exits — e.g. an
# MQTT failure or an unhandled serial exception (upstream only catches
# KeyboardInterrupt) terminates it — restart it automatically so the add-on
# recovers without a manual restart. A clean stop from Home Assistant (SIGTERM)
# breaks the loop instead of restarting.
#
# The restart delay uses exponential backoff (10s -> 300s) so a permanently
# missing serial device does NOT cause a 10s restart storm. A process that ran
# healthily for at least HEALTHY_RUNTIME seconds resets the backoff, so ordinary
# crashes still recover quickly.
RESTART_DELAY_MIN=10
RESTART_DELAY_MAX=300
HEALTHY_RUNTIME=60
delay=${RESTART_DELAY_MIN}
running=true
child_pid=""

term_handler() {
    running=false
    if [ -n "${child_pid}" ]; then
        kill -TERM "${child_pid}" 2>/dev/null || true
        wait "${child_pid}" 2>/dev/null || true
    fi
    exit 0
}
trap term_handler SIGTERM SIGINT

while [ "${running}" = true ]; do
    started=${SECONDS}
    python3 /opt/hacomfoairmqtt/src/ca350.py &
    child_pid=$!
    # bashio runs with `set -o errexit`; a bare `wait` on a non-zero child would
    # abort this script (the loop would never iterate, no backoff, container would
    # full-restart). `|| rc=$?` keeps the exit code without tripping errexit.
    rc=0
    wait "${child_pid}" || rc=$?
    if [ "${running}" != true ]; then
        break
    fi
    ran=$(( SECONDS - started ))
    if [ "${ran}" -ge "${HEALTHY_RUNTIME}" ]; then
        delay=${RESTART_DELAY_MIN}
    fi
    bashio::log.warning "ca350.py exited (code ${rc}) after ${ran}s; restarting in ${delay}s ..."
    sleep "${delay}"
    delay=$(( delay * 2 ))
    if [ "${delay}" -gt "${RESTART_DELAY_MAX}" ]; then
        delay=${RESTART_DELAY_MAX}
    fi
done