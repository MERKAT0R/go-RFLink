package rflink

import (
	"fmt"
	mqtt "github.com/eclipse/paho.mqtt.golang"
	jsoniter "github.com/json-iterator/go"
	"os"
)

// Define more fast JSON lib then default
var json = jsoniter.ConfigCompatibleWithStandardLibrary

var debug = func() bool {
	// Define if DEBUG enabled
	if os.Getenv("GORFLINK_DEBUG") != "true" {
		return false
	}
	return true
}

type rfLink struct {
	rfLink Publisher
}

// Publisher takes input from a SensorReader and publishes the SensorData that
// has been read in an MQTT topic
type Publisher struct {
	c mqtt.Client

	Topic       string
	SensorInput *SensorReader
}

var connectHandler mqtt.OnConnectHandler = func(client mqtt.Client) {
	fmt.Println("Go_RF-Link Connected to MQTT")
}

var connectLostHandler mqtt.ConnectionLostHandler = func(client mqtt.Client, err error) {
	fmt.Printf("Go_RF-Link Lost Connection : %v", err)
}

// NewPublisher return a Publisher according to the options specified
func NewPublisher(o *Options) (*Publisher, error) {
	opts := mqtt.NewClientOptions()
	opts.AddBroker(fmt.Sprintf("%s://%s", o.Publish.Scheme, o.Publish.Host))
	opts.SetClientID(fmt.Sprintf("%s", o.Publish.ClientID))
	opts.SetUsername(fmt.Sprintf("%s", o.Publish.MqttUsername))
	opts.SetPassword(fmt.Sprintf("%s", o.Publish.MqttPassword))
	opts.ProtocolVersion = o.Publish.ProtocolVersion
	opts.CleanSession = true
	opts.OnConnect = connectHandler
	opts.OnConnectionLost = connectLostHandler
	cli := mqtt.NewClient(opts)
	if token := cli.Connect(); token.Wait() && token.Error() != nil {
		return nil, token.Error()
	}

	p := &Publisher{
		c:     cli,
		Topic: o.Publish.Topic,
	}
	return p, nil
}

// Publish formats the input SensorData into JSON and publishes it to the
// configured MQTT topic
func (p *Publisher) Publish(sd *SensorData) error {
	b, err := json.Marshal(sd)
	if err != nil {
		return err
	}
	if debug() {
		fmt.Print(string(b))
	}
	//Publish(topic string, qos byte, retained bool, payload interface{}) Token
	token := p.c.Publish(p.Topic, 0, false, b)
	token.Wait()
	err = token.Error()
	if err != nil {
		return err
	}
	return nil
}

// ReadAndPublish loops infinitely to read SensorData from the SensorReader and
// publish the output via to Publish() method
func (p *Publisher) ReadAndPublish() error {
	for {
		sd, err := p.SensorInput.ReadNext()
		if err != nil {
			if debug() {
				fmt.Print(err)
			}
			continue
		}

		err = p.Publish(sd)
		if err != nil {
			if debug() {
				fmt.Print(err)
			}
			continue
		}
	}
}

// Disconnect properly disconnects the MQTT network connection
func (p *rfLink) Disconnect() interface{} {
	p.rfLink.c.Disconnect(1000)
	return nil
}
