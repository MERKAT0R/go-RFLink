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
	component   string
	topic       string
	serialTopic string
	deviceName  string
	hwid        string // sanitized HWID this config was published for
	origin      string // sanitized raw HWID, stable when the map renames it
	epoch       uint64
}

// sameSensor reports whether this discovery config is already published.
// The device name is part of the payload, so a rename must be published again.
func (d haDiscovered) sameSensor(component, topic, serialTopic, deviceName string) bool {
	return d.component == component && d.topic == topic && d.serialTopic == serialTopic && d.deviceName == deviceName
}

// sameTrigger reports whether this device-automation config is already published.
func (d haDiscovered) sameTrigger(topic, deviceName string) bool {
	return d.topic == topic && d.deviceName == deviceName
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
	if hasSwitchButtonFields(fields) {
		// FieldsForPublish omits cmd and switch once a channel button exists.
		// Retire a config published by an earlier frame that had only one of them.
		hwidSafe := sanitizeTopicSegment(sd.Hwid)
		p.retireDiscovered(ctx, fmt.Sprintf("gorflink_%s_cmd", hwidSafe))
		p.retireDiscovered(ctx, fmt.Sprintf("gorflink_%s_switch", hwidSafe))
	}
	for field, value := range fields {
		if value == "" {
			continue
		}
		// Skip raw switch id / cmd when we already publish per-button sw_XX entities.
		// A previous frame may have discovered cmd or switch on its own. Clear
		// that retained config so Home Assistant does not keep a stale entity.
		if (field == "switch" || field == "cmd") && hasSwitchButtonFields(fields) {
			hwidSafe := sanitizeTopicSegment(sd.Hwid)
			p.retireDiscovered(ctx, fmt.Sprintf("gorflink_%s_%s", hwidSafe, sanitizeTopicSegment(field)))
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
	topicID := sd.TopicID()
	avail := p.sensorAvailability(topicID)
	serialTopic := ""
	if len(avail) > 1 {
		serialTopic = avail[1].Topic
	}
	deviceName := sd.FriendlyName
	if deviceName == "" {
		// Fallback if applyFriendlyName was not called
		deviceName = fmt.Sprintf("goRFLink_%s %s", sd.Model, sd.Id)
	}
	var oldTopic string
	epoch := p.discoveryEpoch.Load()
	if prev, loaded := p.discovered.Load(uniqueID); loaded {
		old, _ := prev.(haDiscovered)
		if old.epoch == epoch && old.sameSensor(spec.component, discoveryTopic, serialTopic, deviceName) {
			p.retireRenamedDiscovery(ctx, sd, uniqueID)
			p.publishMomentaryTrigger(ctx, sd, field, prefix, uniqueID, spec)
			return
		}
		if old.topic != "" && old.topic != discoveryTopic {
			oldTopic = old.topic
		}
	}

	sumTopic := p.sensorFieldTopic(topicID, sumJSONSubtopic)

	// Both topics must be online. MQTT LWT covers a dead process. The serial
	// topic is the one gateway when only one is configured. With several
	// gateways it is the port that last owned this sensor, so a different
	// board going down leaves this entity available.
	// A third topic is added only when per-sensor availability is enabled.

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
		// Leave the previous retained config and the cache entry. Clearing
		// first would remove the entity when this publish fails and dedup
		// then drops the repeat that would have created the new one.
		log.Error("home assistant discovery publish failed",
			"topic", discoveryTopic,
			"err", err,
		)
		return
	}
	if oldTopic != "" {
		if err := p.publishRaw(ctx, oldTopic, "", p.opts.mqttQoS(), true); err != nil {
			log.Error("home assistant discovery retire failed", "topic", oldTopic, "err", err)
			return
		}
	}
	p.discovered.Store(uniqueID, haDiscovered{component: spec.component, topic: discoveryTopic, serialTopic: serialTopic, deviceName: deviceName, hwid: hwidSafe, origin: discoveryOrigin(sd), epoch: epoch})
	p.retireRenamedDiscovery(ctx, sd, uniqueID)
	p.publishMomentaryTrigger(ctx, sd, field, prefix, uniqueID, spec)

	log.Info("home assistant entity discovered",
		"component", spec.component,
		"unique_id", uniqueID,
		"object_id", objectID,
		"name", payload.Name,
		"state_topic", payload.StateTopic,
		"device", deviceName,
	)
}

func (p *Publisher) sensorAvailability(topicID string) []haAvailability {
	avail := []haAvailability{
		{Topic: p.opts.statusTopic(), PayloadAvailable: statusOnline, PayloadNotAvailable: statusOffline},
		{Topic: p.sensorSerialTopic(topicID), PayloadAvailable: statusOnline, PayloadNotAvailable: statusOffline},
	}
	if p.opts.Publish.HASensorAvailability && topicID != "" {
		avail = append(avail, haAvailability{
			Topic:               p.sensorFieldTopic(topicID, "availability"),
			PayloadAvailable:    statusOnline,
			PayloadNotAvailable: statusOffline,
		})
	}
	return avail
}

// publishMomentaryTrigger adds a device_automation trigger next to an sw_* binary
// sensor. The sensor unique_id and its discovery topic stay as they are.
func (p *Publisher) publishMomentaryTrigger(ctx context.Context, sd *SensorData, field, prefix, uniqueID string, spec haEntitySpec) {
	if p == nil || p.opts == nil {
		return
	}
	trigID := uniqueID + "_press"
	if !p.opts.Publish.HADeviceTriggers || spec.component != "binary_sensor" || spec.payloadOn != "ON" {
		// A retained device trigger must not stay after triggers are turned off
		// or after this field stops being an ON binary_sensor.
		p.retireDiscovered(ctx, trigID)
		return
	}
	topic := fmt.Sprintf("%s/device_automation/%s/config", prefix, trigID)
	epoch := p.discoveryEpoch.Load()
	var oldTopic string
	if prev, loaded := p.discovered.Load(trigID); loaded {
		if old, ok := prev.(haDiscovered); ok {
			if old.epoch == epoch && old.sameTrigger(topic, sd.FriendlyName) {
				return
			}
			if old.topic != "" && old.topic != topic {
				oldTopic = old.topic
			}
		}
	}
	ch := strings.TrimPrefix(field, "sw_")
	hwidSafe := sanitizeTopicSegment(sd.Hwid)
	payload := map[string]any{
		"automation_type": "trigger",
		"type":            "button_short_press",
		"subtype":         "button_" + strings.ToLower(ch),
		"payload":         "ON",
		"topic":           p.sensorFieldTopic(sd.TopicID(), field),
		"device": map[string]any{
			"identifiers":  []string{fmt.Sprintf("gorflink_%s", hwidSafe)},
			"name":         sd.FriendlyName,
			"manufacturer": "RFLink",
			"model":        sd.Model,
		},
	}
	if err := p.publishRaw(ctx, topic, payload, p.opts.mqttQoS(), true); err != nil {
		log.Error("HA device trigger discovery failed", "topic", topic, "err", err)
		return
	}
	if oldTopic != "" {
		if err := p.publishRaw(ctx, oldTopic, "", p.opts.mqttQoS(), true); err != nil {
			log.Error("HA device trigger retire failed", "topic", oldTopic, "err", err)
			return
		}
	}
	p.discovered.Store(trigID, haDiscovered{component: "device_automation", topic: topic, deviceName: sd.FriendlyName, hwid: hwidSafe, origin: discoveryOrigin(sd), epoch: epoch})
	log.Info("HA device trigger discovered", "unique_id", trigID, "topic", topic)
}

// retireRenamedDiscovery drops cached configs for this sensor whose published
// HWID is no longer the current one. The raw id stays the identity, so a later
// map edit (Kitchen to Living, or clearing the map) still clears the old entity.
func (p *Publisher) retireRenamedDiscovery(ctx context.Context, sd *SensorData, uniqueID string) {
	if p == nil || sd == nil {
		return
	}
	origin := discoveryOrigin(sd)
	curSafe := sanitizeTopicSegment(sd.Hwid)
	if origin == "" || curSafe == "" {
		return
	}
	// Match the stored HWID, not a prefix of it. OREGON_0001_ROOM starts with
	// OREGON_0001_, and a prefix scan would clear the new entity on the next field.
	oldPrefix := "gorflink_" + origin + "_"
	curPrefix := "gorflink_" + curSafe + "_"
	var stale []string
	p.discovered.Range(func(key, val any) bool {
		id, ok := key.(string)
		if !ok || id == uniqueID {
			return true
		}
		if old, is := val.(haDiscovered); is {
			if old.origin != "" {
				if strings.EqualFold(old.origin, origin) && !strings.EqualFold(old.hwid, curSafe) {
					stale = append(stale, id)
				}
				return true
			}
			if old.hwid != "" {
				if strings.EqualFold(old.hwid, origin) && !strings.EqualFold(old.hwid, curSafe) {
					stale = append(stale, id)
				}
				return true
			}
		}
		if strings.HasPrefix(id, oldPrefix) && !strings.HasPrefix(id, curPrefix) {
			stale = append(stale, id)
		}
		return true
	})
	for _, id := range stale {
		p.retireDiscovered(ctx, id)
	}
}

func discoveryOrigin(sd *SensorData) string {
	if sd == nil {
		return ""
	}
	if raw := sanitizeTopicSegment(sd.RawHWID); raw != "" {
		return raw
	}
	return sanitizeTopicSegment(sd.Hwid)
}

func (p *Publisher) retireDiscovered(ctx context.Context, id string) {
	prev, loaded := p.discovered.Load(id)
	if !loaded {
		return
	}
	old, ok := prev.(haDiscovered)
	if !ok || old.topic == "" {
		p.discovered.Delete(id)
		return
	}
	if err := p.publishRaw(ctx, old.topic, "", p.opts.mqttQoS(), true); err != nil {
		log.Error("home assistant discovery retire failed", "topic", old.topic, "err", err)
		return
	}
	p.discovered.Delete(id)
}
