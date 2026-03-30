PI_HOST ?= pi@192.168.0.1
PI_CONFIG_DIR ?= /etc/tfi-display
BINARY := build/tfi-display

.PHONY: build build-pi deploy test clean preview

# Build for the current host (useful for -mock runs on a laptop).
build:
	go build -o $(BINARY) ./...

# Cross-compile for Raspberry Pi Zero 2W (ARM64 Linux).
build-pi: export GOOS=linux
build-pi: export GOARCH=arm64
build-pi: export CGO_ENABLED=0
build-pi:
	go build -ldflags="-s -w" -o $(BINARY)-linux-arm64 .

# Run unit tests (no hardware required).
test:
	go test ./...

# Deploy: copy binary + service file, enable and start.
deploy: build-pi
	ssh $(PI_HOST) "sudo systemctl stop tfi-display 2>/dev/null || true"
	scp $(BINARY)-linux-arm64 $(PI_HOST):/tmp/tfi-display
	ssh $(PI_HOST) "sudo mv /tmp/tfi-display /usr/local/bin/tfi-display && sudo chmod +x /usr/local/bin/tfi-display"
	scp tfi-display.service $(PI_HOST):/tmp/tfi-display.service
	ssh $(PI_HOST) "sudo mv /tmp/tfi-display.service /etc/systemd/system/"
	ssh $(PI_HOST) "sudo mkdir -p $(PI_CONFIG_DIR)"
	$(if $(wildcard config.yaml),scp config.yaml $(PI_HOST):/tmp/config.yaml && ssh $(PI_HOST) "sudo mv /tmp/config.yaml $(PI_CONFIG_DIR)/config.yaml",)
	$(if $(wildcard config.yaml.example),scp config.yaml.example $(PI_HOST):/tmp/config.yaml.example && ssh $(PI_HOST) "sudo mv /tmp/config.yaml.example $(PI_CONFIG_DIR)/config.yaml.example",)
	ssh $(PI_HOST) "sudo systemctl daemon-reload && \
	               sudo systemctl enable --now tfi-display && \
	               sudo systemctl status tfi-display --no-pager"

# Run mock display locally (writes PNG frames to mock_output/).
run-mock:
	go run ./... -mock -config config.yaml.example

# Render a preview PNG using fixture data (no API key needed).
preview:
	go test ./display/ -run TestRenderPreview -v -count=1

clean:
	rm -rf build/ mock_output/
