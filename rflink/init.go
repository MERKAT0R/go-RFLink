package rflink

import (
	"fmt"
)

func GoRFLinkInit() error {
	// Parse options
	opts := GetOptions()
	// Setup the MQTT publisher
	p, err := NewPublisher(opts)
	if err != nil {
		return err
	}
	if debug() {
		fmt.Print("Go_RF-Link MQTT publisher created")
	}
	// Setup the sensor reader
	sr, err := NewSensorReader(opts)
	if err != nil {
		return err
	}
	defer sr.Close()
	if debug() {
		fmt.Print("Go_RF-Link Sensor reader created")
	}
	// Start reading/publishing loop
	p.SensorInput = sr
	go p.ReadAndPublish()
	return nil
}
