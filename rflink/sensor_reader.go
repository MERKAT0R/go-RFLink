package rflink

import (
	"fmt"
	"go.bug.st/serial"
	"log"
	"strings"
	"time"
)

// SensorReader reads SensorData from the serial connection with RFLink
type SensorReader struct {
	port   serial.Port
	reader string
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

	buff := make([]byte, 100)
	var str string
	for {
		// Reads up to 100 bytes
		n, err := port.Read(buff)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("%s", string(buff[:n]))
		str = string(buff[:n])

		// If we receive a newline stop reading
		if strings.Contains(string(buff[:n]), "\n") {
			break
		}
	}
	sr := &SensorReader{
		port:   port,
		reader: str,
	}
	return sr, nil
}

// ReadNext reads a line from RFLink and returns it in the form of a SensorData
// struct
func (sr *SensorReader) ReadNext() (*SensorData, error) {
	sd, err := SensorDataFromMessage(sr.reader)
	if err != nil {
		if debug() {
			fmt.Printf("Error parsing message from rflink \"%s\": %s \n", sr.reader, err)
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
