PI_HOST ?= pi@raspberrypi.local
PI_CONFIG_DIR ?= /etc/tfi-display
BINARY := build/tfi-display

.PHONY: build build-pi deploy test clean

# Build for the current host (useful for -mock runs on a laptop).
build:
	go build -o $(BINARY) ./...

# Cross-compile for Raspberry Pi Zero 2W (ARM64 Linux).
build-pi:
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
	  go build -ldflags="-s -w" -o $(BINARY)-linux-arm64 ./...

# Run unit tests (no hardware required).
test:
	go test ./...

# Deploy: copy binary + service file, enable and start.
deploy: build-pi
	ssh $(PI_HOST) "sudo systemctl stop tfi-display 2>/dev/null || true"
	scp $(BINARY)-linux-arm64 $(PI_HOST):/usr/local/bin/tfi-display
	scp tfi-display.service $(PI_HOST):/etc/systemd/system/
	ssh $(PI_HOST) "sudo mkdir -p $(PI_CONFIG_DIR)"
	@if [ -f config.yaml ]; then \
	  scp config.yaml $(PI_HOST):$(PI_CONFIG_DIR)/config.yaml; \
	fi
	ssh $(PI_HOST) "sudo systemctl daemon-reload && \
	               sudo systemctl enable --now tfi-display && \
	               sudo systemctl status tfi-display --no-pager"

# Run mock display locally (writes PNG frames to mock_output/).
run-mock:
	go run ./... -mock -config config.yaml.example

clean:
	rm -rf build/ mock_output/
