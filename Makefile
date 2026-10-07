VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/linuxuser586/gcpemu/internal/instance.Version=$(VERSION)

.PHONY: build test race vet lint release
build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/gcpemu ./cmd/gcpemu

test:
	go test ./...

race:
	go test -race ./...

vet:
	gofmt -l . | (! grep .) && go vet ./...

# NFR-PORT-001: static builds for every supported OS/arch.
release:
	@for p in linux/amd64 linux/arm64 darwin/arm64 darwin/amd64; do \
		os=$${p%/*}; arch=$${p#*/}; \
		echo "building $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags '$(LDFLAGS)' -o dist/gcpemu-$$os-$$arch ./cmd/gcpemu || exit 1; \
	done
