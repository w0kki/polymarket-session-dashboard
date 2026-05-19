BINARY := bot

.PHONY: build build-pi build-vps run tidy clean

## Local (Mac) build for testing
build:
	go build -o $(BINARY) ./cmd

## Cross-compile for Raspberry Pi 5 (ARM64 Linux)
build-pi:
	GOOS=linux GOARCH=arm64 go build -o $(BINARY)-pi ./cmd

## Cross-compile for VPS (x86-64 Linux)
build-vps:
	GOOS=linux GOARCH=amd64 go build -o $(BINARY)-vps ./cmd

## Run locally in paper-trading mode
run:
	DRY_RUN=true DB_PATH=../trades.db go run ./cmd

## Download dependencies
tidy:
	go mod tidy

clean:
	rm -f $(BINARY) $(BINARY)-pi $(BINARY)-vps
