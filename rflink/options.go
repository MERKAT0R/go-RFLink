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
		Device string `envconfig:"default=/dev/ttyUSB0"`
		Baud   int    `envconfig:"default=57600"`
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
