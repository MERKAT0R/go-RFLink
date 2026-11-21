# go-rflink Home Assistant add-on

Local add-on template. The header badge in the main README adds this git repository to Home Assistant. [`repository.yaml`](../../repository.yaml) is required at the repository root, and the Supervisor loads `config.yaml` from this folder.

Copy `deploy/homeassistant` into `/addons/go_rflink` when you want it only as a local add-on, then build it from the add-on store.

The image expects the published `ghcr.io/merkat0r/go-rflink` binary. Set the serial device to the UART HA exposes, and point MQTT at the Mosquitto add-on (`core-mosquitto`) or another broker.

`serial_device` accepts a UART path or `tcp://host:port` for a networked RFLink. Extra gateways go in `serial_devices`, comma-separated. `serial_command_devices` limits which of those ports receive commands; leave it empty to send to every connected port. With more than one gateway, a sensor follows the gateway that last delivered a new frame, after a short hold so two radios hearing the same burst do not swap. Buttons and switches stay available while any command target is connected.
