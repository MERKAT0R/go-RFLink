[![Go Reference](https://pkg.go.dev/badge/github.com/MERKAT0R/go-rflink/rflink.svg)](https://pkg.go.dev/github.com/MERKAT0R/go-rflink/rflink)
[![Go Report Card](https://goreportcard.com/badge/github.com/MERKAT0R/go-rflink)](https://goreportcard.com/report/github.com/MERKAT0R/go-rflink)

# Beta state
All seams working fine ;)
Sending(broadcast to 433) from MQTT 2 RFLink - soon
# go-rflink

Inspired by [Pgaxatte](https://github.com/pgaxatte/go-rflink/)

Publish [RFLink](https://www.rflink.nl/) Gateway all messages (temperature and humidity sensors measurement humanreadable and auto to Celsius) to an MQTT topic.

## Difference

Replaced abandoned packets, for ex now main MQTT packet is eclipse/paho.mqtt
So added such features like connectionretry, autoreconnect and etc
Other improvements

## Installation

```bash
go get -u github.com/MERKAT0R/go-rflink
```

This library now support go.mod, so with go.mod

```bash
import github.com/MERKAT0R/go-rflink/rflink
```

## Usage

Optional environment variable can be used to override the default configuration, for example:

```bash
PUBLISH_HOST=192.168.0.1:1883 SERIAL_DEVICE=/dev/ttyACM0 go run main.go
```

See more at env.sh.example

### Within a docker container

It is possible to build go-rflink as a container and run it:

```bash
docker build - < Dockerfile

docker run \
    --device=/dev/ttyACM0 \
    --env PUBLISH_HOST="192.168.0.1:1883" \
    --env PUBLISH_TOPIC="myrflink" \
    go-rflink:latest
```

# TODO

- [ ] Features & Bugs ;)
- [ ] Minor: MQTT TLS
