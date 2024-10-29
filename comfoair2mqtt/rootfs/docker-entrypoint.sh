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


cat <<EOF > "/opt/hacomfoairmqtt/src/config.ini"
[DEFAULT]
SerialPort=$(bashio::config 'serial')
RS485_protocol=$COMFOAIR2MQTT_CONFIG_RS485
refresh_interval=$(bashio::config 'refresh_interval')
enablePcMode=$COMFOAIR2MQTT_CONFIG_PCMode
debug=$COMFOAIR2MQTT_CONFIG_debug

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

bashio::log.info "Startup hacomfoairmqtt"
python3 /opt/hacomfoairmqtt/src/ca350.py