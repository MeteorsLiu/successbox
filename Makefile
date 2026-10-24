.PHONY: generate go apple check

generate:
	PATH="$(CURDIR)/.tools/bin:$$PATH" protoc --go_out=. --go_opt=paths=source_relative --go-grpc_out=. --go-grpc_opt=paths=source_relative api/sandbox/v1/sandbox.proto

go:
	mkdir -p bin
	go build -o bin/sbx ./cmd/sbx
	GOOS=linux GOARCH=arm64 go build -o bin/successbox-gvisor-linux-arm64 ./cmd/successbox-gvisor
	GOOS=linux GOARCH=amd64 go build -o bin/successbox-gvisor-linux-amd64 ./cmd/successbox-gvisor

apple:
	swift build --product successbox-apple
	mkdir -p bin
	cp .build/debug/successbox-apple bin/successbox-apple
	codesign --force --sign - --entitlements apple/entitlements.plist bin/successbox-apple

check:
	go test -race ./...
