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
	Name                string           `json:"name"`
	ObjectID            string           `json:"object_id,omitempty"` // kept for HA before 2026.4
	DefaultEntityID     string           `json:"default_entity_id,omitempty"`
	UniqueID            string           `json:"unique_id"`
	Origin              *haOrigin        `json:"origin,omitempty"`
	StateTopic          string           `json:"state_topic"`
	DeviceClass         string           `json:"device_class,omitempty"`
	UnitOfMeasurement   string           `json:"unit_of_measurement,omitempty"`
	StateClass          string           `json:"state_class,omitempty"`
	PayloadOn           string           `json:"payload_on,omitempty"`
	PayloadOff          string           `json:"payload_off,omitempty"`
	OffDelay            *int             `json:"off_delay,omitempty"`
	AvailabilityTopic   string           `json:"availability_topic,omitempty"`
	PayloadAvailable    string           `json:"payload_available,omitempty"`
	PayloadNotAvailable string           `json:"payload_not_available,omitempty"`
	Availability        []haAvailability `json:"availability,omitempty"`
	AvailabilityMode    string           `json:"availability_mode,omitempty"`
	JsonAttributesTopic string           `json:"json_attributes_topic,omitempty"`
	Device              haDeviceConfig   `json:"device"`
}

type haDiscovered struct {
	component string
	topic     string
}

type haAvailability struct {
	Topic               string `json:"topic"`
	PayloadAvailable    string `json:"payload_available,omitempty"`
	PayloadNotAvailable string `json:"payload_not_available,omitempty"`
}

type haOrigin struct {
	Name      string `json:"name"`
	SWVersion string `json:"sw_version,omitempty"`
}

type haDeviceConfig struct {
	Identifiers  []string `json:"identifiers"`
	Name         string   `json:"name"`
	Manufacturer string   `json:"manufacturer"`
	Model        string   `json:"model"`
}

func (p *Publisher) publishHomeAssistantDiscovery(ctx context.Context, sd *SensorData, fields map[string]string, prefix string) {
	for field, value := range fields {
		if value == "" {
			continue
		}
		// Skip raw switch id / cmd when we already publish per-button sw_XX entities.
		if (field == "switch" || field == "cmd") && hasSwitchButtonFields(fields) {
			continue
		}
		p.publishHomeAssistantEntity(ctx, sd, field, value, prefix)
	}
}

// haDefaultEntityID is the post-2026.4 replacement for JSON object_id.
// The domain is required; the object id itself stays unchanged so existing
// entity ids keep matching unique_id.
func haDefaultEntityID(component, objectID string) string {
	return component + "." + objectID
}

func haOriginInfo() *haOrigin {
	sw := strings.TrimSpace(Version)
	if sw == "" {
		sw = "dev"
	}
	return &haOrigin{Name: "go-rflink", SWVersion: sw}
}

func hasSwitchButtonFields(fields map[string]string) bool {
	for k := range fields {
		if strings.HasPrefix(k, "sw_") {
			return true
		}
	}
	return false
}

func resolveHAEntitySpec(field, value string) haEntitySpec {
	if spec, ok := haEntitySpecs[field]; ok {
		return spec
	}

	// Per-button channel: sw_04, sw_0a, …
	// ON/OFF map to a binary sensor. UP/DOWN/ALLON stay a plain sensor so HA
	// does not drop values that match neither payload_on nor payload_off.
	if after, ok := strings.CutPrefix(field, "sw_"); ok {
		ch := after
		v := strings.ToUpper(strings.TrimSpace(value))
		if v == "ON" || v == "OFF" || v == "" {
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
		return haEntitySpec{
			component:    "sensor",
			objectSuffix: field,
			nameSuffix:   fmt.Sprintf("Button %s", strings.ToUpper(ch)),
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

func (p *Publisher) publishHomeAssistantEntity(ctx context.Context, sd *SensorData, field, value, prefix string) {
	spec := resolveHAEntitySpec(field, value)

	// Stable unique_id; object_id is short and must NOT repeat the device name
	// (HA would otherwise produce entity_id like lacrossev4_0002_lacrossev4_0002_temperature).
	hwidSafe := sanitizeTopicSegment(sd.Hwid)
	uniqueID := fmt.Sprintf("gorflink_%s_%s", hwidSafe, sanitizeTopicSegment(spec.objectSuffix))
	objectID := fmt.Sprintf("%s_%s", hwidSafe, sanitizeTopicSegment(spec.objectSuffix))

	discoveryTopic := fmt.Sprintf("%s/%s/%s/config",
		prefix,
		spec.component,
		uniqueID,
	)
	retiredOld := false
	if prev, loaded := p.discovered.Load(uniqueID); loaded {
		old, _ := prev.(haDiscovered)
		if old.component == spec.component && old.topic == discoveryTopic {
			return
		}
		if old.topic != "" && old.topic != discoveryTopic {
			if err := p.publishRaw(ctx, old.topic, "", p.opts.mqttQoS(), true); err != nil {
				log.Error("home assistant discovery retire failed", "topic", old.topic, "err", err)
				return
			}
			retiredOld = true
		}
	}

	deviceName := sd.FriendlyName
	if deviceName == "" {
		// Fallback if applyFriendlyName was not called
		deviceName = fmt.Sprintf("goRFLink_%s %s", sd.Model, sd.Id)
	}

	topicID := sd.TopicID()
	sumTopic := p.sensorFieldTopic(topicID, sumJSONSubtopic)

	// Both topics must be online. MQTT LWT covers a dead process; serial/status
	// covers a dead radio while the broker session is still up.
	avail := []haAvailability{
		{Topic: p.opts.statusTopic(), PayloadAvailable: statusOnline, PayloadNotAvailable: statusOffline},
		{Topic: p.opts.serialStatusTopic(), PayloadAvailable: statusOnline, PayloadNotAvailable: statusOffline},
	}

	// Entity name is ONLY the measurement/button name.
	// Device block already carries the human device name — avoids HA name doubling.
	payload := haDiscoveryPayload{
		Name:                spec.nameSuffix,
		ObjectID:            objectID,
		DefaultEntityID:     haDefaultEntityID(spec.component, objectID),
		UniqueID:            uniqueID,
		Origin:              haOriginInfo(),
		StateTopic:          p.sensorFieldTopic(topicID, field),
		Availability:        avail,
		AvailabilityMode:    "all",
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

	if err := p.publishRaw(ctx, discoveryTopic, payload, p.opts.mqttQoS(), true); err != nil {
		if retiredOld {
			// The previous config is already cleared. Drop the cache entry so
			// the next frame publishes this topic again.
			p.discovered.Delete(uniqueID)
		}
		log.Error("home assistant discovery publish failed",
			"topic", discoveryTopic,
			"err", err,
		)
		return
	}
	p.discovered.Store(uniqueID, haDiscovered{component: spec.component, topic: discoveryTopic})

	log.Info("home assistant entity discovered",
		"component", spec.component,
		"unique_id", uniqueID,
		"object_id", objectID,
		"name", payload.Name,
		"state_topic", payload.StateTopic,
		"device", deviceName,
	)
}
