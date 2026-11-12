GO ?= go

.PHONY: all build dev test vet check integration it-registrar

all: check test

build:
	$(GO) build ./...

# A dev build of kwak in bin/kwak (decision 40): it uses its own kwak-dev folders for config,
# state and socket, and never installs the user service. go install and the Nix package never
# set this flag, so their binaries are release builds. See docs/development.md.
DEV_LDFLAGS = -X github.com/fcanela/kwak/internal/paths.flavor=dev

dev:
	$(GO) build -ldflags "$(DEV_LDFLAGS)" -o bin/kwak ./cmd/kwak

test:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...
	$(GO) vet -tags integration ./...

check: vet
	sh test/cross.sh

# ./test/... is the harness, the fact probes and, once it exists, test/e2e; ./deploy/ builds the
# registrar image and checks the nginx locations (local images only). On a host whose
# firewall drops traffic between containers, run KWAK_IT_SHARED_NETNS=1 make integration.
integration:
	CGO_ENABLED=0 $(GO) test -tags integration ./test/... ./deploy/

it-registrar:
	CGO_ENABLED=0 $(GO) test -tags integration ./internal/registrar/...
