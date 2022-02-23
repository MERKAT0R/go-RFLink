package rflink

import (
	"fmt"
	jsoniter "github.com/json-iterator/go"
	"os"
)

// Define more fast JSON lib then default
var json = jsoniter.ConfigCompatibleWithStandardLibrary

var debug = func() bool {
	// Define if DEBUG enabled
	if os.Getenv("GORFLINK_DEBUG") != "true" {
		return true //fixme
	}
	return true
}

// GoRFLinkInit Entry point for calling
func GoRFLinkInit() error {
	if debug() {
		fmt.Println("*** Go_RF-Link in DEBUG mode ***")
	}
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
	defer p.Disconnect()
	// Setup the sensor reader
	sr, err := NewSerialReader(opts)
	if err != nil {
		return err
	}
	defer func(sr *SerialReader) {
		err := sr.Close()
		if err != nil {
			fmt.Printf("Go_RF-Link Serial Disconnect Error: %s \n", err)
		}
	}(sr)
	if debug() {
		fmt.Print("Go_RF-Link Sensor reader created")
	}
	// Start reading/publishing loop
	p.SensorInput = sr.data
	go p.ReadAndPublish()
	return nil
}
