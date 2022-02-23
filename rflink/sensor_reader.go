package rflink

import (
	"fmt"
	"go.bug.st/serial"
	"strings"
	"time"
)

// SerialReader reads SensorData from the serial connection with RFLink
type SerialReader struct {
	port serial.Port
	data *SensorReader
}
type SensorReader struct {
	SensorReader string
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
	sr := &SerialReader{
		port: port,
	}
	c := make(chan SensorReader)
	buff := make([]byte, 100)

	go func() {
		for {
			// Reads up to 100 bytes
			n, err := port.Read(buff)
			if err != nil {
				fmt.Println(err)
			}
			//	fmt.Printf("%s", string(buff[:n]))

			c <- SensorReader{string(buff[:n])}
			// If we receive a newline stop reading
			if strings.Contains(string(buff[:n]), "\n") {
				break
			}
		}
	}()

	return sr, nil
}

// ReadNext reads a line from RFLink and returns it in the form of a SensorData
// struct
func (sr *SensorReader) ReadNext() (*SensorData, error) {
	sd, err := SensorDataFromMessage(sr.SensorReader)
	fmt.Println(sd)
	if err != nil {
		if debug() {
			fmt.Printf("Error parsing message from rflink \"%s\": %s \n", sr.SensorReader, err)
		}
		return nil, err
	}
	return sd, nil

}

// Close closes the serial port
func (sr *SerialReader) Close() error {
	err := sr.port.Close()
	if err != nil {

		return err
	}
	return nil
}
