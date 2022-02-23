package rflink

import (
	"crypto/tls"
	"fmt"
	mqtt "github.com/eclipse/paho.mqtt.golang"
	"net/url"
	"os"
)

// Publisher takes input from a SensorReader and publishes the SensorData that
// has been read in an MQTT topic
type Publisher struct {
	C mqtt.Client

	Topic       string
	SensorInput *SensorReader
}

var connectHandler mqtt.OnConnectHandler = func(client mqtt.Client) {
	fmt.Println("Go_RF-Link Connected to MQTT")
}

var connectionAttemptHandler mqtt.ConnectionAttemptHandler = func(broker *url.URL, tlsCfg *tls.Config) *tls.Config {
	if debug() {
		fmt.Printf("Go_RF-Link Connecting to MQTT: %s://%s  with ID: %s and protocolVersion: %s \n",
			os.Getenv("PUBLISH_SCHEME"),
			os.Getenv("PUBLISH_HOST"),
			os.Getenv("PUBLISH_CLIENTID"),
			os.Getenv("PUBLISH_PROTOCOLVERSION"))
	} else {
		fmt.Println("Go_RF-Link Connecting to MQTT")
	}
	return nil
}

var connectLostHandler mqtt.ConnectionLostHandler = func(client mqtt.Client, err error) {
	fmt.Printf("Go_RF-Link Lost Connection : %v", err)
}

var reconnectHandler mqtt.ReconnectHandler = func(client mqtt.Client, co *mqtt.ClientOptions) {
	fmt.Println("Go_RF-Link MQTT Reconnecting")
}

// NewPublisher return a Publisher according to the options specified
func NewPublisher(o *Options) (*Publisher, error) {
	opts := mqtt.NewClientOptions()
	opts.AddBroker(fmt.Sprintf("%s://%s", o.Publish.Scheme, o.Publish.Host))
	opts.SetClientID(fmt.Sprintf("%s", o.Publish.ClientID))
	opts.SetUsername(fmt.Sprintf("%s", o.Publish.MqttUsername))
	opts.SetPassword(fmt.Sprintf("%s", o.Publish.MqttPassword))
	opts.ProtocolVersion = o.Publish.ProtocolVersion
	if o.Publish.InfinityReconnect {
		opts.ConnectRetry = true
		fmt.Println("Go_RF-Link MQTT Connecting setup set to infinity")
		fmt.Println("Go_RF-Link Connecting errors suppressed until first successful connect")
	}
	opts.CleanSession = true
	opts.OnConnectAttempt = connectionAttemptHandler
	opts.OnConnect = connectHandler
	opts.OnConnectionLost = connectLostHandler
	opts.OnReconnecting = reconnectHandler
	cli := mqtt.NewClient(opts)
	if token := cli.Connect(); token.Wait() && token.Error() != nil {
		panic(token.Error())
	}
	p := &Publisher{
		C:     cli,
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
	token := p.C.Publish(p.Topic, 0, false, b)
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
func (p *Publisher) Disconnect() {
	p.C.Disconnect(1000)
}
