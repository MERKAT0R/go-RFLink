/*
 * Copyright (c) 2022.  by MERKATOR <merkator@merkator.pro>
 * This program is free software; you can redistribute it and/or
 * modify it under the terms of the GNU General Public License
 * as published by the Free Software Foundation;
 * This application is distributed in the hope that it will  be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.
 * See the GNU General Public License for more details.
 * Licensed under GNU General Public License 3.0 or later.
 * @license GPL-3.0+ <http://spdx.org/licenses/GPL-3.0+>
 */

package rflink

import (
	"crypto/tls"
	"fmt"
	mqtt "github.com/eclipse/paho.mqtt.golang"
	"net/url"
)

// Publisher takes input from a SensorReader and publishes the SensorData that
// has been read in an MQTT topic
type Publisher struct {
	C     mqtt.Client
	Topic string
}

var connectHandler mqtt.OnConnectHandler = func(client mqtt.Client) {
	fmt.Println("Go_RF-Link Connected to MQTT")
}

var connectionAttemptHandler mqtt.ConnectionAttemptHandler = func(broker *url.URL, tlsCfg *tls.Config) *tls.Config {
	if debug() {

		fmt.Printf("Go_RF-Link Connecting to MQTT: %s \n",
			broker.String())
	} else {
		fmt.Println("Go_RF-Link Connecting to MQTT")
	}
	return nil
}

var connectLostHandler mqtt.ConnectionLostHandler = func(client mqtt.Client, err error) {
	fmt.Printf("Go_RF-Link Lost Connection : %v \n", err)
}

var reconnectHandler mqtt.ReconnectHandler = func(client mqtt.Client, co *mqtt.ClientOptions) {
	fmt.Println("Go_RF-Link MQTT Reconnecting")
}

// Not implemented yet
/*var messageHandler mqtt.MessageHandler = func(client mqtt.Client, msg mqtt.Message) {
	// fixme
}
*/
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

// Not implemented yet fixme
/*func (p *Publisher) ReadAndBroadcast(sd *SensorData) error {
	token := p.C.Subscribe(p.Topic, 0, messageHandler)
	token.Wait()
	err := token.Error()
	if err != nil {
		return err
	}
	return nil
}*/

// ReadAndPublish loops infinitely to read SensorData from the SensorReader and
// publish the output via to Publish() method
func (p *Publisher) ReadAndPublish(rflinkChans *rflinkChannels) {
	for {
		if len(rflinkChans.SerialChannel.Message) > 0 {
			if msg, ok := <-rflinkChans.SerialChannel.Message; ok {
				sd, err := SensorDataFromMessage(msg)
				if err != nil {
					if debug() {
						fmt.Printf("Error parsing message from rflink \"%s\": %s \n", msg, err)
					}
				}
				err = p.Publish(sd)
				if err != nil {
					if debug() {
						fmt.Printf("Error publishing message from rflink \"%s\" \n", err)
					}
					continue
				}
			}
			//continue
			if len(rflinkChans.SensorChannel.Error) > 0 {
				err := <-rflinkChans.SensorChannel.Error
				//err :=  errors.New("dsff")
				if err != nil {
					if debug() {
						fmt.Print(err)
					}
					continue
				}
			}

		}
	}

}

// Disconnect properly disconnects the MQTT network connection
func (p *Publisher) Disconnect() {
	p.C.Disconnect(1000)
}

func test() {
	//Publish()
}
