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
	modbusClientErrorCount  int
	modbusClientErrorTime   time.Time
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
	previousMode            string
	deviceID                string
	resetIntervalMinutes    int       // Reset interval
	lastChangeTime          time.Time // Last change timestamp
	initialValuesLoaded     bool      // Track if values are loaded
	acPower                 int
	gridDraw                int
	gridFeed                int
	pauseActivated          bool
	postCommandDelayMs      int // Delay after write before readback
	batterySoc              int // Battery State of Charge

	// Internal automatic switching state
	internalAutomatic       bool   // True when internally switched to automatic
	internalAutomaticReason string // Reason for internal switching
	actualChargingPower     int    // Actual charging power being commanded (after SOC tapering)

	// Flags for retained state loading
	batteryControlLoaded     bool
	automaticSelectionLoaded bool
	overwriteSelectionLoaded bool

	// Synchronization primitives to prevent Modbus command interference
	modbusMu  sync.Mutex
	controlMu sync.Mutex
	sensorMu  sync.Mutex // Protects sensor value globals

	// Cached topic prefixes
	sensorTopicPrefix      string
	selectStateTopicPrefix string
	numberStateTopicPrefix string

	// Cache of last published sensor values to avoid redundant publishes
	lastSensorValues map[string]string
)

const (
	nearZeroW     = 30  // threshold to treat grid draw/feed as ~0
	chargeActiveW = 500 // threshold to consider charging actively happening
)

func main() {
	modbusClientErrorCount = 0
	modbusClientErrorTime = time.Now()

	// Load environment variables
	loadConfig()

	// Set up MQTT client
	setupMQTT()

	// Load initial settings from MQTT
	loadInitialSettings()

	// Publish MQTT discovery messages
	publishDiscoveryMessages()

	// Set up Modbus client
	setupModbus()

	// Start Modbus reading loop
	go modbusReadLoop()

	// Listen for MQTT messages
	listenTopic := fmt.Sprintf("homeassistant/+/%s/+/set", deviceID)
	token := mqttClient.Subscribe(listenTopic, 0, mqttMessageHandler)
	token.Wait()
	if debugEnabled {
		log.Printf("Subscribed to: %s", listenTopic)
	}

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
	lastChangeTime = time.Now()

	// Precompute topic prefixes and initialize caches
	sensorTopicPrefix = "homeassistant/sensor/" + deviceID + "/"
	selectStateTopicPrefix = "homeassistant/select/" + deviceID + "/"
	numberStateTopicPrefix = "homeassistant/number/" + deviceID + "/"
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

	// Set Last Will and Testament (LWT)
	willTopic := "smastp_modbus/status"
	willPayload := "offline"
	opts.SetWill(willTopic, willPayload, 0, true)

	// Publish birth message after connection
	opts.OnConnect = func(c mqtt.Client) {
		birthTopic := "smastp_modbus/status"
		birthPayload := "online"
		token := c.Publish(birthTopic, 0, true, birthPayload)
		token.Wait()
		if debugEnabled {
			log.Println("Published birth message to", birthTopic)
		}
	}

	// Create and start MQTT client
	mqttClient = mqtt.NewClient(opts)
	if token := mqttClient.Connect(); token.Wait() && token.Error() != nil {
		log.Fatalf("MQTT connection error: %v", token.Error())
	}
}

func publishDiscoveryMessages() {
	// Device information
	deviceInfo := map[string]interface{}{
		"identifiers":  []string{deviceID},
		"manufacturer": "Custom",
		"model":        "SMA Battery Controller",
		"name":         "SMA Battery Controller",
	}

	// Always publish discovery for selects and number so HA can send commands
	publishSelect("automatic_logic_selection", "Automatic Logic Selection", []string{"Automatic", "Balanced", "Pause (charge ok)", "Pause", "Charge Battery", "Discharge Battery"}, automaticLogicSelection, deviceInfo)
	publishSelect("overwrite_logic_selection", "Overwrite Logic Selection", []string{"Off", "Automatic", "Balanced", "Pause (charge ok)", "Pause", "Charge Battery", "Discharge Battery"}, overwriteLogicSelection, deviceInfo)
	// Make Current Logic Selection read-only by publishing as a sensor (no command topic)
	publishSensor("current_logic_selection", "Current Logic Selection", "", deviceInfo)
	// Remove old select-based Current Logic Selection entity by clearing its discovery and state
	oldSelectConfigTopic := fmt.Sprintf("homeassistant/select/%s/current_logic_selection/config", deviceID)
	mqttPublish(oldSelectConfigTopic, []byte(""), true)
	oldSelectStateTopic := fmt.Sprintf("homeassistant/select/%s/current_logic_selection/state", deviceID)
	mqttPublish(oldSelectStateTopic, []byte(""), true)

	publishNumber("battery_control", "Battery Control", 0, float64(maximumBatteryControl), 100, float64(batteryControl), deviceInfo)

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
	publishSensor("dc2_current", "DC1 Current", "A", deviceInfo)
	publishSensor("dc2_voltage", "DC1 Voltage", "V", deviceInfo)
	publishSensor("dc2_power", "DC1 Power", "W", deviceInfo)
	publishSensor("ac_power", "AC Power", "W", deviceInfo)
	publishSensor("grid_feed", "Grid Feed Power", "W", deviceInfo)
	publishSensor("grid_draw", "Grid Draw Power", "W", deviceInfo)
	publishSensor("modbus_error_count", "Modbus Error Count", "", deviceInfo)
	publishSensor("internal_mode_reason", "Internal Mode Reason", "", deviceInfo)
	publishSensor("actual_charging_power_setting", "Actual Charging Power Setting", "W", deviceInfo)

	// Publish initial values for the new sensors
	stateTopic := sensorTopicPrefix + "internal_mode_reason/state"
	mqttPublish(stateTopic, []byte("Starting"), false)
	stateTopic = sensorTopicPrefix + "actual_charging_power_setting/state"
	mqttPublish(stateTopic, []byte("0"), false)
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
				"topic":       "smastp_modbus/status",
				"payload_on":  "online",
				"payload_off": "offline",
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
				"topic":       "smastp_modbus/status",
				"payload_on":  "online",
				"payload_off": "offline",
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
				"topic":       "smastp_modbus/status",
				"payload_on":  "online",
				"payload_off": "offline",
			},
		},
	}

	payloadBytes, _ := json.Marshal(configPayload)
	mqttPublish(configTopic, payloadBytes, true)
}

func setupModbus() {
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
	err := handler.Connect()
	if err != nil {
		modbusMu.Unlock()
		log.Fatalf("Modbus connection error: %v", err)
	}
	modbusClient = modbus.NewClient(handler)
	modbusMu.Unlock()
	currentTime := time.Now()
	timeDiff := currentTime.Sub(modbusClientErrorTime)
	if timeDiff > 30*time.Minute {
		modbusClientErrorCount = 0
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
			if overwriteLogicSelection == "Balanced" {
				// Active balanced discharge mode if:
				// - battery_control > 0
				// - not actively charging (battery_charge_power below threshold)
				// - and either currently discharging or grid deviates from zero beyond nearZeroW
				sensorMu.Lock()
				localBatteryChargePower := batteryChargePower
				localBatteryDischargePower := batteryDischargePower
				localGridDraw := gridDraw
				localGridFeed := gridFeed
				sensorMu.Unlock()
				if batteryControl > 0 && localBatteryChargePower < chargeActiveW && (localBatteryDischargePower > 0 || localGridDraw > nearZeroW || localGridFeed > nearZeroW) {
					readAndPublishData()
					checkPauseChargeOkMode()
				}
			}
		case <-normalTicker.C:
			// In non-Balanced modes (or Balanced but not actively discharging), poll at the configured interval
			sensorMu.Lock()
			localBatteryChargePower := batteryChargePower
			localBatteryDischargePower := batteryDischargePower
			localGridDraw := gridDraw
			localGridFeed := gridFeed
			sensorMu.Unlock()
			if overwriteLogicSelection != "Balanced" || !(batteryControl > 0 && localBatteryChargePower < chargeActiveW && (localBatteryDischargePower > 0 || localGridDraw > nearZeroW || localGridFeed > nearZeroW)) {
				readAndPublishData()
				checkPauseChargeOkMode()
			}
		case <-resetTicker.C:
			applyControlLogic()
		case <-fullPublishTicker.C:
			// Clear cache to force publish of all sensors, then read and publish immediately
			lastSensorValues = make(map[string]string, len(polledRegisters)+1)
			readAndPublishData()
		}
	}
}

func readAndPublishData() {
	for _, r := range polledRegisters {
		modbusMu.Lock()
		result, err := modbusClient.ReadInputRegisters(r.addr, 2)
		modbusMu.Unlock()
		if err != nil {
			if debugEnabled {
				log.Printf("Error reading %s register: %v", r.name, err)
			}
			modbusClientErrorCount++
			modbusClientErrorTime = time.Now()
			if modbusClientErrorCount < 20 {
				log.Printf("Trying to reconnect because of %v", err)
				time.Sleep(30 * time.Second)
				setupModbus()
			} else if modbusClientErrorCount > 20 {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				os.Exit(1)
			}
			continue
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
			valueFloat = valueFloat * 0.01
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
		case "ac_power":
			sensorMu.Lock()
			acPower = int(value)
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

	// Publish modbus error count
	stateTopic := sensorTopicPrefix + "modbus_error_count/state"
	payload := strconv.FormatInt(int64(modbusClientErrorCount), 10)
	if last, ok := lastSensorValues["modbus_error_count"]; !ok || last != payload {
		lastSensorValues["modbus_error_count"] = payload
		mqttPublish(stateTopic, []byte(payload), false)
	}
}

func checkPauseChargeOkMode() {
	var currentMode string
	if overwriteLogicSelection != "Off" {
		currentMode = overwriteLogicSelection
	} else {
		currentMode = automaticLogicSelection
	}
	// Continuously react in Balanced only when Overwrite is actively set to Balanced (not in Automatic mode)
	if overwriteLogicSelection == "Balanced" {
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
	var currentMode string

	if overwriteLogicSelection != "Off" {
		currentMode = overwriteLogicSelection
	} else {
		currentMode = automaticLogicSelection
	}

	if currentMode != currentLogicSelection {
		currentLogicSelection = currentMode
		// Publish current logic selection as a read-only sensor state
		stateTopic := fmt.Sprintf("homeassistant/sensor/%s/current_logic_selection/state", deviceID)
		mqttPublish(stateTopic, []byte(currentLogicSelection), true)
	}

	// Reset internal automatic state when mode changes
	if currentMode != previousMode {
		internalAutomatic = false
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

	previousMode = currentMode

	if spntCom != 0 {
		// Write control commands to Modbus
		writeControlCommands(spntCom, pwrAtCom)
		// Give inverter a brief moment to apply new settings before reading back
		// In Balanced mode we must react quickly based on grid values: skip the post_command delay
		if currentMode != "Balanced" {
			time.Sleep(time.Duration(postCommandDelayMs) * time.Millisecond)
		}
	}
	// Always read and publish after evaluating/applying control changes
	readAndPublishData()
}

func applyMode(mode string, spntCom *uint32, pwrAtCom *int32) {
	const (
		controlOn  uint32 = 802
		controlOff uint32 = 803
	)

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

	// Helper function to publish internal mode reason
	publishInternalReason := func(reason string) {
		if internalAutomaticReason != reason {
			internalAutomaticReason = reason
			stateTopic := sensorTopicPrefix + "internal_mode_reason/state"
			mqttPublish(stateTopic, []byte(reason), false)
		}
	}

	// Helper function to publish actual charging power
	publishActualChargingPower := func(power int) {
		if actualChargingPower != power {
			actualChargingPower = power
			stateTopic := sensorTopicPrefix + "actual_charging_power_setting/state"
			mqttPublish(stateTopic, []byte(strconv.Itoa(power)), false)
		}
	}

	switch mode {
	case "Pause (charge ok)":
		// Check if we should switch internally to automatic
		if localGridFeed > 100 {
			// Excess PV power available - switch to automatic to allow charging
			if !internalAutomatic {
				internalAutomatic = true
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
			internalAutomatic = false
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
		internalAutomatic = false
		publishInternalReason("Pause mode active (no charging allowed)")
		publishActualChargingPower(0)
	case "Charge Battery":
		pauseActivated = false
		// Apply SOC-based tapering
		taperedPower := calculateTaperedCharge(batteryControl)

		// Check if we should switch internally to automatic
		if localGridFeed > 500 {
			// Significant excess PV - switch to automatic for optimal charging
			if !internalAutomatic {
				internalAutomatic = true
				publishInternalReason(fmt.Sprintf("Excess PV detected (grid feed %dW > 500W), using automatic charging", localGridFeed))
				if debugEnabled {
					log.Printf("Charge Battery: Switching internally to automatic - grid feed %dW", localGridFeed)
				}
			}
			*spntCom = controlOff
			*pwrAtCom = 0
			// In automatic mode, show what the inverter is doing
			publishActualChargingPower(localBatteryChargePower)
		} else if internalAutomatic && localBatteryChargePower < (taperedPower-500) {
			// Charging dropped below threshold - switch back to forced charge
			internalAutomatic = false
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
		internalAutomatic = false
		publishInternalReason(fmt.Sprintf("Forcing discharge at %dW", batteryControl))
		publishActualChargingPower(0)
	case "Balanced":
		// Only send Balanced commands when Overwrite is actively set to Balanced; otherwise do nothing (no writes)
		if overwriteLogicSelection != "Balanced" {
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
		internalAutomatic = false
		publishInternalReason("Balanced mode active")
		publishActualChargingPower(0)
	default: // Automatic
		pauseActivated = false
		*spntCom = controlOff
		*pwrAtCom = 0
		internalAutomatic = false
		publishInternalReason("Automatic mode active")
		// In automatic mode, show actual charging if happening
		if localBatteryChargePower > 0 {
			publishActualChargingPower(localBatteryChargePower)
		} else {
			publishActualChargingPower(0)
		}
	}
}

func writeControlCommands(spntCom uint32, pwrAtCom int32) {
	modbusMu.Lock()
	// Write to register 40151 (Communication control)
	spntComData := uint32ToBytes(spntCom)
	if debugEnabled {
		log.Printf("Writing to register 40151: %v", spntComData)
	}
	_, err := modbusClient.WriteMultipleRegisters(40151, 2, spntComData)
	if err != nil {
		modbusMu.Unlock() // Unlock before setupModbus to avoid deadlock
		log.Printf("Error writing to register 40151: %v", err)
		modbusClientErrorCount++
		modbusClientErrorTime = time.Now()
		if modbusClientErrorCount < 5 {
			time.Sleep(30 * time.Second)
			setupModbus()
		} else {
			log.Fatalf("To many modbus errors, have to terminate %v", err)
		}
		return
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
		modbusClientErrorCount++
		modbusClientErrorTime = time.Now()
		if modbusClientErrorCount < 5 {
			time.Sleep(30 * time.Second)
			setupModbus()
		} else {
			log.Fatalf("To many modbus errors, have to terminate %v", err)
		}
		return
	}
	if debugEnabled {
		log.Printf("Control command sent: SpntCom=%d, PwrAtCom=%d", spntCom, pwrAtCom)
	}
}

func loadInitialSettings() {
	// Subscribe to retained state topics
	stateTopic := fmt.Sprintf("homeassistant/select/%s/automatic_logic_selection/state", deviceID)
	token := mqttClient.Subscribe(stateTopic, 0, func(client mqtt.Client, msg mqtt.Message) {
		automaticLogicSelection = string(msg.Payload())
		automaticSelectionLoaded = true
		if debugEnabled {
			log.Printf("Loaded automatic_logic_selection from MQTT: %s", automaticLogicSelection)
		}
	})
	token.Wait()

	stateTopic = fmt.Sprintf("homeassistant/select/%s/overwrite_logic_selection/state", deviceID)
	token = mqttClient.Subscribe(stateTopic, 0, func(client mqtt.Client, msg mqtt.Message) {
		overwriteLogicSelection = string(msg.Payload())
		overwriteSelectionLoaded = true
		if debugEnabled {
			log.Printf("Loaded overwrite_logic_selection from MQTT: %s", overwriteLogicSelection)
		}
	})
	token.Wait()

	stateTopic = fmt.Sprintf("homeassistant/number/%s/battery_control/state", deviceID)
	token = mqttClient.Subscribe(stateTopic, 0, func(client mqtt.Client, msg mqtt.Message) {
		value, err := strconv.Atoi(string(msg.Payload()))
		if err == nil {
			batteryControl = value
			lastValidBatteryControl = value
			batteryControlLoaded = true
		}
		if debugEnabled {
			log.Printf("Loaded battery_control from MQTT: %d", batteryControl)
		}
	})
	token.Wait()

	// Give retained messages time to arrive
	// This is still not perfect but better than the original
	time.Sleep(1 * time.Second)

	// Set defaults if no values are loaded from retained messages
	if !automaticSelectionLoaded || automaticLogicSelection == "" {
		automaticLogicSelection = "Automatic"
	}
	if !overwriteSelectionLoaded || overwriteLogicSelection == "" {
		overwriteLogicSelection = "Off"
	}
	if !batteryControlLoaded {
		// Set default battery control to 90% of maximumBatteryControl
		batteryControl = int(math.Round(float64(maximumBatteryControl) * 0.90))
		lastValidBatteryControl = batteryControl
	}

	initialValuesLoaded = true // Mark that initial values have been loaded
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
			automaticLogicSelection = payload
			stateTopic := fmt.Sprintf("homeassistant/select/%s/%s/state", deviceID, objectID)
			mqttPublish(stateTopic, []byte(payload), true)
			applyControlLogic()
			lastChangeTime = time.Now()
		} else if objectID == "overwrite_logic_selection" {
			// If switching to Balanced, initialize battery_control to 0 or the required discharge value
			if payload == "Balanced" {
				// Determine initial battery control for Balanced
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
					bcStateTopic := fmt.Sprintf("homeassistant/number/%s/%s/state", deviceID, "battery_control")
					mqttPublish(bcStateTopic, []byte(strconv.Itoa(newBC)), true)
				}
			}
			overwriteLogicSelection = payload
			stateTopic := fmt.Sprintf("homeassistant/select/%s/%s/state", deviceID, objectID)
			mqttPublish(stateTopic, []byte(payload), true)
			applyControlLogic()
			lastChangeTime = time.Now()
		}
	case "number":
		if objectID == "battery_control" {
			value, err := strconv.Atoi(payload)
			if err == nil && value >= 0 && value <= maximumBatteryControl {
				batteryControl = value
				lastValidBatteryControl = value
				stateTopic := fmt.Sprintf("homeassistant/number/%s/%s/state", deviceID, objectID)
				mqttPublish(stateTopic, []byte(payload), true)
				applyControlLogic()
				lastChangeTime = time.Now()
			} else {
				// Reset to last valid value
				stateTopic := fmt.Sprintf("homeassistant/number/%s/%s/state", deviceID, objectID)
				mqttPublish(stateTopic, []byte(strconv.Itoa(lastValidBatteryControl)), true)
				if debugEnabled {
					log.Printf("Invalid battery control value: %s. Resetting to last valid value: %d", payload, lastValidBatteryControl)
				}
			}
		}
	}
}

func mqttPublish(topic string, payload []byte, retain bool) {
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
