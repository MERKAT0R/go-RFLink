/* SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright (c) 2022-2026 MERKATOR <merkator@merkator.pro>
 * @AGPL-3.0-or-later <https://spdx.org/licenses/AGPL-3.0-or-later.html>
 */

package rflink

import (
	"context"
	"fmt"
	"strings"
)

type haEntitySpec struct {
	component    string
	objectSuffix string
	nameSuffix   string // short entity name ONLY (device name is on device{})
	deviceClass  string
	unit         string
	stateClass   string
	payloadOn    string
	payloadOff   string
	offDelay     *int // seconds; for momentary RF buttons
}

var haEntitySpecs = map[string]haEntitySpec{
	"temp": {
		component:    "sensor",
		objectSuffix: "temp",
		nameSuffix:   "Temperature",
		deviceClass:  "temperature",
		unit:         "°C",
		stateClass:   "measurement",
	},
	"hum": {
		component:    "sensor",
		objectSuffix: "hum",
		nameSuffix:   "Humidity",
		deviceClass:  "humidity",
		unit:         "%",
		stateClass:   "measurement",
	},
	"bat": {
		component:    "binary_sensor",
		objectSuffix: "bat",
		nameSuffix:   "Battery",
		deviceClass:  "battery",
		payloadOn:    "LOW",
		payloadOff:   "OK",
	},
	// Legacy single "switch" field (SWITCH id as value) — avoided when CMD present.
	"switch": {
		component:    "sensor",
		objectSuffix: "switch",
		nameSuffix:   "Switch ID",
	},
	"cmd": {
		component:    "sensor",
		objectSuffix: "cmd",
		nameSuffix:   "Command",
	},
}

type haDiscoveryPayload struct {
	Name                string         `json:"name"`
	ObjectID            string         `json:"object_id,omitempty"`
	UniqueID            string         `json:"unique_id"`
	StateTopic          string         `json:"state_topic"`
	DeviceClass         string         `json:"device_class,omitempty"`
	UnitOfMeasurement   string         `json:"unit_of_measurement,omitempty"`
	StateClass          string         `json:"state_class,omitempty"`
	PayloadOn           string         `json:"payload_on,omitempty"`
	PayloadOff          string         `json:"payload_off,omitempty"`
	OffDelay            *int           `json:"off_delay,omitempty"`
	AvailabilityTopic   string         `json:"availability_topic,omitempty"`
	PayloadAvailable    string         `json:"payload_available,omitempty"`
	PayloadNotAvailable string         `json:"payload_not_available,omitempty"`
	JsonAttributesTopic string         `json:"json_attributes_topic,omitempty"`
	Device              haDeviceConfig `json:"device"`
}

type haDeviceConfig struct {
	Identifiers  []string `json:"identifiers"`
	Name         string   `json:"name"`
	Manufacturer string   `json:"manufacturer"`
	Model        string   `json:"model"`
}

func (p *Publisher) publishHomeAssistantDiscovery(ctx context.Context, sd *SensorData, fields map[string]string) {
	for field, value := range fields {
		if value == "" {
			continue
		}
		// Skip raw switch id / cmd when we already publish per-button sw_XX entities.
		if (field == "switch" || field == "cmd") && hasSwitchButtonFields(fields) {
			continue
		}
		p.publishHomeAssistantEntity(ctx, sd, field)
	}
}

func hasSwitchButtonFields(fields map[string]string) bool {
	for k := range fields {
		if strings.HasPrefix(k, "sw_") {
			return true
		}
	}
	return false
}

func resolveHAEntitySpec(field string) haEntitySpec {
	if spec, ok := haEntitySpecs[field]; ok {
		return spec
	}

	// Per-button channel: sw_04, sw_0a, …
	if after, ok := strings.CutPrefix(field, "sw_"); ok {
		ch := after
		offDelay := 1 // RF remotes often only send ON
		return haEntitySpec{
			component:    "binary_sensor",
			objectSuffix: field,
			nameSuffix:   fmt.Sprintf("Button %s", strings.ToUpper(ch)),
			payloadOn:    "ON",
			payloadOff:   "OFF",
			offDelay:     &offDelay,
		}
	}

	nameSuffix := field
	if len(field) > 0 {
		nameSuffix = strings.ToUpper(field[:1]) + field[1:]
	}
	return haEntitySpec{
		component:    "sensor",
		objectSuffix: field,
		nameSuffix:   nameSuffix,
	}
}

func (p *Publisher) publishHomeAssistantEntity(ctx context.Context, sd *SensorData, field string) {
	spec := resolveHAEntitySpec(field)

	// Stable unique_id; object_id is short and must NOT repeat the device name
	// (HA would otherwise produce entity_id like lacrossev4_0002_lacrossev4_0002_temperature).
	hwidSafe := sanitizeTopicSegment(sd.Hwid)
	uniqueID := fmt.Sprintf("gorflink_%s_%s", hwidSafe, sanitizeTopicSegment(spec.objectSuffix))
	objectID := fmt.Sprintf("%s_%s", hwidSafe, sanitizeTopicSegment(spec.objectSuffix))

	if _, loaded := p.discovered.LoadOrStore(uniqueID, true); loaded {
		return
	}

	deviceName := sd.FriendlyName
	if deviceName == "" {
		// Fallback if applyFriendlyName was not called
		deviceName = fmt.Sprintf("goRFLink_%s %s", sd.Model, sd.Id)
	}

	topicID := sd.TopicID()
	sumTopic := p.sensorFieldTopic(topicID, sumJSONSubtopic)
	availTopic := p.opts.serialStatusTopic()

	// Entity name is ONLY the measurement/button name.
	// Device block already carries the human device name — avoids HA name doubling.
	payload := haDiscoveryPayload{
		Name:                spec.nameSuffix,
		ObjectID:            objectID,
		UniqueID:            uniqueID,
		StateTopic:          p.sensorFieldTopic(topicID, field),
		AvailabilityTopic:   availTopic,
		PayloadAvailable:    statusOnline,
		PayloadNotAvailable: statusOffline,
		JsonAttributesTopic: sumTopic,
		Device: haDeviceConfig{
			Identifiers:  []string{fmt.Sprintf("gorflink_%s", hwidSafe)},
			Name:         deviceName,
			Manufacturer: "RFLink",
			Model:        sd.Model,
		},
	}

	if field == "temp" {
		switch p.opts.temperatureUnit() {
		case TempUnitF:
			payload.UnitOfMeasurement = "°F"
			payload.DeviceClass = "temperature"
			payload.StateClass = "measurement"
		case TempUnitRaw:
			// raw hex — no unit / device class
		default:
			payload.UnitOfMeasurement = "°C"
			payload.DeviceClass = "temperature"
			payload.StateClass = "measurement"
		}
	} else {
		if spec.deviceClass != "" {
			payload.DeviceClass = spec.deviceClass
		}
		if spec.unit != "" {
			payload.UnitOfMeasurement = spec.unit
		}
		if spec.stateClass != "" {
			payload.StateClass = spec.stateClass
		}
		if spec.payloadOn != "" {
			payload.PayloadOn = spec.payloadOn
		}
		if spec.payloadOff != "" {
			payload.PayloadOff = spec.payloadOff
		}
		if spec.offDelay != nil {
			payload.OffDelay = spec.offDelay
		}
	}

	discoveryTopic := fmt.Sprintf("%s/%s/%s/config",
		p.opts.Publish.HADiscoveryPrefix,
		spec.component,
		uniqueID,
	)

	if err := p.publishRaw(ctx, discoveryTopic, payload, p.opts.mqttQoS(), true); err != nil {
		p.discovered.Delete(uniqueID)
		log.Error("home assistant discovery publish failed",
			"topic", discoveryTopic,
			"err", err,
		)
		return
	}

	log.Info("home assistant entity discovered",
		"component", spec.component,
		"unique_id", uniqueID,
		"object_id", objectID,
		"name", payload.Name,
		"state_topic", payload.StateTopic,
		"device", deviceName,
	)
}
