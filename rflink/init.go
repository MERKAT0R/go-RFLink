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
	"bufio"
	"context"
	"fmt"
	jsoniter "github.com/json-iterator/go"
	"os"
)

// Define more fast JSON lib then default
var json = jsoniter.ConfigCompatibleWithStandardLibrary

var debug = func() bool {
	// Define if DEBUG enabled
	if os.Getenv("GORFLINK_DEBUG") != "true" {
		return false
	}
	return true
}

// GoRFLinkInit Entry point for calling
func GoRFLinkInit() error {
	if debug() {
		fmt.Println("*** Go_RF-Link in DEBUG mode ***")
	}
	// Parse options
	opts := GetOptions()
	// Setup the MQTT publisher
	p, err := NewPublisher(opts)
	if err != nil {
		return err
	}
	if debug() {
		fmt.Print("Go_RF-Link MQTT publisher created")
	}
	// Setup the serial reader
	sr, err := NewSerialReader(opts)
	if err != nil {
		return err
	}
	if debug() {
		fmt.Print("Go_RF-Link Serial reader created")
	}
	// Start Channels
	chans := initrflinkChannels()
	// Start incoming Serial messages Processing
	ctx, _ := context.WithCancel(context.Background())
	go func(ctx context.Context) {
		_, _ = sr.port.Write([]byte("10;version;\n\r"))
		for {
			scanner := bufio.NewScanner(sr.port)
			for scanner.Scan() {
				if debug() {
					fmt.Println("==== RAW scanner.Text() ==== \n")
					fmt.Println(scanner.Text())
					fmt.Println("==== RAW scanner.Text() END ====")
				}
				select {
				case chans.SerialChannel.Message <- scanner.Text(): // Send raw serial data to chan - buffered to 10
				default:
					fmt.Println("Serial Channel full. Discarding value") // At normal processing this should happen very-very rare
				}
			}
			// if execution reaches this point, something went wrong or stream was closed
			select {
			case <-ctx.Done():
				return // ctx was cancelled, just return without error
			default:
				fmt.Print(scanner.Err()) //Something goes very bad - rfLink disconnected?
			}
		}
	}(ctx)

	// Start reading/publishing loop
	go p.ReadAndPublish(chans)

	return err
}
