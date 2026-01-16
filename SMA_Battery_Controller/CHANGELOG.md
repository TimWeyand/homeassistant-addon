# Changelog
**Warning:** This is not an official add-on and is not affiliated with SMA. Use at your own risk. This software is experimental.

## 0.0.28
- **Standalone Docker Support:**
  - Added `Dockerfile.standalone` for running outside of Home Assistant (e.g., on NAS/Unraid)
  - Added `docker-compose.standalone.yml` with example configuration
  - Multi-arch support: `linux/amd64` and `linux/arm64`
  - New Docker image: `tweyand/sma-battery-controller:latest` (standalone)
  - CI automatically builds and pushes standalone image alongside HA Add-on
  - No code changes required - same Go binary, different packaging

## 0.0.27
- **MQTT AutoReconnect Fix:**
  - Added `SetAutoReconnect(true)` - MQTT client now automatically reconnects after connection loss
  - Added `SetMaxReconnectInterval(1 * time.Minute)` - limits reconnect backoff to 1 minute
  - Added `SetConnectRetry(true)` with 10-second retry interval for initial connection
  - Added `ConnectionLostHandler` - logs when connection is lost
  - Added `OnConnectHandler` - re-subscribes to command topics after every reconnect
  - **Critical fix**: Previously, after MQTT broker restart or network issues, the add-on would stop receiving commands until manually restarted

## 0.0.26
- **Build system update:**
  - Updated GitHub Actions workflow to use explicit `--amd64 --aarch64` instead of deprecated `--all`
  - Removed unsupported architectures (armhf, armv7, i386) - Home Assistant builder now only supports amd64 and aarch64
- **Case-insensitive mode handling:**
  - All mode comparisons are now case-insensitive (e.g., "pause (charge ok)", "PAUSE (CHARGE OK)", "Pause (Charge Ok)" all work)
  - Added `normalizeMode()` function that maps any case variation to the canonical form
  - Invalid/unknown modes now safely fallback to "Automatic" with a WARNING log (always logged, not just in debug mode)
- **Battery control validation in Pause modes:**
  - Battery control value changes are now ignored in "Pause (charge ok)" and "Pause" modes
  - Values are still stored but not applied, preventing accidental charging from grid
  - Logged warning when battery control is set but ignored due to pause mode
- **Thread safety improvements:**
  - Added mutex protection for all global mode variable access in MQTT handler
  - `getCurrentMode()` is now thread-safe with proper locking
  - Added `getCurrentModeUnsafe()` for internal use when lock is already held
  - Prevents race conditions between MQTT handler and control logic goroutines

## 0.0.25
- **Fixed PV-aware switching issues:**
  - Added initial sensor value publishing for `actual_charging_power_setting` and `internal_mode_reason`
  - Fixed control logic not being applied on startup (now applies after first sensor read)
  - Enhanced `checkPauseChargeOkMode()` to always evaluate PV-aware modes ("Pause (charge ok)" and "Charge Battery")
  - Ensures immediate response to PV changes without waiting for reset timer

## 0.0.24
- **Critical Bug Fixes:**
  - Fixed race condition in sensor value access between goroutines (added `sensorMu` mutex protection)
  - Fixed potential deadlock when `setupModbus()` was called while holding `modbusMu` lock
  - Improved `loadInitialSettings()` reliability (removed race condition comment, increased timeout to 1s)
  - Fixed `shouldApply` logic in `applyControlLogic()` that was always evaluating to true
- **Stability Improvements:**
  - All sensor values (`batterySoc`, `batteryChargePower`, `batteryDischargePower`, `gridFeed`, `gridDraw`) are now thread-safe
  - Proper mutex unlock on error paths in `writeControlCommands()`
  - More predictable control logic evaluation

## 0.0.23
- **Enhanced PV-aware automatic switching:**
  - **Pause (charge ok)**: Now switches internally to automatic when grid feed > 100W (excess PV), reverts to pause when battery discharge > 100W
  - **Charge Battery**: Switches internally to automatic when grid feed > 500W (significant excess PV), reverts to charge when battery charging drops below (set value - 500W)
  - Both modes optimize PV utilization while maintaining user intent
- **SOC-based charge tapering** (Charge Battery mode only):
  - 80-85% SOC: Max 80% of maximum power
  - 85-90% SOC: Max 60% of maximum power
  - 90-95% SOC: Max 30% of maximum power
  - 95-100% SOC: Max 15% of maximum power
  - Protects battery health by reducing charge rate at high SOC levels
- **New MQTT sensors:**
  - `internal_mode_reason`: Explains why the controller switched internally (e.g., "Excess PV detected", "Battery discharging detected")
  - `actual_charging_power_setting`: Shows actual power being commanded after SOC tapering and internal logic
- **Improved control logic:**
  - Faster reaction to sensor changes with immediate evaluation
  - Internal state properly reset when switching between modes
  - Better logging for debugging internal automatic switching

## 0.0.22
- Fix: Preserve retained battery_control=0 on startup. Removed default override in discovery to avoid publishing 5400 after loading 0 from MQTT.
- Balanced entry behavior: When switching Overwrite to Balanced, set battery_control to 0 or to the current grid_draw (clamped to max) if discharge is required. Publish the new retained number state before applying control.
- Docs: Updated README to document retained state loading, Balanced behavior on entry, and read-only Current Logic Selection.

## 0.0.21
- Balanced: Consider battery_charge_power. If charging is active (>=500 W) and grid_draw/grid_feed are near zero (<=30 W), stay in automatic (no discharge commands).
- Persistence: Do not overwrite battery_control on startup if a retained state (including 0) exists; load from MQTT and retain across restarts.
- Fast polling: Only enable 1-second sensor polling when Balanced overwrite is active and we're actually in discharge activity (not charging and grid deviating from zero or currently discharging).

## 0.0.20
- When Balanced overwrite is active, poll sensor data every second for faster reaction to grid changes. In other modes, keep the configured polling interval.

## 0.0.19
- Balanced: Do not send any Modbus write when internally Automatic (battery_control=0) — no more 803/0 writes in this case.
- Balanced: Dynamically adjust and publish battery_control. Increase by grid_draw when drawing from grid; decrease by grid_feed when exporting. Clamp within max. If it reaches 0, stop writing and switch to internal Automatic.
- Balanced: Still only acts when Overwrite is set to Balanced, and only sends discharge commands. Skips post_command_delay_ms for responsiveness.

## 0.0.18
- Balanced: if battery_control becomes 0 or remains 0, treat as internal Automatic and do not send Modbus commands (prevents discharge→automatic→discharge oscillation).
- Balanced: ignore post_command_delay_ms to react quickly to grid_draw/grid_feed changes; we still read back immediately without waiting.
- Kept guard that Balanced only sends discharge commands and only when Overwrite is set to Balanced (not in Automatic mode).

## 0.0.17
- Balanced mode now sends Modbus commands only when Overwrite is set to Balanced (not in Automatic mode), and only issues discharge commands (no writes for automatic/no-control).

## 0.0.16
- Add new "Balanced" option to Automatic and Overwrite Logic with grid-based discharge control.
- In Balanced: if grid_draw=0 and battery_discharge_power=0 → switch to Automatic (no control) and set battery_control to 0; if grid_draw>0 → discharge with battery_control+grid_draw; if grid_draw=0 and grid_feed>0 → discharge with battery_control-grid_feed if positive, otherwise switch to Automatic and set battery_control to 0.
- Remove old Current Logic Selection select entity by clearing its MQTT discovery topics; keep new sensor entity (Unknown on start until first update).

## 0.0.15
- Make Current Logic Selection read-only by publishing it as a sensor (no command entity in Home Assistant).
- Ensure no Modbus command is sent when battery_control changes while in Automatic mode (logic already prevents writes; documented behavior).
- Reduce telemetry noise: publish sensors on startup, only when values change, and force a refresh every 30 minutes.

## 0.0.14
- Make post-command stabilization delay configurable via environment variable POST_COMMAND_DELAY_MS and add-on options (config.json/config.yaml). Default is 1600 ms.

## 0.0.13
- Increase post-command stabilization delay by 300ms (now 500ms) before reading back sensor values.

## 0.0.12
- Read and publish sensor data immediately after sending Modbus settings.
- Add a short delay after successful write to ensure fresh values are read from the inverter.

## 0.0.11
- Always publish discovery for selects (automatic, overwrite, current) and battery control number so Home Assistant can send commands.
- Wait for MQTT wildcard subscription to complete to ensure commands are received immediately.

## 0.0.10
- Ensure Modbus commands are sent immediately on MQTT commands by serializing Modbus access with a mutex.
- Prevent interference between reads and writes and between concurrent commands by locking around Modbus IO and command application.

## 0.0.9
- Performance optimizations:
  - Avoid redundant MQTT publishes by caching last sensor values.
  - Reduced allocations by caching MQTT topic prefixes and using efficient number formatting.
  - Replaced per-iteration map with static register list in Modbus read loop.
  - Non-blocking MQTT publishes for high-frequency telemetry (retain=false).

## 0.0.8
- trying to optimize reconnect

## 0.0.7
- added some more sensors
  - battery_temperature
  - inverter_temperature
  - battery_health
  - battery_status
  - dc1_current
  - dc1_voltage
  - dc1_power
  - dc2_current
  - dc2_voltage
  - dc2_power

## 0.0.6
- Add currentLogicSelection to see the current active Modus
- Check for broken pipe at modbus connection (also monitor count / time)
- make deviceId configurable
- Change Hardcoded deviceId to configurable deviceId

## 0.0.5
- Removed Check and Reset, which caused to remove the OverwriteLogicSelection to reset

## 0.0.4
- Fixed the Logic for Pause (charge ok)

## 0.0.3
- Fix an overwrite of BatteryControl on Startup
- Fix that control commands are not send on ReadIntervall

## 0.0.2
- Retain Configuration in MQTT and read them on startup

