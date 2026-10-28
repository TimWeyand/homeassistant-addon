package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	modbus "github.com/goburrow/modbus"
)

// fakeModbus implements modbus.Client for the two methods the controller uses;
// the embedded nil interface makes any other call panic loudly.
type fakeModbus struct {
	modbus.Client
	mu         sync.Mutex
	regs       map[uint16]int32
	writes     []fakeWrite // successful writes only
	reads      int
	failWrites int
	failReads  int
}

type fakeWrite struct {
	addr  uint16
	value uint32
}

func (f *fakeModbus) ReadInputRegisters(address, quantity uint16) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.failReads > 0 {
		f.failReads--
		return nil, errors.New("injected read error")
	}
	return int32ToBytes(f.regs[address]), nil
}

func (f *fakeModbus) WriteMultipleRegisters(address, quantity uint16, value []byte) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWrites > 0 {
		f.failWrites--
		return nil, errors.New("injected write error")
	}
	f.writes = append(f.writes, fakeWrite{address, binary.BigEndian.Uint32(value)})
	return nil, nil
}

// set stores a polled register value by sensor name.
func (f *fakeModbus) set(name string, v int32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range polledRegisters {
		if r.name == name {
			f.regs[r.addr] = v
			return
		}
	}
	panic("unknown register " + name)
}

// lastCommand returns the last successfully written SpntCom / PwrAtCom.
func (f *fakeModbus) lastCommand() (spnt uint32, pwr int32, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, w := range f.writes {
		switch w.addr {
		case 40151:
			spnt = w.value
		case 40149:
			pwr = int32(w.value)
		}
	}
	return spnt, pwr, len(f.writes)
}

type fakeMsg struct {
	mqtt.Message
	topic, payload string
}

func (m fakeMsg) Topic() string   { return m.topic }
func (m fakeMsg) Payload() []byte { return []byte(m.payload) }

// setupController resets all controller globals to a known state, installs a
// fake Modbus client and a capturing MQTT publish stub.
func setupController(t *testing.T) (*fakeModbus, func() []publishedMessage) {
	t.Helper()
	origPublish, origReconnect, origClient := mqttPublish, modbusReconnect, modbusClient
	t.Cleanup(func() { mqttPublish, modbusReconnect, modbusClient = origPublish, origReconnect, origClient })

	var mu sync.Mutex
	var captured []publishedMessage
	mqttPublish = func(topic string, payload []byte, retain bool) {
		mu.Lock()
		captured = append(captured, publishedMessage{topic, append([]byte(nil), payload...), retain})
		mu.Unlock()
	}
	fake := &fakeModbus{regs: map[uint16]int32{}}
	modbusClient = fake
	modbusReconnect = func() error { return nil }

	deviceID = "test"
	sensorTopicPrefix = "homeassistant/sensor/test/"
	maximumBatteryControl = 8000
	debugEnabled = false
	postCommandDelayMs = 0
	automaticLogicSelection, overwriteLogicSelection, currentLogicSelection = "Automatic", "Off", "Automatic"
	batteryControl, lastValidBatteryControl = 7000, 7000
	previousMode = ""
	lastWrittenSpntCom = controlOn
	internalAutomatic, internalAutomaticReason, actualChargingPower = false, "", 0
	savedUserBatteryControl, savedUserBatteryControlValid = 0, false
	pauseActivated = false
	batterySoc, batteryChargePower, batteryDischargePower, gridDraw, gridFeed = 0, 0, 0, 0, 0
	lastSensorValues = map[string]string{}
	modbusClientErrorCount.Store(0)

	return fake, func() []publishedMessage {
		mu.Lock()
		defer mu.Unlock()
		return append([]publishedMessage(nil), captured...)
	}
}

func setOverwrite(mode string) {
	mqttMessageHandler(nil, fakeMsg{topic: "homeassistant/select/test/overwrite_logic_selection/set", payload: mode})
}

func wantCommand(t *testing.T, fake *fakeModbus, spnt uint32, pwr int32) {
	t.Helper()
	gotS, gotP, _ := fake.lastCommand()
	if gotS != spnt || gotP != pwr {
		t.Fatalf("last command = %d/%d, want %d/%d", gotS, gotP, spnt, pwr)
	}
}

func TestChargeBatteryToBalanced_ReleasesForcedCharge(t *testing.T) {
	fake, _ := setupController(t)
	fake.set("battery_soc", 50)
	overwriteLogicSelection = "Charge Battery"
	readAndPublishData()
	applyControlLogic()
	wantCommand(t, fake, controlOn, -7000)

	// Forced charge is running; Balanced sees charge >= chargeActiveW -> "no write" branch
	fake.set("battery_charge_power", 7000)
	readAndPublishData()
	setOverwrite("Balanced")
	wantCommand(t, fake, controlOff, 0)

	// Released once: staying in Balanced does not keep writing
	_, _, n := fake.lastCommand()
	applyControlLogic()
	if _, _, n2 := fake.lastCommand(); n2 != n {
		t.Fatalf("expected no further writes, got %d -> %d", n, n2)
	}
}

func TestBalancedDischarge_FeedAboveControl_Releases(t *testing.T) {
	fake, _ := setupController(t)
	overwriteLogicSelection, previousMode, batteryControl = "Balanced", "Balanced", 1000
	lastWrittenSpntCom = controlOff

	fake.set("grid_draw", 500)
	readAndPublishData()
	applyControlLogic()
	wantCommand(t, fake, controlOn, 1500)

	fake.set("grid_draw", 0)
	fake.set("grid_feed", 2000)
	fake.set("battery_discharge_power", 1500)
	readAndPublishData()
	applyControlLogic()
	wantCommand(t, fake, controlOff, 0)
	if batteryControl != 0 {
		t.Fatalf("batteryControl = %d, want 0", batteryControl)
	}
}

func TestAutomaticSelectionBalanced_ReleasesOldCommand(t *testing.T) {
	fake, _ := setupController(t)
	automaticLogicSelection, previousMode = "Balanced", "Discharge Battery"
	lastWrittenSpntCom = controlOn
	applyControlLogic()
	wantCommand(t, fake, controlOff, 0)
}

func TestFailedAutomaticWrite_IsRetried(t *testing.T) {
	fake, _ := setupController(t)
	fake.set("battery_soc", 50)
	automaticLogicSelection = "Charge Battery"
	readAndPublishData()
	applyControlLogic()
	wantCommand(t, fake, controlOn, -7000)

	automaticLogicSelection = "Automatic"
	fake.failWrites = 1
	applyControlLogic()
	wantCommand(t, fake, controlOn, -7000) // 803 not applied
	if previousMode == "Automatic" {
		t.Fatalf("previousMode advanced to Automatic despite failed write")
	}

	applyControlLogic()
	wantCommand(t, fake, controlOff, 0)
	if previousMode != "Automatic" || lastWrittenSpntCom != controlOff {
		t.Fatalf("previousMode=%q lastWritten=%d after successful retry", previousMode, lastWrittenSpntCom)
	}
	if got := modbusClientErrorCount.Load(); got != 0 {
		t.Fatalf("error count = %d after successful read cycle, want 0", got)
	}
}

func TestChargeBatteryRevertThreshold(t *testing.T) {
	cases := []struct {
		bc, charge int
		revert     bool
	}{
		{400, 100, true},   // old threshold (-100) was unreachable
		{400, 300, false},  // PV/charging above half the setpoint stays automatic
		{7000, 6000, true}, // large setpoint keeps tapered-500
		{7000, 6600, false},
	}
	for _, c := range cases {
		setupController(t)
		batteryControl = c.bc
		batterySoc, batteryChargePower, gridFeed = 50, c.charge, 0
		internalAutomatic = true
		var spnt uint32
		var pwr int32
		applyMode("Charge Battery", &spnt, &pwr)
		if c.revert {
			if spnt != controlOn || pwr != -int32(c.bc) || internalAutomatic {
				t.Errorf("bc=%d charge=%d: got %d/%d internal=%v, want forced charge", c.bc, c.charge, spnt, pwr, internalAutomatic)
			}
		} else if spnt != controlOff || !internalAutomatic {
			t.Errorf("bc=%d charge=%d: got %d/%d internal=%v, want stay automatic", c.bc, c.charge, spnt, pwr, internalAutomatic)
		}
	}
}

func TestBalanced_RestoresUserBatteryControl(t *testing.T) {
	_, published := setupController(t)
	batteryControl, lastValidBatteryControl = 6000, 6000

	setOverwrite("Balanced")
	if batteryControl != 0 {
		t.Fatalf("Balanced init: batteryControl = %d, want 0", batteryControl)
	}
	setOverwrite("Off")
	if batteryControl != 6000 || lastValidBatteryControl != 6000 {
		t.Fatalf("after leaving Balanced: batteryControl = %d, want 6000", batteryControl)
	}
	var last string
	for _, m := range published() {
		if m.topic == "homeassistant/number/test/battery_control/state" {
			last = string(m.payload)
		}
	}
	if last != "6000" {
		t.Fatalf("last published battery_control = %q, want 6000", last)
	}
}

func TestNormalizeLoadedSettings(t *testing.T) {
	cases := []struct {
		name                 string
		auto, over           string
		bc                   int
		autoOK, overOK, bcOK bool
		wantAuto, wantOver   string
		wantBC               int
	}{
		{"normalize+clamp high", "pause", "charge battery", 9000, true, true, true, "Pause", "Charge Battery", 8000},
		{"off invalid for auto, empty overwrite, clamp low", "Off", " ", -5, true, true, true, "Automatic", "Off", 0},
		{"overwrite OFF stays Off", "BALANCED", "OFF", 100, true, true, true, "Balanced", "Off", 100},
		{"nothing loaded -> defaults", "Automatic", "Off", 0, false, false, false, "Automatic", "Off", 7200},
	}
	for _, c := range cases {
		setupController(t)
		automaticLogicSelection, overwriteLogicSelection, batteryControl = c.auto, c.over, c.bc
		automaticSelectionLoaded, overwriteSelectionLoaded, batteryControlLoaded = c.autoOK, c.overOK, c.bcOK
		normalizeLoadedSettings()
		if automaticLogicSelection != c.wantAuto || overwriteLogicSelection != c.wantOver || batteryControl != c.wantBC || lastValidBatteryControl != c.wantBC {
			t.Errorf("%s: got %q/%q/%d (lastValid %d), want %q/%q/%d", c.name, automaticLogicSelection, overwriteLogicSelection, batteryControl, lastValidBatteryControl, c.wantAuto, c.wantOver, c.wantBC)
		}
	}
	automaticSelectionLoaded, overwriteSelectionLoaded, batteryControlLoaded = false, false, false
}

func TestReadError_AbortsCycle(t *testing.T) {
	fake, _ := setupController(t)
	fake.failReads = 1
	readAndPublishData()
	if fake.reads != 1 {
		t.Fatalf("reads after first-register error = %d, want 1 (cycle aborted)", fake.reads)
	}
	if got := modbusClientErrorCount.Load(); got != 1 {
		t.Fatalf("error count = %d, want 1", got)
	}
	readAndPublishData()
	if got := modbusClientErrorCount.Load(); got != 0 {
		t.Fatalf("error count = %d after successful cycle, want 0", got)
	}
}

func TestConcurrentReadAndPublish_NoRace(t *testing.T) {
	fake, _ := setupController(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				fake.set("grid_draw", int32(i*100+j))
				readAndPublishData()
				checkPauseChargeOkMode()
				_ = balancedDischargeActive()
				if j%5 == 0 {
					resetSensorCache()
				}
			}
		}(i)
	}
	wg.Wait()
}

func TestRepublishAfterHARestart(t *testing.T) {
	_, published := setupController(t)
	internalAutomatic = true
	lastSensorValues["battery_soc"] = "50"

	republishAfterHARestart()

	if !internalAutomatic {
		t.Fatalf("republish must not reset internalAutomatic")
	}
	if len(lastSensorValues) != 0 {
		t.Fatalf("sensor cache not cleared: %v", lastSensorValues)
	}
	var dc2Name, intAuto string
	for _, m := range published() {
		switch m.topic {
		case "homeassistant/sensor/test/dc2_current/config":
			var cfg map[string]interface{}
			if err := json.Unmarshal(m.payload, &cfg); err != nil {
				t.Fatal(err)
			}
			dc2Name, _ = cfg["name"].(string)
		case "homeassistant/binary_sensor/test/internal_automatic/state":
			intAuto = string(m.payload)
		}
	}
	if dc2Name != "DC2 Current" {
		t.Errorf("dc2_current name = %q, want DC2 Current", dc2Name)
	}
	if intAuto != "ON" {
		t.Errorf("internal_automatic state = %q, want ON", intAuto)
	}
}

// A poll tick racing the overwrite handler must never see the restored user
// value while the overwrite still says Balanced (would discharge into the grid).
func TestLeavingBalanced_PollInWindow_NoDischarge(t *testing.T) {
	fake, _ := setupController(t)
	batteryControl, lastValidBatteryControl = 6000, 6000
	setOverwrite("Balanced")
	fake.set("grid_draw", 400)
	readAndPublishData()

	start := len(fake.writes)
	inner := mqttPublish
	injected := false
	mqttPublish = func(topic string, payload []byte, retain bool) {
		inner(topic, payload, retain)
		if !injected && topic == "homeassistant/number/test/battery_control/state" {
			injected = true
			checkPauseChargeOkMode() // poll tick inside the handler's publish window
		}
	}
	setOverwrite("Off")
	if !injected {
		t.Fatal("battery_control restore was not published")
	}
	for _, w := range fake.writes[start:] {
		if w.addr == 40151 && w.value == controlOn {
			t.Fatalf("forced command written while leaving Balanced: %v", fake.writes[start:])
		}
	}
	if batteryControl != 6000 {
		t.Fatalf("batteryControl = %d, want restored 6000", batteryControl)
	}
}

// A failed write on a mode change is retried by the regular poll tick, not only
// by the resetTicker.
func TestFailedWrite_RetriedOnPollTick(t *testing.T) {
	cases := []struct {
		mode string
		spnt uint32
	}{
		{"Automatic", controlOff},
		{"Pause", controlOn},
	}
	for _, c := range cases {
		fake, _ := setupController(t)
		fake.set("battery_soc", 50)
		automaticLogicSelection = "Charge Battery"
		readAndPublishData()
		applyControlLogic()
		wantCommand(t, fake, controlOn, -7000)

		automaticLogicSelection = c.mode
		fake.failWrites = 1
		applyControlLogic()
		wantCommand(t, fake, controlOn, -7000)

		checkPauseChargeOkMode()
		gotS, gotP, _ := fake.lastCommand()
		if gotS != c.spnt || gotP != 0 || previousMode != c.mode {
			t.Errorf("%s: after poll tick last command=%d/%d previousMode=%q, want %d/0", c.mode, gotS, gotP, previousMode, c.spnt)
		}
	}
}
