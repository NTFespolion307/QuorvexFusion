VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/NTFespolion307/QuorvexFusion/internal/version.Version=$(VERSION)
# Pure-Go build (modernc SQLite), so cross-compiling needs no C toolchain.
export CGO_ENABLED := 0

.PHONY: build cross test vet proto clean

build:
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/cluster ./cmd/cluster

cross:
	GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '$(LDFLAGS)' -o dist/cluster-linux-amd64 ./cmd/cluster
	GOOS=linux GOARCH=arm64 go build -trimpath -ldflags '$(LDFLAGS)' -o dist/cluster-linux-arm64 ./cmd/cluster

test:
	go test ./...

vet:
	go vet ./...
	GOOS=linux go vet ./...

# Regenerate gRPC code after editing internal/clusterpb/cluster.proto.
# Needs protoc plus:
#   go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
#   go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
proto:
	protoc --go_out=. --go_opt=paths=source_relative \
	       --go-grpc_out=. --go-grpc_opt=paths=source_relative \
	       internal/clusterpb/cluster.proto

clean:
	rm -rf bin dist
