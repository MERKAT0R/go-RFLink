package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	//Signal
	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

	<-done
	fmt.Print("Interruption signal received")
	_, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer func() {
		// Disconnect the rf-link serial and network Connection.
		//	Close()
		//	Disconnect()
		cancel()
	}()

	log.Println("Go_RF-Link Terminated ::", time.Now())

}
