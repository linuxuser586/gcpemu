# ADR 0061: every recipe line is one plain command, with no &&, ||, pipes,
# redirects or shell variables, so it runs the same in sh and in Windows
# PowerShell 5.1. Control flow lives in internal/devtool (go run). Recipes
# call only go (and gofmt), git, pnpm, docker, npm and just itself, plus
# echo, which both shells have, and sudo on macOS.

set windows-shell := ["powershell.exe", "-NoProfile", "-Command"]

ldflags := "-s -w -X github.com/linuxuser586/gcpemu/internal/instance.Version="
exe := if os() == "windows" { ".exe" } else { "" }

# #541: a bare `just` lists the recipes rather than building.
[private]
default:
    @just --list --unsorted

# #541: the tools for building and testing gcpemu; `gcpemu doctor` checks
# a host for running it. Required tools fail, optional ones warn and name
# the recipes they block. Nothing is installed.
[doc("Check the tools for building and testing gcpemu (bin/gcpemu doctor checks running it)")]
doctor:
    @go run ./internal/devtool doctor

# Builds the console first when pnpm is on PATH; without it the binary
# embeds the "console not built" placeholder. just's which() needs the
# unstable `set lists`, so devtool looks pnpm up.
[doc("Build bin/gcpemu, with the Web console when pnpm is on PATH")]
[env("CGO_ENABLED", "0")]
build version=`go run ./internal/devtool version`:
    @{{ if `go run ./internal/devtool which pnpm` == "" { "echo 'warning: pnpm not found; embedding the Web console placeholder (install Node.js 26 and pnpm for the real console)'" } else { "just console" } }}
    go build -trimpath -ldflags '{{ ldflags }}{{ version }}' -o bin/gcpemu{{ exe }} ./cmd/gcpemu

# ADR 0005: the linux binary that Managed containers run, next to
# bin/gcpemu, for the container runtime's architecture (the host's when
# docker doesn't answer). Needed when the host isn't linux on that
# architecture and the build isn't a release (vX.Y.Z).
[doc("Build bin/gcpemu-linux-<arch> for the container runtime's architecture")]
agent version=`go run ./internal/devtool version` arch=`go run ./internal/devtool docker-arch`: (cross-one "linux" arch version "bin")

[doc("Build the Web console bundle into console/dist/bundle (needs pnpm)")]
[working-directory("console")]
console:
    pnpm install --frozen-lockfile
    pnpm build

# FR-UI-023: Playwright on Chromium, Firefox and WebKit against a binary
# with the real console. gcpemu-testdir holds the owner locks of the
# tests' Test data dirs (ADR 0076); it is never released.
[doc("Run the Web console's Playwright tests against a fresh build")]
[working-directory("console")]
console-e2e: build
    go build -trimpath -o ../bin/gcpemu-testdir{{ exe }} ../cmd/gcpemu-testdir
    pnpm exec playwright test

# #396: tests that run GKE clusters share GCPEMU_TEST_CLUSTERS slots (2)
# across packages, so services/gke outlasts go test's default 10m.
[doc("Run the Go tests")]
test:
    go test -timeout 45m ./...

# NFR-MNT-001: regenerate the API route tables and field-behaviour
# validation (internal/apidef) from the pinned googleapis commit and the
# discovery documents of the google.golang.org/api version in go.mod.
# `just generate latest` (or a commit SHA) moves the googleapis pin first.
[doc("Regenerate internal/apidef (`just generate latest` or a SHA moves the pin)")]
[working-directory("internal/apidef")]
generate googleapis="":
    go mod download google.golang.org/api
    go run ./internal/apigen -config apis.yaml -out . {{ if googleapis == "" { "" } else { "-googleapis " + googleapis } }}

# The race detector needs cgo, which Go turns off without a C compiler.
[doc("Run the Go tests with the race detector (needs a C compiler)")]
race:
    @go run ./internal/devtool need-cgo
    go test -race -timeout 45m ./...

# devtool vet checks gofmt and that only Windows builds link go-winio
# (ADR 0015, #419).
[doc("Check gofmt, run go vet (with the e2e, compat and conformance build tags too), keep go-winio Windows-only and check ADR numbers")]
vet:
    go run ./internal/devtool vet
    go vet ./...
    go vet -tags e2e ./e2e/...
    go vet -tags compat ./compat/...
    go vet -tags conformance ./conformance/...
    @go run ./internal/devtool adr

# #482: ADRs drafted on parallel branches can take the same number.
[doc("Check that no two ADRs share a number")]
adr:
    @go run ./internal/devtool adr

# The whole e2e package, with no -run filter (#494), so no test is left out:
# the SRS 11.2 Reference stack (container runtime + tofu; steps 2 and 4-8
# download Helm, Istio charts and images: set GCPEMU_NET_TESTS=0 to skip them)
# and the Containerized Instance tests, which build the OCI image (they need
# the net and Docker too). Each test's own checks decide whether it runs.
# TestAutopilot alone is skipped: just autopilot runs it, in its own CI job.
# TestGKESizing skips itself unless just gke-sizing runs it.
[doc("Run the SRS 11.2 Reference stack and the Containerized Instance tests")]
[env("GCPEMU_NET_TESTS", if env("GCPEMU_NET_TESTS", "") == "" { "1" } else { env("GCPEMU_NET_TESTS") })]
e2e:
    go test -tags e2e ./e2e -skip '^TestAutopilot$' -count=1 -timeout 30m -v

# SRS 11.4: an Autopilot cluster from OpenTofu at each supported minor, with
# Kubernetes conformance (Sonobuoy quick mode), scaling past one node, a
# privileged pod refused and Istio sidecar injection. Needs the net.
[doc("Run the SRS 11.4 Autopilot acceptance test")]
[env("GCPEMU_NET_TESTS", "1")]
autopilot:
    go test -tags e2e ./e2e -run '^TestAutopilot$' -count=1 -timeout 45m -v

# #1069: measure an Autopilot cluster's Cluster server and nodes, idle and
# under load, at 1 node and at GCPEMU_GKE_SIZING_NODES nodes (at least 4),
# for the README's GKE sizing table. Prints the table; rerun it when the
# k3s image changes. GCPEMU_GKE_SIZING_CLI=podman samples with podman stats.
[doc("Measure GKE Cluster server and node memory/CPU for the README sizing table")]
[env("GCPEMU_GKE_SIZING", "1")]
gke-sizing $GCPEMU_GKE_SIZING_NODES="5" version=`go run ./internal/devtool version`:
    go test -ldflags "-X github.com/linuxuser586/gcpemu/internal/instance.Version={{ version }}" -tags e2e ./e2e -run '^TestGKESizing$' -count=1 -timeout 60m -v

# #606: the Pre-release host checks (docs/release.md) on each runtime in
# runtimes, docker contexts (default: the Required runtimes). Every OS runs
# bin/gcpemu on the host, so build it first. On macOS loopback and dns
# install need sudo, whose timestamp the tests keep alive; on Linux they
# run only for the runtimes named. macOS and Windows build the release's
# image unless image names one; on Windows dns install shows UAC dialogs.
# Each runtime's VLAN result goes into README.md. The parameters are
# exported under the names the tests read.
[doc("Run the Pre-release host checks on macOS or Windows (just prerelease <context,...> <image>)")]
prerelease $GCPEMU_PRERELEASE_RUNTIMES="" $GCPEMU_PRERELEASE_IMAGE="": build
    {{ if os() == "macos" { "sudo -v" } else { "" } }}
    go test -tags prerelease ./prerelease -count=1 -timeout 2h -v

# #498: the Image readiness check. Runs the image, already built, the way
# the Dockerfile's header documents, and fails unless Docker reports the
# container healthy within 120 s.
[doc("Run the Image readiness check on a built image (just image-check <ref>; never builds or pulls it)")]
image-check image:
    @go run ./internal/devtool image-check {{ image }}

# #862: the Conformance suite's test backend. Its binaries go to
# dist-echo/, not dist/, so they stay out of the GitHub release; its image
# copies them like the main image does:
#
#   docker buildx build --platform linux/amd64,linux/arm64 -f cmd/gcpemu-echo/Dockerfile dist-echo
[doc("Build the gcpemu-echo linux binaries into dist-echo/")]
echo-dist: (echo-one "amd64") (echo-one "arm64")

[private]
[env("CGO_ENABLED", "0")]
[env("GOOS", "linux")]
echo-one $GOARCH:
    go build -trimpath -ldflags '-s -w' -o dist-echo/gcpemu-echo-linux-{{ GOARCH }} ./cmd/gcpemu-echo

[doc("Check a built gcpemu-echo image answers on PORT (just echo-image-check <ref>; never builds or pulls it)")]
echo-image-check image:
    @go run ./internal/devtool echo-image-check {{ image }}

# SRS 7.1 client compatibility: gcloud, kubectl, Helm, Docker, crane, ko,
# psql, the Cloud SQL Auth Proxy and dig against a detached instance.
[doc("Run the SRS 7.1 client compatibility suite")]
compat:
    go test -tags compat ./compat -skip '^(TestOpenTofu|TestKubeConformance)$' -count=1 -timeout 30m -v

# SRS 11.1 / IF-001 OpenTofu acceptance: every module in compat/tofu with
# google and google-beta at the current and previous minor
# (GCPEMU_TOFU_PROVIDERS narrows it, e.g. google-beta or google@8.6.0).
[doc("Run the SRS 11.1 OpenTofu acceptance tests")]
tofu:
    go test -tags compat ./compat -run TestOpenTofu -count=1 -timeout 35m -v

# SRS 11.1 Kubernetes conformance: Sonobuoy quick mode on a Standard
# cluster from gcloud at each supported minor's newest patch. A failed
# run leaves its tarball in compat/kube-conformance-results.
[doc("Run Kubernetes conformance (Sonobuoy quick mode) at each supported GKE minor")]
kube-conformance:
    go test -tags compat ./compat -run TestKubeConformance -count=1 -timeout 75m -v

# SRS 11.1 / ADR 0023 Conformance suite: compare an in-process Instance
# with the Recordings in conformance/testdata, behind the Escape guard.
# A test without a Recording skips.
[doc("Compare the emulator with the Conformance suite's Recordings")]
conformance:
    go test -tags conformance ./conformance -count=1 -timeout 15m -v

# Records GCP's responses into conformance/testdata, against the project
# GCPEMU_CONFORMANCE_PROJECT names with Application Default Credentials,
# after sweeping what earlier runs left there. The second run takes the
# Products' tests by name prefix; the harness's own tests need no GCP.
[doc("Record the Conformance suite against GCPEMU_CONFORMANCE_PROJECT (real GCP)")]
[env("GCPEMU_CONFORMANCE_RECORD", "1")]
conformance-record:
    @{{ if env("GCPEMU_CONFORMANCE_PROJECT", "") == "" { error("set GCPEMU_CONFORMANCE_PROJECT to the GCP project to record against") } else { "" } }}
    go test -tags conformance ./conformance -run '^TestSweep$' -count=1 -timeout 15m -v
    go test -tags conformance ./conformance -run '^Test(Storage|PubSub|SecretManager|DNS|ArtifactRegistry|IAM)' -count=1 -timeout 30m -v

# NFR-PORT-001: static builds for every supported OS/arch, with the real
# Web console embedded. ADR 0060: console/embed.go embeds the bundle only
# when it is there, so the check runs before cross does.
[doc("Build the release binaries into dist/, with the Web console (just release vX.Y.Z)")]
release version=`go run ./internal/devtool version`: console && (cross version)
    @{{ if path_exists("console/dist/bundle/index.html") == "true" { "" } else { error("release: the Web console bundle is not built (console/dist/bundle/index.html is missing)") } }}

# The release builds with whatever console/dist/bundle holds (Node-free CI).
[doc("Build static binaries for every supported OS/arch into dist/")]
cross version=`go run ./internal/devtool version`: (cross-one "linux" "amd64" version) (cross-one "linux" "arm64" version) (cross-one "darwin" "arm64" version) (cross-one "darwin" "amd64" version) (cross-one "windows" "amd64" version) (cross-one "windows" "arm64" version)

# #1055: cross's binary for this host only, at the same dist/ path. CI
# runs cross on x64 Linux alone; every other runner runs this, so its Go
# cache holds one target's compile, not six.
[doc("Build the static binary for this host's OS/arch into dist/")]
cross-host version=`go run ./internal/devtool version`: (cross-one `go env GOOS` `go env GOARCH` version)

# One static binary, dir/gcpemu-<os>-<arch>, for cross and agent. GOOS and
# GOARCH are exported parameters, since [env] can't read parameters and
# must not reach the backticks that `go run` devtool for the host.
[private]
[env("CGO_ENABLED", "0")]
cross-one $GOOS $GOARCH version dir="dist":
    @echo 'building {{ GOOS }}/{{ GOARCH }}'
    go build -trimpath -ldflags '{{ ldflags }}{{ version }}' -o {{ dir }}/gcpemu-{{ GOOS }}-{{ GOARCH }}{{ if GOOS == "windows" { ".exe" } else { "" } }} ./cmd/gcpemu

# FR-CI-001/002: the setup-gcpemu GitHub Action; dist/ is committed (the
# runner executes it as is), so rebuild and commit it with the sources.
[doc("Test and rebuild the setup-gcpemu action (commit its dist/)")]
[working-directory("setup-gcpemu")]
action:
    npm ci
    npm test
    npm run build
