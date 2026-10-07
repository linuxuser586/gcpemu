VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/linuxuser586/gcpemu/internal/instance.Version=$(VERSION)

.PHONY: build test race vet lint release e2e
build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/gcpemu ./cmd/gcpemu

test:
	go test ./...

race:
	go test -race ./...

vet:
	gofmt -l . | (! grep .) && go vet ./... && go vet -tags e2e ./e2e/...

# SRS 11.2 reference stack (container runtime + tofu; steps 2 and 4-8
# download Helm, Istio charts and images: set GCPEMU_NET_TESTS=0 to skip them).
e2e:
	GCPEMU_NET_TESTS=$${GCPEMU_NET_TESTS:-1} go test -tags e2e ./e2e -run TestReferenceStack -count=1 -timeout 30m -v

# NFR-PORT-001: static builds for every supported OS/arch.
release:
	@for p in linux/amd64 linux/arm64 darwin/arm64 darwin/amd64; do \
		os=$${p%/*}; arch=$${p#*/}; \
		echo "building $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags '$(LDFLAGS)' -o dist/gcpemu-$$os-$$arch ./cmd/gcpemu || exit 1; \
	done
