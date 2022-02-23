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
		fmt.Println(err)
	}
	<-done
	fmt.Println("Go_RF-Link Interruption signal received")
	_, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer func() {
		// Something to close on exit
		cancel()
	}()

	fmt.Println("Go_RF-Link Terminated :: ", time.Now())

}
