package main

import (
	"context"
	"fmt"
	"github.com/MERKAT0R/go-rflink/rflink"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	//Signal
	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)
	err := rflink.GoRFLinkInit()
	if err != nil {
		fmt.Print(err)
	}
	<-done
	fmt.Println("Go_RF-Link Interruption signal received")
	_, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer func() {
		// Disconnect the rf-link serial and network Connection.
		var rfmqtt rflink.Publisher
		var rfserial rflink.SensorReader
		rfmqtt.Disconnect()
		err := rfserial.Close()
		if err != nil {
			fmt.Printf("Go_RF-Link Serial Disconnect Error: %s \n", err)
		}
		cancel()
	}()

	fmt.Println("Go_RF-Link Terminated :: ", time.Now())

}
