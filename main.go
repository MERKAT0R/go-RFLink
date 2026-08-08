package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/MERKAT0R/go-rflink/rflink"
)

func main() {
	// os.Interrupt — Ctrl+C on Windows and Unix (SIGINT)
	// syscall.SIGTERM — kill / systemd / docker stop (Unix; Windows)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	app, err := rflink.Init()
	if err != nil {
		log.Fatal(err)
	}

	<-ctx.Done()
	stop()
	log.Printf("shutdown signal received, stopping…")

	done := make(chan struct{})
	go func() {
		app.Stop()
		close(done)
	}()

	select {
	case <-done:
		log.Printf("shutdown complete")
	case <-time.After(15 * time.Second):
		log.Printf("shutdown timed out")
		os.Exit(1)
	}
}
