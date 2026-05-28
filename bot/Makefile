BINARY  := bot
PI_HOST := pi@10.0.1.225
PI_DIR  := ~/polymarket-session-dashboard/bot

# Ensure Homebrew Go is on the PATH (needed when make is invoked from an IDE
# or shell where /opt/homebrew/bin isn't in PATH yet).
export PATH := /opt/homebrew/bin:/usr/local/go/bin:$(PATH)

.PHONY: build build-pi build-vps deploy-pi tidy clean

## Local (Mac) build — useful for verifying the code compiles
build:
	go build -o $(BINARY) ./cmd

## Cross-compile for Raspberry Pi 5 (ARM64 Linux)
build-pi:
	GOOS=linux GOARCH=arm64 go build -o $(BINARY)-pi ./cmd

## Cross-compile for VPS (x86-64 Linux)
build-vps:
	GOOS=linux GOARCH=amd64 go build -o $(BINARY)-vps ./cmd

## Build for Pi, copy binary, and restart via pm2 ecosystem config
deploy-pi: build-pi
	scp $(BINARY)-pi $(PI_HOST):$(PI_DIR)/$(BINARY)-pi
	ssh $(PI_HOST) "cd $(PI_DIR) && \
	  ~/.npm-global/bin/pm2 restart ecosystem.config.cjs 2>/dev/null || \
	  ~/.npm-global/bin/pm2 start ecosystem.config.cjs && \
	  ~/.npm-global/bin/pm2 save"

## Download / tidy dependencies
tidy:
	go mod tidy

clean:
	rm -f $(BINARY) $(BINARY)-pi $(BINARY)-vps
