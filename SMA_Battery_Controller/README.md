# SMA Battery Controller

**Warning:** This is not an official add-on and is not affiliated with SMA. Use at your own risk. This software is experimental.

## Overview

The SMA Battery Controller is a Home Assistant add-on that enables control over the battery of an SMA Sunny Tripower SE 10 inverter via Modbus TCP. It integrates with Home Assistant using MQTT, providing real-time monitoring and control of the inverter's battery functions.

The add-on allows you to:

- Monitor battery statistics such as state of charge, charge/discharge power, AC power, grid feed, and grid draw.
- Control the battery operation modes, including Automatic Logic Selection and Overwrite Logic Selection.
- Set custom battery charging or discharging power levels.
- Automatically reset settings after a configurable interval.

## Features

- **Modbus TCP Communication**: Direct communication with the SMA inverter over Modbus TCP.
- **MQTT Integration**: Uses MQTT for communication with Home Assistant, supporting MQTT auto-discovery and retained states.
- **Battery Control Modes**:
    - **Automatic**: Default mode where the inverter operates automatically.
    - **Balanced**: Grid-balancing discharge logic. On entry, battery_control is set to 0 or to the current grid_draw (clamped), and only discharge commands are issued. When charging is active and grid is near zero, Balanced stays automatic (no discharge). Fast 1-second polling is enabled only while actively discharging in Balanced.
    - **Pause (charge ok)**: Pauses discharging; charging is allowed.
    - **Pause**: Pauses both charging and discharging.
    - **Charge Battery**: Forces the battery to charge at a specified power level.
    - **Discharge Battery**: Forces the battery to discharge at a specified power level.
- **Battery Control Input**: Set a custom power level for charging or discharging, within a configurable maximum limit. The value is published retained and loaded on startup (including 0).
- **Automatic Reset**: Option to reset the Overwrite Logic Selection to "Automatic" after a specified interval.
- **Debug Logging**: Detailed logging for troubleshooting when enabled.

## Installation

1. **Clone or Download the Add-on Repository**:

    - Clone this repository into your Home Assistant `addons` directory, or download and place the files in a new directory under `addons/sma_battery_controller`.

2. **Refresh Add-on Store**:

    - In Home Assistant, navigate to **Settings > Add-ons**.
    - Click the three dots in the top right corner and select **Reload**.
    - The SMA Battery Controller add-on should now appear in the list of available add-ons.

3. **Install the Add-on**:

    - Click on the **SMA Battery Controller** add-on.
    - Click the **Install** button.

## Configuration

Before starting the add-on, you need to configure it according to your setup. The configuration options are available in the add-on's **Configuration** tab.

### Options

- `mqtt_server_address` (string): Address of the MQTT broker. *(Default: "127.0.0.1")*

- `mqtt_server_port` (integer): Port of the MQTT broker. *(Default: 1883)*

- `mqtt_user` (string): Username for the MQTT broker. *(Default: "")*

- `mqtt_password` (string): Password for the MQTT broker. *(Default: "")*

- `sma_inverter_modbus_address` (string): IP address of the SMA inverter. *(Required)*

- `sma_inverter_modbus_port` (integer): Modbus TCP port of the SMA inverter. *(Default: 502)*

- `maximum_battery_control` (integer): Maximum allowed wattage for battery control. *(Default: 5000)*

- `debug_enabled` (boolean): Enable detailed debug logging. *(Default: true)*

- `modbus_interval_in_seconds` (integer): Interval in seconds for Modbus polling. *(Default: 5)*

- `reset_interval_minutes` (integer): Interval in minutes after which the Overwrite Logic Selection resets to "Automatic". *(Default: 5)*

### Example Configuration

```yaml
mqtt_server_address: "127.0.0.1"
mqtt_server_port: 1883
mqtt_user: "your_mqtt_username"
mqtt_password: "your_mqtt_password"
sma_inverter_modbus_address: "192.168.1.100"
sma_inverter_modbus_port: 502
maximum_battery_control: 5000
debug_enabled: true
modbus_interval_in_seconds: 5
reset_interval_minutes: 5
```

### Starting the Add-on

After configuring the add-on:

1. Click **Save** to save the configuration.
2. Go to the **Info** tab.
3. Click **Start** to start the add-on.
4. Watch the logs to ensure the add-on starts correctly and connects to your inverter.

## Home Assistant Integration

The add-on uses MQTT auto-discovery to integrate with Home Assistant. After starting the add-on:

- Home Assistant should automatically discover new entities provided by the add-on.
- The entities will appear under **Settings > Devices & Services > MQTT**.

### Exposed Entities

- **Sensors**:
    - Battery State of Charge (`sensor.battery_soc`)
    - Battery Charge Power (`sensor.battery_charge_power`)
    - Battery Discharge Power (`sensor.battery_discharge_power`)
    - AC Power (`sensor.ac_power`)
    - Grid Feed Power (`sensor.grid_feed`)
    - Grid Draw Power (`sensor.grid_draw`)
    - Internal Mode Reason (`sensor.internal_mode_reason`) - Explains internal automatic switching
    - Actual Charging Power Setting (`sensor.actual_charging_power_setting`) - Shows actual commanded power

- **Controls**:
    - Automatic Logic Selection (`select.automatic_logic_selection`)
    - Overwrite Logic Selection (`select.overwrite_logic_selection`)
    - Battery Control (`number.battery_control`)
- **Read-only**:
    - Current Logic Selection (`sensor.current_logic_selection`)

### Using the Controls

- **Automatic Logic Selection**: Sets the default operating mode when Overwrite Logic Selection is set to "Automatic".

- **Overwrite Logic Selection**: Overrides the Automatic Logic Selection when set to any mode other than "Automatic".

- **Battery Control**: Set the desired power level (in watts) for charging or discharging when in "Charge Battery" or "Discharge Battery" modes.

### Modes Description

- **Automatic**: The inverter operates in its default automatic mode.

- **Pause (charge ok)**: The battery will not discharge; it will charge if possible.
  - **PV-aware switching**: Automatically switches to automatic mode when excess PV is detected (grid feed > 100W)
  - Reverts to pause when battery starts discharging (> 100W)

- **Pause**: The battery will neither charge nor discharge.

- **Charge Battery**: Forces the battery to charge at the specified power level set in Battery Control.
  - **PV-aware switching**: Automatically switches to automatic mode when significant excess PV is detected (grid feed > 500W)
  - Reverts to forced charging when battery charging drops below (set value - 500W)
  - **SOC-based tapering**: Automatically reduces charging power at high SOC levels to protect battery health:
    - 80-85% SOC: Max 80% of maximum power
    - 85-90% SOC: Max 60% of maximum power
    - 90-95% SOC: Max 30% of maximum power
    - 95-100% SOC: Max 15% of maximum power

- **Discharge Battery**: Forces the battery to discharge at the specified power level set in Battery Control.

## Important Notes

- **Safety**: Controlling inverter settings may have implications on your electrical system's performance and safety. Ensure you understand the impact of the settings you apply.

- **Experimental Software**: This add-on is experimental. Thoroughly test in a controlled environment before relying on it.

- **Not Affiliated with SMA**: This software is not endorsed or affiliated with SMA Solar Technology AG.

## Troubleshooting

- **Connection Issues**:

    - Verify that the Modbus TCP interface on your inverter is enabled and accessible.
    - Ensure the MQTT broker details are correct.

- **Entities Not Showing Up**:

    - Confirm that MQTT auto-discovery is enabled in Home Assistant.
    - Restart the MQTT integration or Home Assistant if necessary.

- **Debugging**:

    - Set `debug_enabled` to `true` to enable detailed logging.
    - Check the logs for any error messages that can help identify issues.

## License

This project is licensed under the MIT License.

## Disclaimer

This software is provided "as is", without warranty of any kind. Use it at your own risk.

---

*This README was generated by AI.*