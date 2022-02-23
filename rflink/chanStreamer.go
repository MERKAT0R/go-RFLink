package rflink

type Event struct {
	// Events are pushed to this channel by the main events-gathering routine
	Message chan string
}

// Initialize event and Start procnteessing requests
func NewServer() (event *Event) {
	event = &Event{
		Message: make(chan string),
	}
	return
}
