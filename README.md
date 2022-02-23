[![Go Reference](https://pkg.go.dev/badge/github.com/MERKAT0R/go-rflink/rflink.svg)](https://pkg.go.dev/github.com/MERKAT0R/go-rflink/rflink)
[![Build Status](https://app.travis-ci.com/MERKAT0R/go-RFLink.svg?branch=master)](https://app.travis-ci.com/MERKAT0R/go-RFLink)

# Under dev!

# go-rflink
Inspired by [Pgaxatte](https://github.com/pgaxatte/go-rflink/)

Publish [RFLink](https://www.rflink.nl/) Gateway temperature and humidity measurement to an MQTT topic.
  Other will be added soon, including reading from MQTT and sending to rf-link gate

## Difference
 Replaced abandoned packets, for ex now main MQTT packet is eclipse/paho.mqtt
 So added such features like connectionretry, autoreconnect and etc
 Other improvements

## Installation

```bash
go get -u github.com/MERKAT0R/go-rflink
```
Or with go.mod
```bash
This library now support go.mod with the import github.com/MERKAT0R/go-rflink/rflink
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
docker build -t go-rflink .

docker run \
    --device=/dev/ttyACM0 \
    --env PUBLISH_HOST="192.168.0.1:1883" \
    --env PUBLISH_TOPIC="myrflink" \
    go-rflink:latest
```

# TODO
- [ ] Features & Bugs ;)
- [ ] Minor: MQTT TLS
