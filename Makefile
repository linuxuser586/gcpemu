# Evaluated once, before the console build rewrites console/dist/index.html.
ifeq ($(origin VERSION),undefined)
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
endif
LDFLAGS := -s -w -X github.com/linuxuser586/gcpemu/internal/instance.Version=$(VERSION)

# FR-UI-001: the Web console is embedded from console/dist. Building it
# overwrites the committed placeholder console/dist/index.html, which is
# put back once the binaries are built.
CONSOLE_PLACEHOLDER := console/dist/index.html
restore-placeholder = git checkout -- $(CONSOLE_PLACEHOLDER) 2>/dev/null || true

.PHONY: build test race vet lint release cross console console-e2e e2e compat tofu action generate
# Builds the console first when pnpm is on PATH; without it the binary
# embeds the "console not built" placeholder.
build:
	@if command -v pnpm >/dev/null 2>&1; then \
		$(MAKE) --no-print-directory console; \
	else \
		echo "warning: pnpm not found; embedding the Web console placeholder (install Node.js 24 and pnpm for the real console)" >&2; \
	fi
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/gcpemu ./cmd/gcpemu
	@$(restore-placeholder)

console:
	cd console && pnpm install --frozen-lockfile && pnpm build

# FR-UI-023: Playwright on Chromium, Firefox and WebKit against a binary
# with the real console.
console-e2e: build
	cd console && pnpm exec playwright test

test:
	go test ./...

# NFR-MNT-001: regenerate the API route tables and field-behaviour
# validation (internal/apidef) from the pinned googleapis commit and the
# discovery documents of the google.golang.org/api version in go.mod.
# GOOGLEAPIS=latest (or a commit SHA) moves the googleapis pin first.
generate:
	go mod download google.golang.org/api
	cd internal/apidef && go run ./internal/apigen -config apis.yaml -out . $(if $(GOOGLEAPIS),-googleapis $(GOOGLEAPIS))

race:
	go test -race ./...

vet:
	gofmt -l . | (! grep .) && go vet ./... && go vet -tags e2e ./e2e/... && go vet -tags compat ./compat/...

# SRS 11.2 reference stack (container runtime + tofu; steps 2 and 4-8
# download Helm, Istio charts and images: set GCPEMU_NET_TESTS=0 to skip them).
e2e:
	GCPEMU_NET_TESTS=$${GCPEMU_NET_TESTS:-1} go test -tags e2e ./e2e -run TestReferenceStack -count=1 -timeout 30m -v

# SRS 7.1 client compatibility: gcloud, kubectl, Helm, Docker, crane, ko,
# psql, the Cloud SQL Auth Proxy and dig against a detached instance.
compat:
	go test -tags compat ./compat -skip TestOpenTofu -count=1 -timeout 30m -v

# SRS 11.1 / IF-001 OpenTofu acceptance: every module in compat/tofu with
# google and google-beta at the current and previous minor
# (GCPEMU_TOFU_PROVIDERS narrows it, e.g. google-beta or google@8.6.0).
tofu:
	go test -tags compat ./compat -run TestOpenTofu -count=1 -timeout 60m -v

# NFR-PORT-001: static builds for every supported OS/arch, with the real
# Web console embedded.
release: console
	@! grep -q 'content="placeholder"' $(CONSOLE_PLACEHOLDER) || { echo "release: the Web console bundle is not built" >&2; exit 1; }
	@$(MAKE) --no-print-directory cross || { $(restore-placeholder); exit 1; }
	@$(restore-placeholder)

# The release builds with whatever console/dist holds (Node-free CI).
cross:
	@for p in linux/amd64 linux/arm64 darwin/arm64 darwin/amd64; do \
		os=$${p%/*}; arch=$${p#*/}; \
		echo "building $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags '$(LDFLAGS)' -o dist/gcpemu-$$os-$$arch ./cmd/gcpemu || exit 1; \
	done

# FR-CI-001/002: the setup-gcpemu GitHub Action; dist/ is committed (the
# runner executes it as is), so rebuild and commit it with the sources.
action:
	cd setup-gcpemu && npm ci && npm test && npm run build
