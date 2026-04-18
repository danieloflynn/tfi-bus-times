package main

import (
	"log"

	"tfi-display/updater"
)

func main() {
	cfg, err := updater.DefaultConfig()
	if err != nil {
		log.Fatalf("initialising updater config: %v", err)
	}

	if err := updater.Run(cfg); err != nil {
		log.Fatalf("update failed: %v", err)
	}
	log.Println("update complete")
}
