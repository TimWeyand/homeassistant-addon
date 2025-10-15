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

# Run the Go application
exec /sma_battery_controller