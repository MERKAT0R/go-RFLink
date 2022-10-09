/*
 * Copyright (c) 2022.  by MERKATOR <merkator@merkator.pro>
 * This program is free software; you can redistribute it and/or
 * modify it under the terms of the GNU General Public License
 * as published by the Free Software Foundation;
 * This application is distributed in the hope that it will  be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.
 * See the GNU General Public License for more details.
 * Licensed under GNU General Public License 3.0 or later.
 * @license GPL-3.0+ <http://spdx.org/licenses/GPL-3.0+>
 */

package rflink

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// SensorData represents one message received from RFLink.
// A friendly name can be added to the data so that the Publisher can tag it
// before sending it to the MQTT topic.
type SensorData struct {
	Hwid         string   `json:"hwid"`
	Model        string   `json:"model"`
	Id           string   `json:"id"`
	FriendlyName string   `json:"name,omitempty"`
	Temperature  *float32 `json:"temp,omitempty"`
	Humidity     *uint16  `json:"hum,omitempty"`
	Bat          string   `json:"bat,omitempty"`
	Switch       string   `json:"switch,omitempty"`
	Cmd          string   `json:"cmd,omitempty"`
	Timestamp    string   `json:"timestamp"`
}

// SensorDataFromMessage crafts a SensorData struct from a message read from
// RFLink
func SensorDataFromMessage(msg string) (*SensorData, error) {
	pieces := strings.Split(msg, ";")

	sd := SensorData{
		Model: strings.Replace(pieces[2], " ", "_", -1),
	}
	for i := 3; i < len(pieces); i++ {
		arr := strings.SplitN(pieces[i], "=", 2)
		switch arr[0] {
		case "ID":
			sd.Id = arr[1]
			sd.Hwid = strings.ToUpper(sd.Model + "_" + sd.Id)
			sd.Timestamp = time.Now().Format(time.RFC1123Z)
		case "TEMP":
			t, err := strToUint16(arr[1], 16)
			if err != nil {
				return nil, errors.New("skipping message, temperature could not be parsed")
			}
			temp := float32(t) / 10
			sd.Temperature = &temp
		case "HUM":
			h, err := strToUint16(arr[1], 10)
			if err != nil {
				return nil, errors.New("skipping message, humidity could not be parsed")
			}
			sd.Humidity = &h
		case "BAT":
			sd.Bat = arr[1]
		case "SWITCH":
			sd.Switch = arr[1]
		case "CMD":
			sd.Cmd = arr[1]
		}
	}
	// Not temp sensor - all others (not using now)
	if sd.Temperature == nil && sd.Humidity == nil {
		return &sd, nil
	}

	return &sd, nil
}

// String outputs a string representing the SensorData
func (sd *SensorData) String() string {
	format := "%s [%s]:"
	args := []interface{}{
		sd.Model,
		sd.Id,
		sd.Bat,
		sd.Switch,
		sd.Cmd,
	}

	if sd.Temperature != nil {
		format += " temp=%.1f°C"
		args = append(args, *sd.Temperature)
	}

	if sd.Humidity != nil {
		format += " hum=%d%%"
		args = append(args, *sd.Humidity)
	}

	return fmt.Sprintf(format, args...)
}
