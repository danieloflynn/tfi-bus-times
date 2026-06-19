package main

import (
	"context"
	"flag"
	"log"
	"os/signal"
	"syscall"

	"tfi-display/agent"
)

func main() {
	configPath := flag.String("config", "/etc/tfi-display/config.yaml", "path to config.yaml")
	secretsPath := flag.String("secrets", "/etc/tfi-display/secrets.yaml", "path to secrets.yaml")
	flag.Parse()

	a, err := agent.New(*configPath, *secretsPath)
	if err != nil {
		log.Fatalf("initialising agent: %v", err)
	}

	// systemd stops the service with SIGTERM; exit the loop cleanly on it.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := a.Run(ctx); err != nil {
		log.Fatalf("agent exited with error: %v", err)
	}
}
