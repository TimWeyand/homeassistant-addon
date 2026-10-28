package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	modbus "github.com/goburrow/modbus"
)

// regDef describes a Modbus input register we poll and expose
type regDef struct {
	name string
	addr uint16
}

var (
	mqttClient              mqtt.Client
	modbusClient            modbus.Client
	modbusHandler           *modbus.TCPClientHandler // kept so a reconnect can close the old TCP connection
	modbusClientErrorCount  atomic.Int32             // CONSECUTIVE Modbus failures; reset after a fully successful read cycle
	maximumBatteryControl   int
	modbusIntervalInSeconds int
	debugEnabled            bool
	automaticLogicSelection string
	overwriteLogicSelection string
	currentLogicSelection   string
	batteryControl          int
	lastValidBatteryControl int
	batteryDischargePower   int
	batteryChargePower      int
	previousMode            string // last mode whose command was written successfully
	deviceID                string
	resetIntervalMinutes    int // Reset interval
	gridDraw                int
	gridFeed                int
	pauseActivated          bool
	postCommandDelayMs      int // Delay after write before readback
	batterySoc              int // Battery State of Charge

	// Internal automatic switching state
	internalAutomatic       bool   // True when internally switched to automatic
	internalAutomaticReason string // Reason for internal switching
	actualChargingPower     int    // Actual charging power being commanded (after SOC tapering)

	// Last SpntCom written successfully. Starts as controlOn ("unknown, maybe
	// still forced by a previous run") so the first no-write decision releases it.
	lastWrittenSpntCom uint32 = controlOn

	// battery_control the user had before the overwrite switched INTO Balanced;
	// restored when the overwrite leaves Balanced again.
	savedUserBatteryControl      int
	savedUserBatteryControlValid bool

	// Set once startup (load, discovery, Modbus) is done; HA-restart republish waits for it.
	startupDone atomic.Bool

	// Flags for retained state loading
	batteryControlLoaded     bool
	automaticSelectionLoaded bool
	overwriteSelectionLoaded bool

	// Synchronization primitives to prevent Modbus command interference
	modbusMu  sync.Mutex
	controlMu sync.Mutex
	sensorMu  sync.Mutex // Protects sensor value globals
	readMu    sync.Mutex // Serializes readAndPublishData (protects lastSensorValues)
	// Lock order: controlMu -> readMu -> sensorMu / modbusMu

	// Cached topic prefixes
	sensorTopicPrefix string

	// Cache of last published sensor values to avoid redundant publishes (guarded by readMu)
	lastSensorValues map[string]string

	// modbusReconnect is a var so tests can replace the real TCP reconnect.
	modbusReconnect = setupModbus
)

const (
	nearZeroW       = 30  // threshold to treat grid draw/feed as ~0
	chargeActiveW   = 500 // threshold to consider charging actively happening
	maxModbusErrors = 20  // consecutive Modbus failures before exiting (run.sh supervisor restarts us)

	controlOn  uint32 = 802 // SpntCom: external control active
	controlOff uint32 = 803 // SpntCom: inverter automatic
)

func main() {
	// Load environment variables
	loadConfig()

	// Set up MQTT client
	setupMQTT()

	// Load initial settings from MQTT
	loadInitialSettings()

	// Publish MQTT discovery messages
	publishDiscoveryMessages()

	// Set up Modbus client; at startup a failure exits so the run.sh supervisor restarts us
	if err := setupModbus(); err != nil {
		log.Fatalf("Modbus connection error: %v", err)
	}
	startupDone.Store(true)

	// Start Modbus reading loop
	go modbusReadLoop()

	// MQTT Subscribe wird jetzt im OnConnectHandler gemacht (für AutoReconnect-Support)
	// Das initiale Subscribe passiert automatisch beim ersten Connect

	// Keep the application running
	select {}
}

func loadConfig() {
	// Load and parse environment variables
	var err error

	maximumBatteryControl, err = strconv.Atoi(getEnv("MAXIMUM_BATTERY_CONTROL", "6000"))
	if err != nil {
		log.Fatalf("Invalid MAXIMUM_BATTERY_CONTROL: %v", err)
	}

	modbusIntervalInSeconds, err = strconv.Atoi(getEnv("MODBUS_INTERVAL_IN_SECONDS", "5"))
	if err != nil {
		log.Fatalf("Invalid MODBUS_INTERVAL_IN_SECONDS: %v", err)
	}
	if modbusIntervalInSeconds <= 0 {
		// time.NewTicker panics on a non-positive duration
		modbusIntervalInSeconds = 5
	}

	debugEnabled, err = strconv.ParseBool(getEnv("DEBUG_ENABLED", "true"))
	if err != nil {
		debugEnabled = true
	}

	resetIntervalMinutes, err = strconv.Atoi(getEnv("RESET_INTERVAL_MINUTES", "5"))
	if err != nil || resetIntervalMinutes <= 0 {
		resetIntervalMinutes = 5
	}

	// Post-command stabilization delay (ms)
	postCommandDelayMs, err = strconv.Atoi(getEnv("POST_COMMAND_DELAY_MS", "1600"))
	if err != nil || postCommandDelayMs < 0 {
		postCommandDelayMs = 1600
	}

	deviceID = getEnv("DEVICE_ID", "sma_battery_controller")

	// Initialize control variables
	automaticLogicSelection = "Automatic"
	overwriteLogicSelection = "Off"
	currentLogicSelection = "Automatic"
	lastValidBatteryControl = 0
	previousMode = ""

	// Precompute topic prefixes and initialize caches
	sensorTopicPrefix = "homeassistant/sensor/" + deviceID + "/"
	lastSensorValues = make(map[string]string, 24)
}

func setupMQTT() {
	// Set up MQTT options
	opts := mqtt.NewClientOptions()
	mqttServerAddress := getEnv("MQTT_SERVER_ADDRESS", "127.0.0.1")
	mqttServerPort := getEnv("MQTT_SERVER_PORT", "1883")
	brokerURL := fmt.Sprintf("tcp://%s:%s", mqttServerAddress, mqttServerPort)
	opts.AddBroker(brokerURL)
	mqttUser := getEnv("MQTT_USER", "")
	mqttPassword := getEnv("MQTT_PASSWORD", "")
	if mqttUser != "" {
		opts.Username = mqttUser
		opts.Password = mqttPassword
	}
	opts.SetClientID(deviceID)

	// Enable AutoReconnect - KRITISCH für stabile MQTT-Verbindung!
	opts.SetAutoReconnect(true)
	opts.SetMaxReconnectInterval(1 * time.Minute)
	opts.SetConnectRetry(true)
	opts.SetConnectRetryInterval(10 * time.Second)

	// Set Last Will and Testament (LWT)
	willTopic := "smastp_modbus/status"
	willPayload := "offline"
	opts.SetWill(willTopic, willPayload, 0, true)

	// Connection Lost Handler - wird aufgerufen wenn Verbindung verloren geht
	opts.SetConnectionLostHandler(func(c mqtt.Client, err error) {
		log.Printf("[MQTT] Connection lost: %v - AutoReconnect wird versuchen, die Verbindung wiederherzustellen", err)
	})

	// OnConnect Handler - wird bei JEDER (Re-)Verbindung aufgerufen
	opts.SetOnConnectHandler(func(c mqtt.Client) {
		log.Println("[MQTT] Connected to broker")

		// Publish birth message
		birthTopic := "smastp_modbus/status"
		birthPayload := "online"
		token := c.Publish(birthTopic, 0, true, birthPayload)
		token.Wait()
		if debugEnabled {
			log.Println("[MQTT] Published birth message to", birthTopic)
		}

		// Re-Subscribe zu Command-Topics nach Reconnect
		// Das ist KRITISCH - ohne Re-Subscribe werden keine Befehle mehr empfangen!
		listenTopic := fmt.Sprintf("homeassistant/+/%s/+/set", deviceID)
		token = c.Subscribe(listenTopic, 0, mqttMessageHandler)
		if token.Wait() && token.Error() != nil {
			log.Printf("[MQTT] Error re-subscribing to %s: %v", listenTopic, token.Error())
		} else {
			log.Printf("[MQTT] Re-subscribed to: %s", listenTopic)
		}

		// After a Home Assistant restart ("online" birth) re-announce discovery and states
		token = c.Subscribe("homeassistant/status", 0, haStatusHandler)
		if token.Wait() && token.Error() != nil {
			log.Printf("[MQTT] Error subscribing to homeassistant/status: %v", token.Error())
		}
	})

	// Create and start MQTT client
	mqttClient = mqtt.NewClient(opts)
	if token := mqttClient.Connect(); token.Wait() && token.Error() != nil {
		log.Fatalf("MQTT connection error: %v", token.Error())
	}
}

func publishDiscoveryMessages() {
	controlMu.Lock()
	autoSel, overSel, bc := automaticLogicSelection, overwriteLogicSelection, batteryControl
	controlMu.Unlock()
	publishDiscoveryConfigs(autoSel, overSel, bc)

	// Publish initial values for the new sensors
	stateTopic := sensorTopicPrefix + "internal_mode_reason/state"
	mqttPublish(stateTopic, []byte("Starting"), false)
	stateTopic = sensorTopicPrefix + "actual_charging_power_setting/state"
	mqttPublish(stateTopic, []byte("0"), false)

	// Initial state for the internal_automatic binary_sensor: always OFF right
	// after discovery. This is a plain publish (not setInternalAutomatic),
	// since internalAutomatic already defaults to false and the change-guard
	// in setInternalAutomatic would otherwise skip the publish.
	internalAutomatic = false
	stateTopic = fmt.Sprintf("homeassistant/binary_sensor/%s/internal_automatic/state", deviceID)
	mqttPublish(stateTopic, []byte("OFF"), true)
}

// haStatusHandler reacts to Home Assistant's birth message. Runs the republish
// in its own goroutine so the paho message router is not blocked.
func haStatusHandler(client mqtt.Client, msg mqtt.Message) {
	if string(msg.Payload()) == "online" && startupDone.Load() {
		go republishAfterHARestart()
	}
}

// republishAfterHARestart re-announces discovery configs and the current
// states without touching any control state (internalAutomatic etc.).
func republishAfterHARestart() {
	controlMu.Lock()
	autoSel, overSel, bc := automaticLogicSelection, overwriteLogicSelection, batteryControl
	curSel, intAuto, reason, acp := currentLogicSelection, internalAutomatic, internalAutomaticReason, actualChargingPower
	controlMu.Unlock()

	log.Println("[MQTT] Home Assistant online - republishing discovery and states")
	publishDiscoveryConfigs(autoSel, overSel, bc)
	mqttPublish(fmt.Sprintf("homeassistant/sensor/%s/current_logic_selection/state", deviceID), []byte(curSel), true)
	intAutoPayload := "OFF"
	if intAuto {
		intAutoPayload = "ON"
	}
	mqttPublish(fmt.Sprintf("homeassistant/binary_sensor/%s/internal_automatic/state", deviceID), []byte(intAutoPayload), true)
	mqttPublish(sensorTopicPrefix+"internal_mode_reason/state", []byte(reason), false)
	mqttPublish(sensorTopicPrefix+"actual_charging_power_setting/state", []byte(strconv.Itoa(acp)), false)
	// Sensor states are not retained: force the next poll to publish all of them
	resetSensorCache()
}

// resetSensorCache clears the publish cache so the next read cycle publishes every sensor.
func resetSensorCache() {
	readMu.Lock()
	lastSensorValues = make(map[string]string, len(polledRegisters)+1)
	readMu.Unlock()
}

// publishDiscoveryConfigs publishes all HA discovery configs plus the
// select/number states passed in.
func publishDiscoveryConfigs(autoSel, overSel string, bc int) {
	// Device information
	deviceInfo := map[string]interface{}{
		"identifiers":  []string{deviceID},
		"manufacturer": "Custom",
		"model":        "SMA Battery Controller",
		"name":         "SMA Battery Controller",
	}

	// Always publish discovery for selects and number so HA can send commands
	publishSelect("automatic_logic_selection", "Automatic Logic Selection", []string{"Automatic", "Balanced", "Pause (charge ok)", "Pause", "Charge Battery", "Discharge Battery"}, autoSel, deviceInfo)
	publishSelect("overwrite_logic_selection", "Overwrite Logic Selection", []string{"Off", "Automatic", "Balanced", "Pause (charge ok)", "Pause", "Charge Battery", "Discharge Battery"}, overSel, deviceInfo)
	// Make Current Logic Selection read-only by publishing as a sensor (no command topic)
	publishSensor("current_logic_selection", "Current Logic Selection", "", deviceInfo)
	// Remove old select-based Current Logic Selection entity by clearing its discovery and state
	oldSelectConfigTopic := fmt.Sprintf("homeassistant/select/%s/current_logic_selection/config", deviceID)
	mqttPublish(oldSelectConfigTopic, []byte(""), true)
	oldSelectStateTopic := fmt.Sprintf("homeassistant/select/%s/current_logic_selection/state", deviceID)
	mqttPublish(oldSelectStateTopic, []byte(""), true)

	publishNumber("battery_control", "Battery Control", 0, float64(maximumBatteryControl), 100, float64(bc), deviceInfo)

	// Publish sensors regardless of initial state
	publishSensor("battery_status", "Battery Status", "", deviceInfo)
	publishSensor("battery_soc", "Battery State of Charge", "%", deviceInfo)
	publishSensor("battery_temperature", "Battery Temperature", "°C", deviceInfo)
	publishSensor("inverter_temperature", "Inverter Temperature", "°C", deviceInfo)
	publishSensor("battery_diagnose_current_capacity", "Battery Health", "%", deviceInfo)
	publishSensor("battery_charge_power", "Battery Charge Power", "W", deviceInfo)
	publishSensor("battery_discharge_power", "Battery Discharge Power", "W", deviceInfo)
	publishSensor("dc1_current", "DC1 Current", "A", deviceInfo)
	publishSensor("dc1_voltage", "DC1 Voltage", "V", deviceInfo)
	publishSensor("dc1_power", "DC1 Power", "W", deviceInfo)
	publishSensor("dc2_current", "DC2 Current", "A", deviceInfo)
	publishSensor("dc2_voltage", "DC2 Voltage", "V", deviceInfo)
	publishSensor("dc2_power", "DC2 Power", "W", deviceInfo)
	publishSensor("ac_power", "AC Power", "W", deviceInfo)
	publishSensor("grid_feed", "Grid Feed Power", "W", deviceInfo)
	publishSensor("grid_draw", "Grid Draw Power", "W", deviceInfo)
	publishSensor("modbus_error_count", "Modbus Error Count", "", deviceInfo)
	publishSensor("internal_mode_reason", "Internal Mode Reason", "", deviceInfo)
	publishSensor("actual_charging_power_setting", "Actual Charging Power Setting", "W", deviceInfo)
	publishBinarySensor("internal_automatic", "Internal Automatic", deviceInfo)
}

// setInternalAutomatic updates the internalAutomatic flag and publishes the
// corresponding binary_sensor state (retained), but only when the value
// actually changes. This does not alter any control/Modbus decision logic -
// it is purely an additional, side-effect-free MQTT publish alongside the
// existing internalAutomatic assignment it replaces.
func setInternalAutomatic(value bool) {
	if internalAutomatic == value {
		return
	}
	internalAutomatic = value
	payload := "OFF"
	if value {
		payload = "ON"
	}
	stateTopic := fmt.Sprintf("homeassistant/binary_sensor/%s/internal_automatic/state", deviceID)
	mqttPublish(stateTopic, []byte(payload), true)
}

func publishSelect(objectID, name string, options []string, initial string, deviceInfo map[string]interface{}) {
	configTopic := fmt.Sprintf("homeassistant/select/%s/%s/config", deviceID, objectID)
	commandTopic := fmt.Sprintf("homeassistant/select/%s/%s/set", deviceID, objectID)
	stateTopic := fmt.Sprintf("homeassistant/select/%s/%s/state", deviceID, objectID)

	configPayload := map[string]interface{}{
		"name":          name,
		"command_topic": commandTopic,
		"state_topic":   stateTopic,
		"options":       options,
		"unique_id":     fmt.Sprintf("%s_%s", deviceID, objectID),
		"device":        deviceInfo,
		"availability": []map[string]string{
			{
				"topic":                 "smastp_modbus/status",
				"payload_available":     "online",
				"payload_not_available": "offline",
			},
		},
	}

	payloadBytes, _ := json.Marshal(configPayload)
	mqttPublish(configTopic, payloadBytes, true)

	// Publish initial state
	mqttPublish(stateTopic, []byte(initial), true)
}

func publishNumber(objectID, name string, min, max, step, initial float64, deviceInfo map[string]interface{}) {
	configTopic := fmt.Sprintf("homeassistant/number/%s/%s/config", deviceID, objectID)
	commandTopic := fmt.Sprintf("homeassistant/number/%s/%s/set", deviceID, objectID)
	stateTopic := fmt.Sprintf("homeassistant/number/%s/%s/state", deviceID, objectID)

	configPayload := map[string]interface{}{
		"name":                name,
		"command_topic":       commandTopic,
		"state_topic":         stateTopic,
		"min":                 min,
		"max":                 max,
		"step":                step,
		"unit_of_measurement": "W",
		"unique_id":           fmt.Sprintf("%s_%s", deviceID, objectID),
		"device":              deviceInfo,
		"availability": []map[string]string{
			{
				"topic":                 "smastp_modbus/status",
				"payload_available":     "online",
				"payload_not_available": "offline",
			},
		},
	}

	payloadBytes, _ := json.Marshal(configPayload)
	mqttPublish(configTopic, payloadBytes, true)

	// Publish initial state
	mqttPublish(stateTopic, []byte(fmt.Sprintf("%.0f", initial)), true)
}

func publishSensor(objectID, name, unit string, deviceInfo map[string]interface{}) {
	configTopic := fmt.Sprintf("homeassistant/sensor/%s/%s/config", deviceID, objectID)
	stateTopic := fmt.Sprintf("homeassistant/sensor/%s/%s/state", deviceID, objectID)

	configPayload := map[string]interface{}{
		"name":                name,
		"state_topic":         stateTopic,
		"unit_of_measurement": unit,
		"value_template":      "{{ value }}",
		"unique_id":           fmt.Sprintf("%s_%s", deviceID, objectID),
		"device":              deviceInfo,
		"availability": []map[string]string{
			{
				"topic":                 "smastp_modbus/status",
				"payload_available":     "online",
				"payload_not_available": "offline",
			},
		},
	}

	payloadBytes, _ := json.Marshal(configPayload)
	mqttPublish(configTopic, payloadBytes, true)
}

// publishBinarySensor publishes MQTT discovery config for a binary_sensor
// entity. Unlike publishSensor it does not publish an initial state; the
// state topic is written separately by the caller (see setInternalAutomatic
// and the initial-OFF publish in publishDiscoveryMessages).
func publishBinarySensor(objectID, name string, deviceInfo map[string]interface{}) {
	configTopic := fmt.Sprintf("homeassistant/binary_sensor/%s/%s/config", deviceID, objectID)
	stateTopic := fmt.Sprintf("homeassistant/binary_sensor/%s/%s/state", deviceID, objectID)

	configPayload := map[string]interface{}{
		"name":        name,
		"state_topic": stateTopic,
		"payload_on":  "ON",
		"payload_off": "OFF",
		"unique_id":   fmt.Sprintf("%s_%s", deviceID, objectID),
		"device":      deviceInfo,
		"availability": []map[string]string{
			{
				"topic":                 "smastp_modbus/status",
				"payload_available":     "online",
				"payload_not_available": "offline",
			},
		},
	}

	payloadBytes, _ := json.Marshal(configPayload)
	mqttPublish(configTopic, payloadBytes, true)
}

// setupModbus (re)creates the Modbus TCP client, closing a previous connection.
// The new client is installed even if Connect fails: the goburrow handler
// connects lazily on the next request, so the next poll simply retries.
func setupModbus() error {
	log.Printf("Setting up modbus")
	// Create Modbus TCP client handler
	handler := modbus.NewTCPClientHandler(
		fmt.Sprintf("%s:%s",
			getEnv("SMA_INVERTER_MODBUS_ADDRESS", "192.168.1.100"),
			getEnv("SMA_INVERTER_MODBUS_PORT", "502")),
	)
	handler.Timeout = 10 * time.Second
	handler.SlaveId = 3 // SMA inverter Modbus slave ID

	// Connect to Modbus device
	modbusMu.Lock()
	defer modbusMu.Unlock()
	if modbusHandler != nil {
		_ = modbusHandler.Close()
	}
	err := handler.Connect()
	modbusHandler = handler
	modbusClient = modbus.NewClient(handler)
	return err
}

// handleModbusError counts a consecutive Modbus failure, exits after
// maxModbusErrors (run.sh restarts the process) and otherwise reconnects.
// Caller must NOT hold modbusMu.
func handleModbusError(err error) {
	n := modbusClientErrorCount.Add(1)
	if n > maxModbusErrors {
		fmt.Fprintf(os.Stderr, "error: %d consecutive Modbus errors, terminating: %v\n", n, err)
		os.Exit(1)
	}
	log.Printf("Modbus error %d/%d, reconnecting: %v", n, maxModbusErrors, err)
	if rerr := modbusReconnect(); rerr != nil {
		log.Printf("Modbus reconnect failed: %v", rerr)
	}
}

// Static list of polled input registers (2 words each)
var polledRegisters = []regDef{
	{"battery_status", 31391},
	{"battery_soc", 30845},
	{"battery_temperature", 30849},
	{"battery_diagnose_current_capacity", 30847},
	{"battery_charge_power", 31393},
	{"battery_discharge_power", 31395},
	{"dc1_current", 30769},
	{"dc1_voltage", 30771},
	{"dc1_power", 30773},
	{"dc2_current", 30957},
	{"dc2_voltage", 30959},
	{"dc2_power", 30961},
	{"ac_power", 30775},
	{"grid_feed", 30867},
	{"grid_draw", 30865},
	{"inverter_temperature", 30953},
}

func modbusReadLoop() {
	// Normal polling ticker and a fast 1s ticker used while in Balanced mode
	normalTicker := time.NewTicker(time.Duration(modbusIntervalInSeconds) * time.Second)
	fastTicker := time.NewTicker(1 * time.Second)
	resetTicker := time.NewTicker(time.Duration(resetIntervalMinutes) * time.Minute) // periodic control logic check
	fullPublishTicker := time.NewTicker(30 * time.Minute)                            // force full sensor publish every 30 minutes

	// Do initial read and apply control logic
	readAndPublishData()
	applyControlLogic()

	for {
		select {
		case <-fastTicker.C:
			// When Balanced overwrite is active AND we're actually in discharge activity, poll every second
			if balancedDischargeActive() {
				readAndPublishData()
				checkPauseChargeOkMode()
			}
		case <-normalTicker.C:
			// In non-Balanced modes (or Balanced but not actively discharging), poll at the configured interval
			if !balancedDischargeActive() {
				readAndPublishData()
				checkPauseChargeOkMode()
			}
		case <-resetTicker.C:
			applyControlLogic()
		case <-fullPublishTicker.C:
			// Clear cache to force publish of all sensors, then read and publish immediately
			resetSensorCache()
			readAndPublishData()
		}
	}
}

// balancedDischargeActive reports whether the Balanced overwrite is actively
// discharging and therefore needs the fast 1s poll:
// - overwrite is Balanced and battery_control > 0
// - not actively charging (battery_charge_power below threshold)
// - and either currently discharging or grid deviates from zero beyond nearZeroW
func balancedDischargeActive() bool {
	controlMu.Lock()
	balanced := normalizeMode(overwriteLogicSelection) == "Balanced"
	bc := batteryControl
	controlMu.Unlock()
	if !balanced || bc <= 0 {
		return false
	}
	sensorMu.Lock()
	defer sensorMu.Unlock()
	return batteryChargePower < chargeActiveW && (batteryDischargePower > 0 || gridDraw > nearZeroW || gridFeed > nearZeroW)
}

func readAndPublishData() {
	readMu.Lock()
	defer readMu.Unlock()
	for _, r := range polledRegisters {
		modbusMu.Lock()
		result, err := modbusClient.ReadInputRegisters(r.addr, 2)
		modbusMu.Unlock()
		if err != nil {
			log.Printf("Error reading %s register: %v", r.name, err)
			// Abort this cycle; the next tick retries after the reconnect
			handleModbusError(err)
			publishModbusErrorCount()
			return
		}
		value := int32(binary.BigEndian.Uint32(result))
		valueFloat := float32(value)

		// Update control variables and apply scaling
		switch r.name {
		case "dc1_current", "dc2_current":
			valueFloat = valueFloat * 0.001
		case "dc1_voltage", "dc2_voltage":
			valueFloat = valueFloat * 0.01
		case "battery_temperature":
			valueFloat = valueFloat * 0.1
		case "inverter_temperature":
			// SMA TEMP format, 1 decimal like battery_temperature (HA history at x0.01 showed 3.0-4.7 "°C")
			valueFloat = valueFloat * 0.1
		case "battery_soc":
			sensorMu.Lock()
			batterySoc = int(value)
			sensorMu.Unlock()
		case "battery_discharge_power":
			sensorMu.Lock()
			batteryDischargePower = int(value)
			sensorMu.Unlock()
		case "battery_charge_power":
			sensorMu.Lock()
			batteryChargePower = int(value)
			sensorMu.Unlock()
		case "grid_feed":
			sensorMu.Lock()
			gridFeed = int(value)
			sensorMu.Unlock()
		case "grid_draw":
			sensorMu.Lock()
			gridDraw = int(value)
			sensorMu.Unlock()
		}

		// Build payload string efficiently and publish only if changed
		var payloadStr string
		if int32(valueFloat) != value {
			// format float with trimming to avoid noisy changes
			payloadStr = strconv.FormatFloat(float64(valueFloat), 'f', 2, 64)
		} else {
			payloadStr = strconv.FormatInt(int64(value), 10)
		}
		stateTopic := sensorTopicPrefix + r.name + "/state"
		if last, ok := lastSensorValues[r.name]; !ok || last != payloadStr {
			lastSensorValues[r.name] = payloadStr
			mqttPublish(stateTopic, []byte(payloadStr), false)
		}
	}

	// Full cycle succeeded: the error counter counts consecutive failures only
	modbusClientErrorCount.Store(0)
	publishModbusErrorCount()
}

// publishModbusErrorCount publishes the error counter if changed. Caller must hold readMu.
func publishModbusErrorCount() {
	stateTopic := sensorTopicPrefix + "modbus_error_count/state"
	payload := strconv.FormatInt(int64(modbusClientErrorCount.Load()), 10)
	if last, ok := lastSensorValues["modbus_error_count"]; !ok || last != payload {
		lastSensorValues["modbus_error_count"] = payload
		mqttPublish(stateTopic, []byte(payload), false)
	}
}

func checkPauseChargeOkMode() {
	// Use normalized mode for case-insensitive comparison
	controlMu.Lock()
	currentMode := getCurrentModeUnsafe()
	normalizedOverwrite := normalizeMode(overwriteLogicSelection)
	// A mode change not yet applied (e.g. a failed write) is retried on the next poll tick
	modePending := currentMode != previousMode
	if currentMode == "Automatic" {
		// No command is re-applied in Automatic, so keep the displayed charge power live here
		sensorMu.Lock()
		chargePower := batteryChargePower
		sensorMu.Unlock()
		if chargePower < 0 { // SMA NaN marker (0x80000000) reads as negative
			chargePower = 0
		}
		publishActualChargingPower(chargePower)
	}
	controlMu.Unlock()

	// Continuously react in Balanced only when Overwrite is actively set to Balanced (not in Automatic mode)
	if normalizedOverwrite == "Balanced" || modePending {
		applyControlLogic()
		return
	}
	// For Pause (charge ok) and Charge Battery modes, always check if we need to switch
	if currentMode == "Pause (charge ok)" || currentMode == "Charge Battery" {
		applyControlLogic()
		return
	}
	sensorMu.Lock()
	localBatteryDischargePower := batteryDischargePower
	sensorMu.Unlock()
	if currentMode == "Pause (charge ok)" && !pauseActivated && localBatteryDischargePower > 0 {
		applyControlLogic()
	}
}

func applyControlLogic() {
	controlMu.Lock()
	defer controlMu.Unlock()
	var spntCom uint32 = 0
	var pwrAtCom int32 = 0

	// Use normalized mode for case-insensitive comparison
	// Note: Using unsafe version since we already hold controlMu
	currentMode := getCurrentModeUnsafe()

	if currentMode != currentLogicSelection {
		currentLogicSelection = currentMode
		// Publish current logic selection as a read-only sensor state
		stateTopic := fmt.Sprintf("homeassistant/sensor/%s/current_logic_selection/state", deviceID)
		mqttPublish(stateTopic, []byte(currentLogicSelection), true)
	}

	// Reset internal automatic state when mode changes
	if currentMode != previousMode {
		setInternalAutomatic(false)
		if debugEnabled {
			log.Printf("Mode changed from %s to %s, resetting internal automatic state", previousMode, currentMode)
		}
	}

	// Apply control logic if:
	// 1. Mode has changed
	// 2. We're not in pure Automatic mode (need to maintain control)
	// Note: Pause (charge ok) and Charge Battery need continuous evaluation for internal switching
	shouldApply := currentMode != previousMode || currentMode != "Automatic"

	if shouldApply {
		if debugEnabled {
			log.Printf("Applying control logic: Mode=%s, Internal=%v", currentMode, internalAutomatic)
		}
		applyMode(currentMode, &spntCom, &pwrAtCom)
	} else {
		// In "Automatic" mode and mode has not changed, do not send commands
		return
	}

	// "No write" must not leave an earlier forced command (802) active on the
	// inverter, e.g. entering Balanced during a forced charge: release it once.
	if spntCom == 0 && lastWrittenSpntCom == controlOn {
		spntCom = controlOff
		pwrAtCom = 0
	}

	if spntCom != 0 {
		// Write control commands to Modbus
		if err := writeControlCommands(spntCom, pwrAtCom); err != nil {
			// previousMode stays unchanged, so the next evaluation retries
			// (also in Automatic, where shouldApply relies on the mode change).
			log.Printf("Control command not applied, will retry: %v", err)
		} else {
			lastWrittenSpntCom = spntCom
			previousMode = currentMode
		}
		// Give inverter a brief moment to apply new settings before reading back
		// In Balanced mode we must react quickly based on grid values: skip the post_command delay
		if currentMode != "Balanced" {
			time.Sleep(time.Duration(postCommandDelayMs) * time.Millisecond)
		}
	} else {
		previousMode = currentMode
	}
	// Always read and publish after evaluating/applying control changes
	readAndPublishData()
}

// publishInternalReason publishes internal_mode_reason if changed. Caller must hold controlMu.
func publishInternalReason(reason string) {
	if internalAutomaticReason != reason {
		internalAutomaticReason = reason
		stateTopic := sensorTopicPrefix + "internal_mode_reason/state"
		mqttPublish(stateTopic, []byte(reason), false)
	}
}

// publishActualChargingPower publishes actual_charging_power_setting if changed. Caller must hold controlMu.
func publishActualChargingPower(power int) {
	if actualChargingPower != power {
		actualChargingPower = power
		stateTopic := sensorTopicPrefix + "actual_charging_power_setting/state"
		mqttPublish(stateTopic, []byte(strconv.Itoa(power)), false)
	}
}

// applyMode decides SpntCom/PwrAtCom for mode. Caller must hold controlMu.
func applyMode(mode string, spntCom *uint32, pwrAtCom *int32) {
	// Read sensor values atomically
	sensorMu.Lock()
	localBatterySoc := batterySoc
	localBatteryChargePower := batteryChargePower
	localBatteryDischargePower := batteryDischargePower
	localGridFeed := gridFeed
	localGridDraw := gridDraw
	sensorMu.Unlock()

	// Helper function to calculate SOC-tapered charging power
	calculateTaperedCharge := func(requestedPower int) int {
		if localBatterySoc <= 80 {
			return requestedPower
		}

		var maxAllowed int
		if localBatterySoc < 85 {
			// 80-85%: 80% of max power
			maxAllowed = int(float64(maximumBatteryControl) * 0.8)
		} else if localBatterySoc < 90 {
			// 85-90%: 60% of max power
			maxAllowed = int(float64(maximumBatteryControl) * 0.6)
		} else if localBatterySoc < 95 {
			// 90-95%: 30% of max power
			maxAllowed = int(float64(maximumBatteryControl) * 0.3)
		} else {
			// 95-100%: 15% of max power
			maxAllowed = int(float64(maximumBatteryControl) * 0.15)
		}

		if requestedPower > maxAllowed {
			return maxAllowed
		}
		return requestedPower
	}

	switch mode {
	case "Pause (charge ok)":
		// Check if we should switch internally to automatic
		if localGridFeed > 100 {
			// Excess PV power available - switch to automatic to allow charging
			if !internalAutomatic {
				setInternalAutomatic(true)
				publishInternalReason("Excess PV detected (grid feed > 100W), allowing automatic charging")
				if debugEnabled {
					log.Printf("Pause (charge ok): Switching internally to automatic - grid feed %dW", localGridFeed)
				}
			}
			*spntCom = controlOff
			*pwrAtCom = 0
			pauseActivated = false
		} else if internalAutomatic && localBatteryDischargePower > 100 {
			// Battery started discharging - switch back to pause
			setInternalAutomatic(false)
			publishInternalReason("Battery discharging detected, reverting to pause")
			if debugEnabled {
				log.Printf("Pause (charge ok): Battery discharging %dW, switching back to pause", localBatteryDischargePower)
			}
			*spntCom = controlOn
			*pwrAtCom = 0
			pauseActivated = true
		} else if !internalAutomatic {
			// Stay in pause mode
			*spntCom = controlOn
			*pwrAtCom = 0
			pauseActivated = true
			publishInternalReason("Pause mode active")
		} else {
			// Stay in internal automatic
			*spntCom = controlOff
			*pwrAtCom = 0
			pauseActivated = false
		}
		publishActualChargingPower(0)
	case "Pause":
		pauseActivated = true
		*spntCom = controlOn
		*pwrAtCom = 0
		setInternalAutomatic(false)
		publishInternalReason("Pause mode active (no charging allowed)")
		publishActualChargingPower(0)
	case "Charge Battery":
		pauseActivated = false
		// Apply SOC-based tapering
		taperedPower := calculateTaperedCharge(batteryControl)
		// Revert threshold: tapered-500 is unreachable for setpoints <= 500 W, so use at least half the setpoint
		revertBelow := taperedPower - 500
		if half := taperedPower / 2; half > revertBelow {
			revertBelow = half
		}

		// Check if we should switch internally to automatic
		if localGridFeed > 500 {
			// Significant excess PV - switch to automatic for optimal charging
			if !internalAutomatic {
				setInternalAutomatic(true)
				publishInternalReason(fmt.Sprintf("Excess PV detected (grid feed %dW > 500W), using automatic charging", localGridFeed))
				if debugEnabled {
					log.Printf("Charge Battery: Switching internally to automatic - grid feed %dW", localGridFeed)
				}
			}
			*spntCom = controlOff
			*pwrAtCom = 0
			// In automatic mode, show what the inverter is doing
			publishActualChargingPower(localBatteryChargePower)
		} else if internalAutomatic && localBatteryChargePower < revertBelow {
			// Charging dropped below threshold - switch back to forced charge
			setInternalAutomatic(false)
			publishInternalReason(fmt.Sprintf("Charging dropped to %dW, forcing charge at %dW", localBatteryChargePower, taperedPower))
			if debugEnabled {
				log.Printf("Charge Battery: Charging only %dW, switching back to forced charge at %dW", localBatteryChargePower, taperedPower)
			}
			*spntCom = controlOn
			*pwrAtCom = -int32(taperedPower)
			publishActualChargingPower(taperedPower)
		} else if !internalAutomatic {
			// Stay in forced charge mode
			*spntCom = controlOn
			*pwrAtCom = -int32(taperedPower)
			if taperedPower < batteryControl {
				publishInternalReason(fmt.Sprintf("Charging at %dW (SOC %d%% limiting to %dW)", taperedPower, localBatterySoc, taperedPower))
			} else {
				publishInternalReason(fmt.Sprintf("Forcing charge at %dW", taperedPower))
			}
			publishActualChargingPower(taperedPower)
		} else {
			// Stay in internal automatic
			*spntCom = controlOff
			*pwrAtCom = 0
			publishActualChargingPower(localBatteryChargePower)
		}
	case "Discharge Battery":
		pauseActivated = false
		*spntCom = controlOn
		*pwrAtCom = int32(batteryControl)
		setInternalAutomatic(false)
		publishInternalReason(fmt.Sprintf("Forcing discharge at %dW", batteryControl))
		publishActualChargingPower(0)
	case "Balanced":
		// Only send Balanced commands when Overwrite is actively set to Balanced; otherwise do nothing (no writes)
		if normalizeMode(overwriteLogicSelection) != "Balanced" {
			*spntCom = 0
			*pwrAtCom = 0
			if debugEnabled {
				log.Println("Balanced logic ignored because we are in Automatic mode")
			}
			break
		}
		// If we are actively charging and grid is near zero, stay automatic: do not send discharge commands
		if localBatteryChargePower >= chargeActiveW {
			*spntCom = 0
			*pwrAtCom = 0
			if debugEnabled {
				log.Printf("Balanced: charging (charge_power=%dW) with grid near 0 (draw=%d, feed=%d) → stay automatic", localBatteryChargePower, localGridDraw, localGridFeed)
			}
			break
		}

		// If battery_control is 0 (either just became 0 or stayed 0), treat as internal Automatic: do not send Modbus commands
		if batteryControl == 0 {
			*spntCom = 0
			*pwrAtCom = 0
			if debugEnabled {
				log.Println("Balanced: battery_control is 0 → internal Automatic, no Modbus commands")
			}
			break
		}
		// Balanced logic (discharge-only commands) with dynamic battery_control adjustment:
		// - If grid_draw == 0 and battery_discharge_power == 0: set battery_control to 0 and do not write (internal Automatic)
		// - If grid_draw > 0: increase battery_control by grid_draw (clamped) and discharge with that value
		// - If grid_draw == 0 and grid_feed > 0: decrease battery_control by grid_feed; if <=0 set to 0 and do not write
		if localGridDraw == 0 && localBatteryDischargePower == 0 {
			if batteryControl != 0 {
				batteryControl = 0
				lastValidBatteryControl = 0
				stateTopic := fmt.Sprintf("homeassistant/number/%s/%s/state", deviceID, "battery_control")
				mqttPublish(stateTopic, []byte("0"), true)
			}
			*spntCom = 0
			*pwrAtCom = 0
		} else if localGridDraw > 0 {
			newBC := batteryControl + localGridDraw
			if newBC > maximumBatteryControl {
				newBC = maximumBatteryControl
			}
			if newBC != batteryControl {
				batteryControl = newBC
				lastValidBatteryControl = newBC
				stateTopic := fmt.Sprintf("homeassistant/number/%s/%s/state", deviceID, "battery_control")
				mqttPublish(stateTopic, []byte(strconv.Itoa(newBC)), true)
			}
			*spntCom = controlOn
			*pwrAtCom = int32(newBC)
		} else if localGridFeed > 0 { // gridDraw == 0 implied here
			newBC := batteryControl - localGridFeed
			if newBC > 0 {
				if newBC != batteryControl {
					batteryControl = newBC
					lastValidBatteryControl = newBC
					stateTopic := fmt.Sprintf("homeassistant/number/%s/%s/state", deviceID, "battery_control")
					mqttPublish(stateTopic, []byte(strconv.Itoa(newBC)), true)
				}
				*spntCom = controlOn
				*pwrAtCom = int32(newBC)
			} else {
				// Going to zero or below: set to 0 and do not write (internal Automatic)
				if batteryControl != 0 {
					batteryControl = 0
					lastValidBatteryControl = 0
					stateTopic := fmt.Sprintf("homeassistant/number/%s/%s/state", deviceID, "battery_control")
					mqttPublish(stateTopic, []byte("0"), true)
				}
				*spntCom = 0
				*pwrAtCom = 0
			}
		} else {
			// Fallback: no decisive grid change detected, do not write
			*spntCom = 0
			*pwrAtCom = 0
		}
		// Balanced mode doesn't use internal automatic switching
		setInternalAutomatic(false)
		publishInternalReason("Balanced mode active")
		publishActualChargingPower(0)
	default: // Automatic
		pauseActivated = false
		*spntCom = controlOff
		*pwrAtCom = 0
		setInternalAutomatic(false)
		publishInternalReason("Automatic mode active")
		// In automatic mode, show actual charging if happening
		if localBatteryChargePower > 0 {
			publishActualChargingPower(localBatteryChargePower)
		} else {
			publishActualChargingPower(0)
		}
	}
}

// writeControlCommands writes SpntCom (40151) and PwrAtCom (40149). A non-nil
// error means the command is not (fully) applied and must be retried.
func writeControlCommands(spntCom uint32, pwrAtCom int32) error {
	modbusMu.Lock()
	// Write to register 40151 (Communication control)
	spntComData := uint32ToBytes(spntCom)
	if debugEnabled {
		log.Printf("Writing to register 40151: %v", spntComData)
	}
	_, err := modbusClient.WriteMultipleRegisters(40151, 2, spntComData)
	if err != nil {
		modbusMu.Unlock() // Unlock before reconnect to avoid deadlock
		log.Printf("Error writing to register 40151: %v", err)
		handleModbusError(err)
		return err
	}
	time.Sleep(100 * time.Millisecond)

	// Write to register 40149 (Power command)
	pwrAtComData := int32ToBytes(pwrAtCom)
	if debugEnabled {
		log.Printf("Writing to register 40149: %v", pwrAtComData)
	}
	_, err = modbusClient.WriteMultipleRegisters(40149, 2, pwrAtComData)
	modbusMu.Unlock() // Unlock after both writes complete or on error
	if err != nil {
		log.Printf("Error writing to register 40149: %v", err)
		handleModbusError(err)
		return err
	}
	if debugEnabled {
		log.Printf("Control command sent: SpntCom=%d, PwrAtCom=%d", spntCom, pwrAtCom)
	}
	return nil
}

func loadInitialSettings() {
	// Subscribe to retained state topics
	autoTopic := fmt.Sprintf("homeassistant/select/%s/automatic_logic_selection/state", deviceID)
	token := mqttClient.Subscribe(autoTopic, 0, func(client mqtt.Client, msg mqtt.Message) {
		controlMu.Lock()
		automaticLogicSelection = string(msg.Payload())
		automaticSelectionLoaded = true
		controlMu.Unlock()
		if debugEnabled {
			log.Printf("Loaded automatic_logic_selection from MQTT: %s", msg.Payload())
		}
	})
	token.Wait()

	overTopic := fmt.Sprintf("homeassistant/select/%s/overwrite_logic_selection/state", deviceID)
	token = mqttClient.Subscribe(overTopic, 0, func(client mqtt.Client, msg mqtt.Message) {
		controlMu.Lock()
		overwriteLogicSelection = string(msg.Payload())
		overwriteSelectionLoaded = true
		controlMu.Unlock()
		if debugEnabled {
			log.Printf("Loaded overwrite_logic_selection from MQTT: %s", msg.Payload())
		}
	})
	token.Wait()

	bcTopic := fmt.Sprintf("homeassistant/number/%s/battery_control/state", deviceID)
	token = mqttClient.Subscribe(bcTopic, 0, func(client mqtt.Client, msg mqtt.Message) {
		value, err := strconv.Atoi(string(msg.Payload()))
		if err == nil {
			controlMu.Lock()
			batteryControl = value
			batteryControlLoaded = true
			controlMu.Unlock()
		}
		if debugEnabled {
			log.Printf("Loaded battery_control from MQTT: %s", msg.Payload())
		}
	})
	token.Wait()

	// Give retained messages time to arrive
	// This is still not perfect but better than the original
	time.Sleep(1 * time.Second)

	// Stop listening: later echoes of our own retained publishes must not overwrite the globals
	if token = mqttClient.Unsubscribe(autoTopic, overTopic, bcTopic); token.Wait() && token.Error() != nil {
		log.Printf("[MQTT] Error unsubscribing startup state topics: %v", token.Error())
	}

	controlMu.Lock()
	normalizeLoadedSettings()
	controlMu.Unlock()
}

// normalizeLoadedSettings applies defaults, mode normalization and the
// battery_control clamp to the values loaded from retained MQTT state.
// Caller must hold controlMu.
func normalizeLoadedSettings() {
	if !automaticSelectionLoaded {
		automaticLogicSelection = "Automatic"
	}
	automaticLogicSelection = normalizeMode(automaticLogicSelection)
	if automaticLogicSelection == "Off" { // "Off" is only valid for the overwrite select
		automaticLogicSelection = "Automatic"
	}
	if !overwriteSelectionLoaded || strings.TrimSpace(overwriteLogicSelection) == "" {
		overwriteLogicSelection = "Off"
	}
	overwriteLogicSelection = normalizeMode(overwriteLogicSelection)

	if !batteryControlLoaded {
		// Set default battery control to 90% of maximumBatteryControl
		batteryControl = int(math.Round(float64(maximumBatteryControl) * 0.90))
	}
	if batteryControl < 0 {
		batteryControl = 0
	} else if batteryControl > maximumBatteryControl {
		batteryControl = maximumBatteryControl
	}
	lastValidBatteryControl = batteryControl
}

func mqttMessageHandler(client mqtt.Client, msg mqtt.Message) {
	topicLevels := strings.Split(msg.Topic(), "/")
	if len(topicLevels) < 5 {
		return
	}
	entityType := topicLevels[1]
	deviceID := topicLevels[2]
	objectID := topicLevels[3]
	action := topicLevels[4]

	payload := string(msg.Payload())

	if debugEnabled {
		log.Printf("Received MQTT message on %s: %s", msg.Topic(), payload)
	}

	if action != "set" {
		return
	}

	switch entityType {
	case "select":
		if objectID == "automatic_logic_selection" {
			// Normalize the mode for case-insensitive matching
			normalizedPayload := normalizeMode(payload)
			// Protect write access to global variable
			controlMu.Lock()
			automaticLogicSelection = normalizedPayload
			controlMu.Unlock()
			stateTopic := fmt.Sprintf("homeassistant/select/%s/%s/state", deviceID, objectID)
			mqttPublish(stateTopic, []byte(normalizedPayload), true)
			applyControlLogic()
		} else if objectID == "overwrite_logic_selection" {
			// Normalize the mode for case-insensitive matching
			normalizedPayload := normalizeMode(payload)
			// Save/restore, Balanced init and the overwrite switch happen in ONE
			// controlMu section, so a poll tick never sees a half-applied change.
			controlMu.Lock()
			wasBalanced := normalizeMode(overwriteLogicSelection) == "Balanced"
			if normalizedPayload == "Balanced" && !wasBalanced {
				// Balanced rewrites battery_control dynamically: remember the user's value
				savedUserBatteryControl = batteryControl
				savedUserBatteryControlValid = true
			}
			publishBC := -1
			if normalizedPayload != "Balanced" && wasBalanced && savedUserBatteryControlValid {
				batteryControl = savedUserBatteryControl
				lastValidBatteryControl = savedUserBatteryControl
				savedUserBatteryControlValid = false
				publishBC = batteryControl
			}
			// If switching to Balanced, initialize battery_control to 0 or the required discharge value
			if normalizedPayload == "Balanced" {
				var newBC int
				sensorMu.Lock()
				localBatteryChargePower := batteryChargePower
				localGridDraw := gridDraw
				localGridFeed := gridFeed
				sensorMu.Unlock()
				if localBatteryChargePower >= chargeActiveW && localGridDraw <= nearZeroW && localGridFeed <= nearZeroW {
					newBC = 0
				} else if localGridDraw > 0 {
					newBC = localGridDraw
					if newBC > maximumBatteryControl {
						newBC = maximumBatteryControl
					}
				} else {
					newBC = 0
				}
				if batteryControl != newBC {
					batteryControl = newBC
					lastValidBatteryControl = newBC
					publishBC = newBC
				}
			}
			overwriteLogicSelection = normalizedPayload
			controlMu.Unlock()
			if publishBC >= 0 {
				bcStateTopic := fmt.Sprintf("homeassistant/number/%s/%s/state", deviceID, "battery_control")
				mqttPublish(bcStateTopic, []byte(strconv.Itoa(publishBC)), true)
			}
			stateTopic := fmt.Sprintf("homeassistant/select/%s/%s/state", deviceID, objectID)
			mqttPublish(stateTopic, []byte(normalizedPayload), true)
			applyControlLogic()
		}
	case "number":
		if objectID == "battery_control" {
			value, err := strconv.Atoi(payload)
			if err == nil && value >= 0 && value <= maximumBatteryControl {
				// Check current mode - ignore battery_control changes in Pause modes
				currentMode := getCurrentMode()
				if currentMode == "Pause (charge ok)" || currentMode == "Pause" {
					// In Pause modes, battery_control changes are ignored
					// Keep the value stored but don't apply it
					controlMu.Lock()
					batteryControl = value
					lastValidBatteryControl = value
					controlMu.Unlock()
					stateTopic := fmt.Sprintf("homeassistant/number/%s/%s/state", deviceID, objectID)
					mqttPublish(stateTopic, []byte(payload), true)
					if debugEnabled {
						log.Printf("Battery control set to %d but ignored in %s mode", value, currentMode)
					}
					// Don't call applyControlLogic() - the value is stored but not applied
					return
				}
				// Protect write access to global variables
				controlMu.Lock()
				batteryControl = value
				lastValidBatteryControl = value
				controlMu.Unlock()
				stateTopic := fmt.Sprintf("homeassistant/number/%s/%s/state", deviceID, objectID)
				mqttPublish(stateTopic, []byte(payload), true)
				applyControlLogic()
			} else {
				// Reset to last valid value
				controlMu.Lock()
				lastValid := lastValidBatteryControl
				controlMu.Unlock()
				stateTopic := fmt.Sprintf("homeassistant/number/%s/%s/state", deviceID, objectID)
				mqttPublish(stateTopic, []byte(strconv.Itoa(lastValid)), true)
				if debugEnabled {
					log.Printf("Invalid battery control value: %s. Resetting to last valid value: %d", payload, lastValid)
				}
			}
		}
	}
}

// mqttPublish is a package-level var (not a plain func) so tests can inject a
// capturing stub instead of talking to a real MQTT broker.
var mqttPublish = func(topic string, payload []byte, retain bool) {
	token := mqttClient.Publish(topic, 0, retain, payload)
	// For retained/config messages we wait; for high-frequency telemetry we don't block
	if retain || debugEnabled {
		token.Wait()
	} else {
		// non-blocking publish; let the client handle delivery
		go func() { _ = token.Wait() }()
	}
	if debugEnabled {
		log.Printf("Published MQTT message to %s: %s", topic, payload)
	}
}

func getEnv(key, defaultValue string) string {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}
	return value
}

func uint32ToBytes(value uint32) []byte {
	bytes := make([]byte, 4)
	binary.BigEndian.PutUint32(bytes, value)
	return bytes
}

func int32ToBytes(value int32) []byte {
	bytes := make([]byte, 4)
	binary.BigEndian.PutUint32(bytes, uint32(value))
	return bytes
}

// normalizeMode converts a mode string to its canonical form (case-insensitive matching)
// Returns "Automatic" for invalid/unknown modes as a safe fallback
func normalizeMode(mode string) string {
	lower := strings.ToLower(strings.TrimSpace(mode))
	switch lower {
	case "automatic", "":
		return "Automatic"
	case "balanced":
		return "Balanced"
	case "pause (charge ok)", "pause(charge ok)", "pause (chargeok)", "pause(chargeok)":
		return "Pause (charge ok)"
	case "pause":
		return "Pause"
	case "charge battery", "chargebattery":
		return "Charge Battery"
	case "discharge battery", "dischargebattery":
		return "Discharge Battery"
	case "off":
		return "Off"
	default:
		// Unknown mode - fallback to Automatic for safety
		// Always log this warning, not just in debug mode
		log.Printf("WARNING: Unknown mode '%s' received, falling back to 'Automatic'", mode)
		return "Automatic"
	}
}

// getCurrentModeUnsafe returns the effective current mode (normalized)
// NOT thread-safe: caller must hold controlMu lock
func getCurrentModeUnsafe() string {
	if overwriteLogicSelection != "Off" && overwriteLogicSelection != "" {
		return normalizeMode(overwriteLogicSelection)
	}
	return normalizeMode(automaticLogicSelection)
}

// getCurrentMode returns the effective current mode (normalized)
// Thread-safe: uses controlMu to protect access to global mode variables
func getCurrentMode() string {
	controlMu.Lock()
	defer controlMu.Unlock()
	return getCurrentModeUnsafe()
}
