.PHONY: generate go check

generate:
	PATH="$(CURDIR)/.tools/bin:$$PATH" protoc --go_out=. --go_opt=paths=source_relative --go-grpc_out=. --go-grpc_opt=paths=source_relative api/sandbox/v1/sandbox.proto

go:
	mkdir -p bin
	go build -o bin/sbx ./cmd/sbx

check:
	go test -race ./...
