.PHONY: build
build:
	@go build -o ./bin/btc ./cmd

.PHONY: run
run: build
	./bin/btc -file=sample.torrent -path=./download