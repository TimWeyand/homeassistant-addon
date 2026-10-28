package main

import (
	"encoding/json"
	"testing"
)

// publishedMessage captures a single call to the injectable mqttPublish function.
type publishedMessage struct {
	topic   string
	payload []byte
	retain  bool
}

// withCapturedPublish swaps the package-level mqttPublish var for a capturing
// stub for the duration of fn, then restores the original implementation.
// This is the "injizierbare Publish-Funktion" the ticket asks the tests to use.
func withCapturedPublish(fn func(capture *[]publishedMessage)) {
	original := mqttPublish
	defer func() { mqttPublish = original }()

	var captured []publishedMessage
	mqttPublish = func(topic string, payload []byte, retain bool) {
		captured = append(captured, publishedMessage{topic: topic, payload: append([]byte(nil), payload...), retain: retain})
	}

	fn(&captured)
}

func TestSetInternalAutomatic_PublishesOnlyOnChange(t *testing.T) {
	origDeviceID := deviceID
	origInternal := internalAutomatic
	defer func() {
		deviceID = origDeviceID
		internalAutomatic = origInternal
	}()

	deviceID = "test_device"
	internalAutomatic = false

	wantTopic := "homeassistant/binary_sensor/test_device/internal_automatic/state"

	withCapturedPublish(func(capture *[]publishedMessage) {
		// First call: false -> true is a real change, must publish "ON" retained.
		setInternalAutomatic(true)

		if len(*capture) != 1 {
			t.Fatalf("expected exactly 1 publish after switching to true, got %d: %+v", len(*capture), *capture)
		}
		msg := (*capture)[0]
		if msg.topic != wantTopic {
			t.Errorf("topic = %q, want %q", msg.topic, wantTopic)
		}
		if string(msg.payload) != "ON" {
			t.Errorf("payload = %q, want %q", string(msg.payload), "ON")
		}
		if !msg.retain {
			t.Errorf("retain = false, want true")
		}
		if !internalAutomatic {
			t.Errorf("internalAutomatic = false after setInternalAutomatic(true), want true")
		}

		// Second call with the same value: must NOT publish again.
		setInternalAutomatic(true)
		if len(*capture) != 1 {
			t.Fatalf("expected no additional publish for unchanged value, got %d total: %+v", len(*capture), *capture)
		}

		// Third call: true -> false is a change again, must publish "OFF" retained.
		setInternalAutomatic(false)
		if len(*capture) != 2 {
			t.Fatalf("expected exactly 2 publishes total after switching back to false, got %d: %+v", len(*capture), *capture)
		}
		msg = (*capture)[1]
		if msg.topic != wantTopic {
			t.Errorf("topic = %q, want %q", msg.topic, wantTopic)
		}
		if string(msg.payload) != "OFF" {
			t.Errorf("payload = %q, want %q", string(msg.payload), "OFF")
		}
		if !msg.retain {
			t.Errorf("retain = false, want true")
		}
		if internalAutomatic {
			t.Errorf("internalAutomatic = true after setInternalAutomatic(false), want false")
		}
	})
}

func TestPublishBinarySensor_DiscoveryPayload(t *testing.T) {
	origDeviceID := deviceID
	defer func() { deviceID = origDeviceID }()
	deviceID = "test_device2"

	deviceInfo := map[string]interface{}{
		"identifiers":  []string{deviceID},
		"manufacturer": "Custom",
		"model":        "SMA Battery Controller",
		"name":         "SMA Battery Controller",
	}

	wantConfigTopic := "homeassistant/binary_sensor/test_device2/internal_automatic/config"
	wantStateTopic := "homeassistant/binary_sensor/test_device2/internal_automatic/state"

	withCapturedPublish(func(capture *[]publishedMessage) {
		publishBinarySensor("internal_automatic", "Internal Automatic", deviceInfo)

		if len(*capture) != 1 {
			t.Fatalf("expected exactly 1 publish for discovery, got %d: %+v", len(*capture), *capture)
		}
		msg := (*capture)[0]
		if msg.topic != wantConfigTopic {
			t.Errorf("topic = %q, want %q", msg.topic, wantConfigTopic)
		}
		if !msg.retain {
			t.Errorf("retain = false, want true for discovery config")
		}

		var payload map[string]interface{}
		if err := json.Unmarshal(msg.payload, &payload); err != nil {
			t.Fatalf("failed to unmarshal discovery payload: %v\npayload: %s", err, msg.payload)
		}

		if got, want := payload["name"], "Internal Automatic"; got != want {
			t.Errorf("name = %v, want %v", got, want)
		}
		if got, want := payload["unique_id"], "test_device2_internal_automatic"; got != want {
			t.Errorf("unique_id = %v, want %v", got, want)
		}
		if got, want := payload["state_topic"], wantStateTopic; got != want {
			t.Errorf("state_topic = %v, want %v", got, want)
		}
		if got, want := payload["payload_on"], "ON"; got != want {
			t.Errorf("payload_on = %v, want %v", got, want)
		}
		if got, want := payload["payload_off"], "OFF"; got != want {
			t.Errorf("payload_off = %v, want %v", got, want)
		}
		if _, ok := payload["device"]; !ok {
			t.Errorf("device field missing from discovery payload")
		}
		avail, ok := payload["availability"].([]interface{})
		if !ok || len(avail) != 1 {
			t.Fatalf("availability = %v, want a 1-element array", payload["availability"])
		}
		availEntry, ok := avail[0].(map[string]interface{})
		if !ok {
			t.Fatalf("availability[0] is not an object: %v", avail[0])
		}
		if got, want := availEntry["topic"], "smastp_modbus/status"; got != want {
			t.Errorf("availability topic = %v, want %v", got, want)
		}
		if got, want := availEntry["payload_available"], "online"; got != want {
			t.Errorf("availability payload_available = %v, want %v", got, want)
		}
		if got, want := availEntry["payload_not_available"], "offline"; got != want {
			t.Errorf("availability payload_not_available = %v, want %v", got, want)
		}
	})
}
