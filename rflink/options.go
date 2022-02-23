package rflink

import (
	"github.com/vrischmann/envconfig"
)

// Options stores the options needed to communicate with RFLink and the
// message queue
type Options struct {
	// MQTT options
	Publish struct {
		Host              string `envconfig:"default=localhost:1883"` // host:port
		Scheme            string `envconfig:"default=tcp"`
		MqttUsername      string `envconfig:"default=username"`
		MqttPassword      string `envconfig:"default=password"`
		ProtocolVersion   uint   `envconfig:"default=4"`
		InfinityReconnect bool   `envconfig:"default=true"`
		ClientID          string `envconfig:"default=gorflink"`
		Topic             string `envconfig:"default=rflink"`
	}

	// Serial connection options
	Serial struct {
		Device string `envconfig:"default=/dev/serial/by-id/usb-Prolific_Technology_Inc._USB-Serial_Controller_D-if00-port0"`
		Baud   int    `envconfig:"default=115200"`
	}
}

// GetOptions reads the options from the environment and returns an Options
// struct
func GetOptions() *Options {
	var opts Options
	if err := envconfig.Init(&opts); err != nil {
		panic(err)
	}
	return &opts
}
