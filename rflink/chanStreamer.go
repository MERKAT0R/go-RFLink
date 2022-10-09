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

// rflinkChannels Struct for all channels
type rflinkChannels struct {
	SerialChannel *SerialChannel
	SensorChannel *SensorChannel
}

// SerialChannel Struct for Channels of Serial messages
type SerialChannel struct {
	Message chan string
	Error   chan error
}

// SensorChannel Struct for Channels of Sensor messages
type SensorChannel struct {
	Message chan *SensorData
	Error   chan error
}

// NewSerialChannels Initialization
func NewSerialChannels() (event *SerialChannel) {
	event = &SerialChannel{
		Message: make(chan string, 10), //serial data, buffered to 10
		Error:   make(chan error),      //error
	}
	return event
}

// NewSensorChannels Initialization
func NewSensorChannels() (event *SensorChannel) {
	event = &SensorChannel{
		Message: make(chan *SensorData, 10), //sensor data, buffered to 10
		Error:   make(chan error),           //error
	}
	return event
}

// initrflinkChannels Start all goRFlink channels
func initrflinkChannels() (event *rflinkChannels) {
	event = &rflinkChannels{
		SerialChannel: NewSerialChannels(), //Serial Grub
		SensorChannel: NewSensorChannels(), //Sensor Grub
	}
	return event
}

func SendData(data chan *SensorData, p *SensorData) {
	data <- p
}

func SendError(err chan error, error error) {
	err <- error
}
