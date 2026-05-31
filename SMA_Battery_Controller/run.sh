#!/usr/bin/with-contenv bashio

# Export environment variables from add-on options
export MQTT_SERVER_ADDRESS=$(bashio::config 'mqtt_server_address')
export MQTT_SERVER_PORT=$(bashio::config 'mqtt_server_port')
export MQTT_USER=$(bashio::config 'mqtt_user')
export MQTT_PASSWORD=$(bashio::config 'mqtt_password')
export SMA_INVERTER_MODBUS_ADDRESS=$(bashio::config 'sma_inverter_modbus_address')
export SMA_INVERTER_MODBUS_PORT=$(bashio::config 'sma_inverter_modbus_port')
export MAXIMUM_BATTERY_CONTROL=$(bashio::config 'maximum_battery_control')
export DEBUG_ENABLED=$(bashio::config 'debug_enabled')
export MODBUS_INTERVAL_IN_SECONDS=$(bashio::config 'modbus_interval_in_seconds')
export RESET_INTERVAL_MINUTES=$(bashio::config 'reset_interval_minutes')
export DEVICE_ID=$(bashio::config 'device_id')
export POST_COMMAND_DELAY_MS=$(bashio::config 'post_command_delay_ms')

# --- Self-healing supervisor -------------------------------------------------
# The Go binary already auto-reconnects to MQTT on connection loss. This loop is
# an additional safety net: if the binary ever exits on its own (e.g. after
# sustained Modbus errors it calls os.Exit/log.Fatal, or the inverter is asleep
# at night and the Modbus connect fails), restart it automatically so the
# add-on never needs a manual restart. A clean stop from Home Assistant
# (SIGTERM) breaks the loop instead of restarting.
#
# The restart delay uses exponential backoff (10s -> 300s) so a permanently
# unreachable inverter does NOT cause a 10s restart storm with the Home
# Assistant device flapping online/offline. A process that ran healthily for at
# least HEALTHY_RUNTIME seconds resets the backoff, so ordinary crashes still
# recover quickly.
RESTART_DELAY_MIN=10
RESTART_DELAY_MAX=300
HEALTHY_RUNTIME=60
delay=${RESTART_DELAY_MIN}
running=true
child_pid=""

term_handler() {
    running=false
    if [ -n "${child_pid}" ]; then
        kill -TERM "${child_pid}" 2>/dev/null
        wait "${child_pid}" 2>/dev/null
    fi
    exit 0
}
trap term_handler SIGTERM SIGINT

while [ "${running}" = true ]; do
    started=${SECONDS}
    /sma_battery_controller &
    child_pid=$!
    wait "${child_pid}"
    rc=$?
    if [ "${running}" != true ]; then
        break
    fi
    ran=$(( SECONDS - started ))
    if [ "${ran}" -ge "${HEALTHY_RUNTIME}" ]; then
        delay=${RESTART_DELAY_MIN}
    fi
    bashio::log.warning "SMA Battery Controller exited (code ${rc}) after ${ran}s; restarting in ${delay}s ..."
    sleep "${delay}"
    delay=$(( delay * 2 ))
    if [ "${delay}" -gt "${RESTART_DELAY_MAX}" ]; then
        delay=${RESTART_DELAY_MAX}
    fi
done