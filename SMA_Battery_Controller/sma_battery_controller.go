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
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	modbus "github.com/goburrow/modbus"
)

var (
	mqttClient              mqtt.Client
	modbusClient            modbus.Client
	maximumBatteryControl   int
	modbusIntervalInSeconds int
	debugEnabled            bool
	automaticLogicSelection string
	overwriteLogicSelection string
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

	// Set up Modbus client
	setupModbus()

	// Start Modbus reading loop
	go modbusReadLoop()

	// Listen for MQTT messages
	mqttClient.Subscribe("homeassistant/+/sma_battery_controller/+/set", 0, mqttMessageHandler)

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

	deviceID = "sma_battery_controller"

	// Initialize control variables
	automaticLogicSelection = "Automatic"
	overwriteLogicSelection = "Automatic"
	lastValidBatteryControl = 0
	previousMode = ""
	lastChangeTime = time.Now()
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

	// Publish entities only if defaults are still in use
	if automaticLogicSelection == "Automatic" {
		publishSelect("automatic_logic_selection", "Automatic Logic Selection", []string{"Automatic", "Pause (charge ok)", "Pause", "Charge Battery", "Discharge Battery"}, automaticLogicSelection, deviceInfo)
	}

	if overwriteLogicSelection == "Automatic" {
		publishSelect("overwrite_logic_selection", "Overwrite Logic Selection", []string{"Automatic", "Pause (charge ok)", "Pause", "Charge Battery", "Discharge Battery"}, overwriteLogicSelection, deviceInfo)
	}

	if batteryControl == 0 {
		batteryControl = int(math.Round(float64(maximumBatteryControl) * 0.90)) // 90% of max control
		lastValidBatteryControl = batteryControl
		publishNumber("battery_control", "Battery Control", 0, float64(maximumBatteryControl), 100, float64(batteryControl), deviceInfo)
	} else {
		publishNumber("battery_control", "Battery Control", 0, float64(maximumBatteryControl), 100, float64(batteryControl), deviceInfo)
	}

	// Publish sensors regardless of initial state
	publishSensor("battery_soc", "Battery State of Charge", "%", deviceInfo)
	publishSensor("battery_charge_power", "Battery Charge Power", "W", deviceInfo)
	publishSensor("battery_discharge_power", "Battery Discharge Power", "W", deviceInfo)
	publishSensor("ac_power", "AC Power", "W", deviceInfo)
	publishSensor("grid_feed", "Grid Feed Power", "W", deviceInfo)
	publishSensor("grid_draw", "Grid Draw Power", "W", deviceInfo)
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
	// Create Modbus TCP client handler
	handler := modbus.NewTCPClientHandler(
		fmt.Sprintf("%s:%s",
			getEnv("SMA_INVERTER_MODBUS_ADDRESS", "192.168.1.100"),
			getEnv("SMA_INVERTER_MODBUS_PORT", "502")),
	)
	handler.Timeout = 10 * time.Second
	handler.SlaveId = 3 // SMA inverter Modbus slave ID

	// Connect to Modbus device
	err := handler.Connect()
	if err != nil {
		log.Fatalf("Modbus connection error: %v", err)
	}
	modbusClient = modbus.NewClient(handler)
}

func modbusReadLoop() {
	ticker := time.NewTicker(time.Duration(modbusIntervalInSeconds) * time.Second)
	resetTicker := time.NewTicker(time.Duration(resetIntervalMinutes) * time.Minute) // Check every minute
	for {
		select {
		case <-ticker.C:
			readAndPublishData()
			checkPauseChargeOkMode()
		case <-resetTicker.C:
			applyControlLogic()
		}
	}
}

func readAndPublishData() {
	// Define Modbus input register addresses
	registers := map[string]uint16{
		"battery_soc":             30845,
		"battery_charge_power":    31393,
		"battery_discharge_power": 31395,
		"ac_power":                30775,
		"grid_feed":               30867,
		"grid_draw":               30865,
	}

	for key, address := range registers {
		result, err := modbusClient.ReadInputRegisters(address, 2)
		if err != nil {
			if debugEnabled {
				log.Printf("Error reading %s register: %v", key, err)
			}
			continue
		}
		value := int32(binary.BigEndian.Uint32(result))

		// Update control variables
		switch key {
		case "battery_discharge_power":
			batteryDischargePower = int(value)
		case "battery_charge_power":
			batteryChargePower = int(value)
		case "ac_power":
			acPower = int(value)
		case "grid_feed":
			gridFeed = int(value)
		case "grid_draw":
			gridDraw = int(value)
		}

		// Publish to MQTT
		stateTopic := fmt.Sprintf("homeassistant/sensor/%s/%s/state", deviceID, key)
		mqttPublish(stateTopic, []byte(fmt.Sprintf("%d", value)), false)
	}
}

func checkPauseChargeOkMode() {
	var currentMode string
	if overwriteLogicSelection != "Automatic" {
		currentMode = overwriteLogicSelection
	} else {
		currentMode = automaticLogicSelection
	}
	if currentMode == "Pause (charge ok)" && !pauseActivated && batteryDischargePower > 0 {
		applyControlLogic()
	}
}

func applyControlLogic() {
	var spntCom uint32 = 0
	var pwrAtCom int32 = 0
	var currentMode string

	if overwriteLogicSelection != "Automatic" {
		currentMode = overwriteLogicSelection
	} else {
		currentMode = automaticLogicSelection
	}

	// Only apply control logic if mode has changed or not in "Automatic" mode
	if currentMode != previousMode || (currentMode != "Automatic" && !(currentMode == "Pause (charge ok)" && !pauseActivated && gridFeed > 50 && batteryDischargePower == 0)) {
		if debugEnabled {
			log.Printf("Applying control logic: Mode=%s", currentMode)
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
	}
	readAndPublishData()
}

func applyMode(mode string, spntCom *uint32, pwrAtCom *int32) {
	const (
		controlOn  uint32 = 802
		controlOff uint32 = 803
	)

	switch mode {
	case "Pause (charge ok)":
		*spntCom = controlOn
		if gridFeed > 100 && batteryDischargePower == 0 {
			pauseActivated = false
			// Allow charging up to the specified battery control value
			*spntCom = controlOff
			*pwrAtCom = 0
			if debugEnabled {
				log.Println("We are supplying Power, disable control")
			}
		} else {
			pauseActivated = true
			// if we supply energy to the grid, turn on charging
			*pwrAtCom = 0
			if debugEnabled {
				log.Println("Battery is discharging, setting power command to 0W")
			}
		}
	case "Pause":
		pauseActivated = true
		*spntCom = controlOn
		*pwrAtCom = 0
	case "Charge Battery":
		pauseActivated = false
		*spntCom = controlOn
		*pwrAtCom = -int32(batteryControl)
	case "Discharge Battery":
		pauseActivated = false
		*spntCom = controlOn
		*pwrAtCom = int32(batteryControl)
	default: // Automatic
		pauseActivated = false
		*spntCom = controlOff
		*pwrAtCom = 0
	}
}

func writeControlCommands(spntCom uint32, pwrAtCom int32) {
	// Write to register 40151 (Communication control)
	spntComData := uint32ToBytes(spntCom)
	if debugEnabled {
		log.Printf("Writing to register 40151: %v", spntComData)
	}
	_, err := modbusClient.WriteMultipleRegisters(40151, 2, spntComData)
	if err != nil {
		log.Printf("Error writing to register 40151: %v", err)
		return
	}
	time.Sleep(100 * time.Millisecond)

	// Write to register 40149 (Power command)
	pwrAtComData := int32ToBytes(pwrAtCom)
	if debugEnabled {
		log.Printf("Writing to register 40149: %v", pwrAtComData)
	}
	_, err = modbusClient.WriteMultipleRegisters(40149, 2, pwrAtComData)
	if err != nil {
		log.Printf("Error writing to register 40149: %v", err)
		return
	}
	if debugEnabled {
		log.Printf("Control command sent: SpntCom=%d, PwrAtCom=%d", spntCom, pwrAtCom)
	}
}

func loadInitialSettings() {
	mqttClient.Subscribe("homeassistant/select/sma_battery_controller/automatic_logic_selection/state", 0, func(client mqtt.Client, msg mqtt.Message) {
		automaticLogicSelection = string(msg.Payload())
		if debugEnabled {
			log.Printf("Loaded automatic_logic_selection from MQTT: %s", automaticLogicSelection)
		}
	})

	mqttClient.Subscribe("homeassistant/select/sma_battery_controller/overwrite_logic_selection/state", 0, func(client mqtt.Client, msg mqtt.Message) {
		overwriteLogicSelection = string(msg.Payload())
		if debugEnabled {
			log.Printf("Loaded overwrite_logic_selection from MQTT: %s", overwriteLogicSelection)
		}
	})

	mqttClient.Subscribe("homeassistant/number/sma_battery_controller/battery_control/state", 0, func(client mqtt.Client, msg mqtt.Message) {
		value, err := strconv.Atoi(string(msg.Payload()))
		if err == nil {
			batteryControl = value
			lastValidBatteryControl = value
		}
		if debugEnabled {
			log.Printf("Loaded battery_control from MQTT: %d", batteryControl)
		}
	})

	// bad work around for racecondition problem
	// Delay to allow initial values to load
	time.Sleep(500 * time.Millisecond) // Wait for subscriptions to take effect

	// Set defaults if no values are loaded
	if automaticLogicSelection == "" {
		automaticLogicSelection = "Automatic"
	}
	if overwriteLogicSelection == "" {
		overwriteLogicSelection = "Automatic"
	}
	if batteryControl == 0 {
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
	token.Wait()
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
