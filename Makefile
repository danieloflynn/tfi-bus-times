PI_HOST ?= pi@192.168.0.1
PI_CONFIG_DIR ?= /etc/tfi-display
BINARY := build/tfi-display
AGENT_BINARY := build/tfi-agent

.PHONY: build build-pi build-agent-pi deploy deploy-agent test clean preview run-mock

# Build for the current host (useful for -mock runs on a laptop).
build:
	go build -o $(BINARY) .

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
	$(if $(wildcard secrets.yaml.example),scp secrets.yaml.example $(PI_HOST):/tmp/secrets.yaml.example && ssh $(PI_HOST) "sudo mv /tmp/secrets.yaml.example $(PI_CONFIG_DIR)/secrets.yaml.example",)
	ssh $(PI_HOST) "sudo systemctl daemon-reload && \
	               sudo systemctl enable --now tfi-display && \
	               sudo systemctl status tfi-display --no-pager"

# Cross-compile the agent for Raspberry Pi Zero 2W (ARM64 Linux).
# The API origin is not baked in — set base_url in /etc/tfi-display/secrets.yaml.
build-agent-pi: export GOOS=linux
build-agent-pi: export GOARCH=arm64
build-agent-pi: export CGO_ENABLED=0
build-agent-pi:
	go build -ldflags="-s -w" -o $(AGENT_BINARY)-linux-arm64 ./cmd/agent

# Bootstrap/refresh the agent: install the display binary + the agent, register
# and start the long-running tfi-agent service. From then on the agent keeps the
# display binary and config in sync on its own — no further SSH needed.
deploy-agent: build-pi build-agent-pi
	scp $(BINARY)-linux-arm64 $(PI_HOST):/tmp/tfi-display
	ssh $(PI_HOST) "sudo install -m 0755 /tmp/tfi-display /usr/local/bin/tfi-display"
	scp $(AGENT_BINARY)-linux-arm64 $(PI_HOST):/tmp/tfi-agent
	ssh $(PI_HOST) "sudo install -m 0755 /tmp/tfi-agent /usr/local/bin/tfi-agent"
	scp tfi-agent.service $(PI_HOST):/tmp/tfi-agent.service
	ssh $(PI_HOST) "sudo mv /tmp/tfi-agent.service /etc/systemd/system/ && sudo systemctl daemon-reload && sudo systemctl enable --now tfi-agent && sudo systemctl status tfi-agent --no-pager"

# Run mock display locally (writes PNG frames to mock_output/).
# TFI_API_KEY=dummy avoids needing api_key in config.yaml.example.
run-mock:
	TFI_API_KEY=dummy go run . -mock -config config.yaml.example

# Render a preview PNG using fixture data (no API key needed).
preview:
	go test ./display/ -run TestRenderPreview -v -count=1

clean:
	rm -rf build/ mock_output/
