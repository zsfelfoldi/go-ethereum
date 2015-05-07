package main

import (
	"os"
	"syscall"
	"time"
)

// Returns when mist is stopped (or crashed)
func waitForMist() {
	for {
		timeout := time.After(1 * time.Second)
		select {
		case <-timeout:
			proc, err := os.FindProcess(os.Getppid())
			if err != nil {
				return
			}

			err = proc.Signal(syscall.Signal(0))
			if err != nil {
				return
			}
		}
	}
}
