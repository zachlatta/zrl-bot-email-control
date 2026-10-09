.PHONY: build fmt run test

build:
	go build -o zrl-bot-email ./cmd/zrl-bot-email

fmt:
	gofmt -w $$(find . -name '*.go' -not -path './vendor/*')

run:
	go run ./cmd/zrl-bot-email serve

test:
	test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)
	go vet ./...
	go test -race ./...
