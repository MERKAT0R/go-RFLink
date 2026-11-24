module github.com/MERKAT0R/go-rflink

go 1.27.4

require (
	github.com/caarlos0/env/v11 v11.4.2
	github.com/eclipse/paho.golang v0.24.0
	github.com/goccy/go-json v0.12.0
	github.com/gorilla/websocket v1.5.3
	go.bug.st/serial v1.8.0
	golang.org/x/net v0.63.0
)

require (
	github.com/stretchr/testify v1.12.1 // indirect
	go.yaml.in/yaml/v3 v3.0.6 // indirect
	golang.org/x/sys v0.50.0 // indirect
)

// Obsolet
retract (
	v0.0.0-20220223200338-f752fdb4f31d
	v0.0.0-20220223193413-6e5b53c426bf
)
