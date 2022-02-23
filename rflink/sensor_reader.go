package rflink

import (
	"bufio"
	"fmt"
	"go.bug.st/serial"
	"time"
)

// SensorReader reads SensorData from the serial connection with RFLink
type SensorReader struct {
	port   serial.Port
	reader *bufio.Scanner
}

// NewSensorReader returns a SensorReader according to the options specified
func NewSensorReader(o *Options) (*SensorReader, error) {
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
	port, err := serial.Open(o.Serial.Device, &serial.Mode{
		BaudRate: o.Serial.Baud,
	})
	if err != nil {
		return nil, err
	}
	err = port.SetReadTimeout(time.Second * 1)
	if err != nil {
		return nil, err
	}
	sr := &SensorReader{
		port:   port,
		reader: bufio.NewScanner(port),
	}
	return sr, nil
}

// ReadNext reads a line from RFLink and returns it in the form of a SensorData
// struct
func (sr *SensorReader) ReadNext() (*SensorData, error) {
	var line string
	for sr.reader.Scan() {
		line = sr.reader.Text()
	}
	//	line, _, err := sr.reader.Scan()
	if err := sr.reader.Err(); err != nil {
		if debug() {
			fmt.Printf("Cannot read from serial: %s \n", err)
		}
		return nil, err
	}

	sd, err := SensorDataFromMessage(line)
	if err != nil {
		if debug() {
			fmt.Printf("Error parsing message from rflink \"%s\": %s \n", line, err)
		}
		return nil, err
	}

	return sd, nil
}

// Close closes the serial port
func (sr *SensorReader) Close() error {
	err := sr.port.Close()
	if err != nil {

		return err
	}
	return nil
}
