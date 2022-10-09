/*
 * Copyright (c) 2022.  by MERKATOR <merkator@merkator.pro>
 * This program is free software; you can redistribute it and/or
 * modify it under the terms of the GNU General Public License
 * as published by the Free Software Foundation;
 * This application is distributed in the hope that it will  be useful, but WITHOUT ANY WARRANTY; without even the implied warranty  * of MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the GNU General Public License for more details.
 * Licensed under GNU General Public License 3.0 or later.
 * @license GPL-3.0+ <http://spdx.org/licenses/GPL-3.0+>
 */

package rflink

import (
	"fmt"
	"go.bug.st/serial"
	"time"
)

// SerialReader reads SensorData from the serial connection with RFLink
type SerialReader struct {
	port serial.Port
}

// NewSerialReader  returns a SensorReader according to the options specified
func NewSerialReader(o *Options) (*SerialReader, error) {
	if debug() {
		ports, err := serial.GetPortsList()
		if err != nil {
			fmt.Printf("Can`t Get serial ports: %s \n", err)
		}
		if len(ports) == 0 {
			fmt.Println("No serial ports found!")
		}
		// Print the list of detected ports
		for _, port := range ports {
			fmt.Printf("Found port: %v\n", port)
		}
	}
	// Serial open
	port, err := serial.Open(o.Serial.Device, &serial.Mode{
		BaudRate: 57600, // Fixed by Design
		Parity:   serial.NoParity,
		DataBits: 8,
		StopBits: serial.OneStopBit,
	})
	if err != nil {
		return nil, err
	}
	err = port.SetReadTimeout(time.Second * 10)
	if err != nil {
		return nil, err
	}
	sr := &SerialReader{
		port: port,
	}
	return sr, nil
}

// Close closes the serial port
func (sr *SerialReader) Close() error {
	err := sr.port.Close()
	if err != nil {

		return err
	}
	return nil
}
