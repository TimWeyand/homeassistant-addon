# SMA Battery Controller

**Warning:** This is not an official add-on and is not affiliated with SMA. Use at your own risk. This software is experimental.

## Overview

The SMA Battery Controller is a Home Assistant add-on that enables control over the battery of an SMA Sunny Tripower SE 10 inverter via Modbus TCP. It integrates with Home Assistant using MQTT, providing real-time monitoring and control of the inverter's battery functions.

The add-on allows you to:

- Monitor battery statistics such as state of charge, charge/discharge power, AC power, grid feed, and grid draw.
- Control the battery operation modes, including Automatic Logic Selection and Overwrite Logic Selection.
- Set custom battery charging or discharging power levels.
- Re-evaluate the control logic periodically (`reset_interval_minutes`) so the selected mode is enforced again.

## Features

- **Modbus TCP Communication**: Direct communication with the SMA inverter over Modbus TCP.
- **MQTT Integration**: Uses MQTT for communication with Home Assistant, supporting MQTT auto-discovery and retained states. After a Home Assistant restart (`homeassistant/status` = `online`) discovery and all states are republished.
- **Battery Control Modes** (see "Modes Description" for the details):
    - **Automatic**: The inverter operates automatically (control released).
    - **Balanced**: Grid-balancing discharge logic. Only works as **Overwrite** (not as Automatic Logic Selection). On entry, battery_control is set to 0 or to the current grid_draw (clamped), and only discharge commands are issued. While the battery charges with >= 500 W, Balanced issues no discharge command (the inverter stays in or is released to automatic), independent of the grid value. Fast 1-second polling is enabled in Balanced while battery_control > 0, the battery is not charging with >= 500 W, and it is discharging or the grid deviates by more than 30 W.
    - **Pause (charge ok)**: Pauses discharging; charging is allowed.
    - **Pause**: Pauses both charging and discharging.
    - **Charge Battery**: Forces the battery to charge at a specified power level.
    - **Discharge Battery**: Forces the battery to discharge at a specified power level.
- **Battery Control Input**: Set a custom power level for charging or discharging, within a configurable maximum limit. The value is published retained and loaded on startup (including 0); on startup it is clamped to `0..maximum_battery_control`, and defaults to 90 % of the maximum if nothing is stored.
- **Periodic Re-evaluation**: Every `reset_interval_minutes` the control logic is evaluated again. This does **not** reset any selection to "Automatic"; the selected modes stay as they are until you or an automation change them.
- **Retry on failed writes**: If a control command cannot be written to the inverter, it is retried on the next poll tick.
- **Debug Logging**: Detailed logging for troubleshooting when enabled.

## Installation

1. **Clone or Download the Add-on Repository**:

    - Recommended: in Home Assistant go to **Settings > Add-ons > Add-on Store**, open the three dots menu, select **Repositories** and add `https://github.com/TimWeyand/homeassistant-addon`.
    - Alternatively clone this repository into your Home Assistant `addons` directory, or download and place the files in a new directory under `addons/sma_battery_controller`. Note: the manifest sets `image: tweyand/sma_battery_controller-{arch}`, so a local-folder install pulls the published image of that version instead of building the Dockerfile locally; it only works if that image tag exists (remove the `image` line in your local copy to build locally).

2. **Refresh Add-on Store**:

    - In Home Assistant, navigate to **Settings > Add-ons**.
    - Click the three dots in the top right corner and select **Reload**.
    - The SMA Battery Controller add-on should now appear in the list of available add-ons.

3. **Install the Add-on**:

    - Click on the **SMA Battery Controller** add-on.
    - Click the **Install** button.

## Configuration

Before starting the add-on, you need to configure it according to your setup. The configuration options are available in the add-on's **Configuration** tab. All options are optional in the schema; the defaults below apply when an option is left out. The MQTT broker credentials are taken from what you enter here (`mqtt_*`).

### Options

- `mqtt_server_address` (string): Address of the MQTT broker. *(Default: "127.0.0.1")*

- `mqtt_server_port` (port): Port of the MQTT broker. *(Default: 1883)*

- `mqtt_user` (string): Username for the MQTT broker. *(Default: "")*

- `mqtt_password` (password): Password for the MQTT broker. *(Default: "")*

- `sma_inverter_modbus_address` (string): IP address of the SMA inverter. **Set this to your inverter's address**, the default is only a placeholder. *(Default: "192.168.1.100")*

- `sma_inverter_modbus_port` (port): Modbus TCP port of the SMA inverter. *(Default: 502)*

- `maximum_battery_control` (integer, 0-20000): Maximum allowed wattage for battery control (upper limit of the `Battery Control` number and basis of the SOC tapering). Set it to what your inverter/battery can actually do. *(Default: 5000)*

- `debug_enabled` (boolean): Enable detailed debug logging. *(Default: true)*

- `modbus_interval_in_seconds` (integer, >= 1): Interval in seconds for Modbus polling. *(Default: 5)*

- `reset_interval_minutes` (integer, >= 1): Interval in minutes in which the control logic is evaluated again (the selected mode is enforced again, e.g. after a missed write). It does **not** reset the selection to "Automatic". *(Default: 5)*

- `device_id` (string): Device ID used in the MQTT topics and discovery (change it only when running several instances). *(Default: "sma_battery_controller")*

- `post_command_delay_ms` (integer, >= 0): Pause in milliseconds after a written control command before the values are read back (the inverter needs a moment to apply them). Not applied in Balanced; it is also applied after a failed write. *(Default: 1600)*

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
device_id: "sma_battery_controller"
post_command_delay_ms: 1600
```

### Starting the Add-on

After configuring the add-on:

1. Click **Save** to save the configuration.
2. Go to the **Info** tab.
3. Click **Start** to start the add-on.
4. Watch the logs to ensure the add-on starts correctly and connects to your inverter.

### Standalone Docker (outside of Home Assistant)

`Dockerfile.standalone` and `docker-compose.standalone.yml` run the same binary with the options as environment variables (`MQTT_SERVER_ADDRESS`, `SMA_INVERTER_MODBUS_ADDRESS`, `MAXIMUM_BATTERY_CONTROL`, ...). The compose file contains only placeholders (`<mqtt-broker-host>`, `<mqtt-user>`, `<mqtt-password>`, `<inverter-ip>`): fill in your own values locally and never commit them. Set `MAXIMUM_BATTERY_CONTROL` explicitly, the built-in fallback of the binary/image is 6000.

## Home Assistant Integration

The add-on uses MQTT auto-discovery to integrate with Home Assistant. After starting the add-on:

- Home Assistant should automatically discover new entities provided by the add-on.
- The entities will appear under **Settings > Devices & Services > MQTT**.
- Entity IDs are built by Home Assistant from the device and entity name, typically `sensor.sma_battery_controller_<name>` (e.g. `sensor.sma_battery_controller_battery_state_of_charge`). Check the exact IDs in your installation; the names below are authoritative.

### Exposed Entities

- **Sensors** (state is not retained, it is republished after a Home Assistant restart):
    - Battery State of Charge, Battery Status, Battery Temperature, Battery Health, Battery Charge Power, Battery Discharge Power
    - Inverter Temperature (value scaled x0.1 since 0.0.32, previously x0.01 and therefore 10x too low)
    - DC1 Current / Voltage / Power and DC2 Current / Voltage / Power (DC2 was announced as "DC1 ..." before 0.0.32; entities created by older versions keep their `..._dc1_*_2` entity IDs, only the displayed names changed)
    - AC Power, Grid Feed Power, Grid Draw Power
    - Modbus Error Count - number of **consecutive** Modbus errors (reset after a fully successful read cycle)
    - Internal Mode Reason - explains internal automatic switching
    - Actual Charging Power Setting - shows the actually commanded power (live battery charge power in Automatic and internal automatic)

- **Binary Sensors**:
    - Internal Automatic - `ON` while the add-on has internally switched the inverter to automatic (see "Internal PV-aware switching" below), `OFF` while it is actively enforcing the selected mode. Retained, so the last value stays visible while the add-on is stopped; on every add-on start it is reset to `OFF` and then re-evaluated on the first control cycle (a Home Assistant restart keeps the real state).

- **Controls**:
    - Automatic Logic Selection (select)
    - Overwrite Logic Selection (select)
    - Battery Control (number, step 100, `0..maximum_battery_control`)
- **Read-only**:
    - Current Logic Selection (sensor) - the effective mode

### Using the Controls

The **effective mode** is chosen by this rule:

- **Overwrite Logic Selection = "Off"** (default): Overwrite is inactive, the **Automatic Logic Selection** is the effective mode.
- **Overwrite Logic Selection = any other value** (including "Automatic"): the Overwrite wins over the Automatic Logic Selection. Overwrite "Automatic" therefore actively keeps the inverter in automatic, no matter what the Automatic Logic Selection says. To hand control back to the Automatic Logic Selection, set the Overwrite to **"Off"**.

- **Automatic Logic Selection**: The default operating mode (used while the Overwrite is "Off"). "Balanced" has no effect here, it only works as Overwrite.

- **Overwrite Logic Selection**: Temporary override for automations or manual use, see the rule above.

- **Battery Control**: Set the desired power level (in watts) for "Charge Battery" or "Discharge Battery". In the Pause modes the value is stored but not applied. In "Balanced" the add-on adjusts the value itself: when you enter Balanced your value is saved and restored when you leave Balanced; changes you make while Balanced is active are dropped on leaving.

- **Stopping a forced charge from an automation:** setting the Overwrite to "Automatic" ends the forced command unconditionally (803). Setting it to "Off" only hands control back to the Automatic Logic Selection: provided that is "Automatic", the inverter runs in automatic; if it is still "Charge Battery" / "Discharge Battery" / "Pause (charge ok)", that forced command continues. In automatic the inverter can keep charging from PV surplus, possibly up to 100 %. For a hard stop of charging set the Overwrite to **"Pause"** instead.

### Modes Description

Technically the add-on writes two inverter registers: 40151 (SpntCom, `802` = external control active, `803` = inverter automatic) and 40149 (PwrAtCom, power in W; negative = charge, positive = discharge).

- **Automatic**: The inverter operates in its default automatic mode (803). Commands are not repeated while the mode stays Automatic.

- **Pause (charge ok)**: The battery will not discharge; it will charge if possible (802, 0 W).
  - **PV-aware switching**: Automatically switches to automatic mode when excess PV is detected (grid feed > 100W)
  - Reverts to pause when battery starts discharging (> 100W) and the grid feed is no longer > 100W

- **Pause**: The battery will neither charge nor discharge (802, 0 W). This is the hard stop.

- **Charge Battery**: Forces the battery to charge at the specified power level set in Battery Control (802, negative power).
  - **PV-aware switching**: Automatically switches to automatic mode when significant excess PV is detected (grid feed > 500W). PV charging above the setpoint (and above the SOC taper) is intended and always fine: the inverter charges from surplus, possibly up to 100 %.
  - Reverts to forced charging when battery charging drops below `max(tapered power - 500 W, tapered power / 2)`, where tapered power is the set value after the SOC taper below (equal to the set value at SOC <= 80 %); the second term makes the revert possible for small setpoints
  - **SOC-based tapering**: Automatically reduces charging power at high SOC levels to protect battery health:
    - 80-85% SOC: Max 80% of maximum power
    - 85-90% SOC: Max 60% of maximum power
    - 90-95% SOC: Max 30% of maximum power
    - 95-100% SOC: Max 15% of maximum power

- **Discharge Battery**: Forces the battery to discharge at the specified power level set in Battery Control (802, positive power).

- **Balanced** (Overwrite only): Discharge-only grid balancing, see "Features". If the decision is "do not write" (battery_control 0, or the battery charging with >= 500 W, or no grid deviation), the inverter is released with `803` once if an earlier forced command (`802`) is still active, so a previous forced charge does not stay in effect.

If a write to the inverter fails, the mode is **not** marked as applied and is retried on the next poll tick.

### Internal PV-aware switching (`Internal Automatic` binary sensor)

In two modes the add-on temporarily lets the inverter run itself instead of enforcing the selected mode, so excess PV production is not wasted. The `Internal Automatic` binary sensor turns `ON` while this override is active and `OFF` while the add-on is actively enforcing the mode:

- **Pause (charge ok)**:
  - Switches internally to automatic when grid feed (export) is **> 100 W** (there is excess PV to charge with).
  - Switches back to enforcing Pause once battery discharge power is **> 100 W** and the grid feed is no longer > 100 W (the battery started discharging, which "Pause (charge ok)" must prevent). While the feed is still > 100 W the add-on stays in internal automatic even if the battery discharges.
- **Charge Battery**:
  - Switches internally to automatic when grid feed (export) is **> 500 W** (significant excess PV, better handled by the inverter's own MPP/charge logic).
  - Switches back to enforcing the forced charge power once the battery's actual charge power drops below `max(tapered power - 500 W, tapered power / 2)` (tapered power = the configured power after the SOC taper, equal to it at SOC <= 80 %; the inverter is no longer charging fast enough on its own).
- All other modes (`Automatic`, `Pause`, `Discharge Battery`, `Balanced`) never use this internal override; the sensor is `OFF` there.
- The sensor also flips to `OFF` whenever the mode itself changes (Automatic/Overwrite Logic Selection), before the new mode's own switching rules take over. While a failed write is retried it can flip `OFF`/`ON` on each retry (cosmetic).
- See `Internal Mode Reason` for the human-readable reason behind the current state.

## Failure Behavior

- **Add-on or process dies:** the inverter itself falls back to automatic after about 5-30 minutes without a new control command. The add-on therefore does not need (and does not do) a shutdown fail-safe. Until then the last command stays active.
- **Availability:** all entities use the availability topic `smastp_modbus/status` (birth `online`, last will `offline`), so they show as "unavailable" in Home Assistant while the add-on is stopped or restarting (including the `run.sh` backoff after an exit).
- **Modbus errors:** a failed read aborts the cycle and reconnects. After **more than 20 consecutive** errors the process exits and the `run.sh` supervisor restarts it with backoff. A startup without Modbus connection also exits and is retried by the supervisor.
- **Known limit:** if only *writes* keep failing while reads still work, every successful read cycle resets the error counter, so the process never exits; it retries once per poll tick (with reconnect) until the write works.
- **Known limit of `device_id`:** the availability topic is fixed and not derived from `device_id`; with several instances, one instance going down marks the entities of all instances unavailable.
- **Known limits of Balanced:** when the add-on starts with a retained Balanced overwrite, the pre-Balanced `battery_control` is not known and cannot be restored; the republish after a Home Assistant restart uses a snapshot and can be briefly stale.

## Important Notes

- **Safety**: Controlling inverter settings may have implications on your electrical system's performance and safety. Ensure you understand the impact of the settings you apply.

- **Experimental Software**: This add-on is experimental. Thoroughly test in a controlled environment before relying on it.

- **Not Affiliated with SMA**: This software is not endorsed or affiliated with SMA Solar Technology AG.

## Troubleshooting

- **Connection Issues**:

    - Verify that the Modbus TCP interface on your inverter is enabled and accessible.
    - Ensure the MQTT broker details are correct.
    - A rising `Modbus Error Count` shows consecutive failures; it returns to 0 after a successful cycle.

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