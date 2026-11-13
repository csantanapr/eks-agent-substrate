# substrate-eks native installer (POC). Same targets as substrate-gke's Makefile.
#
#   make run            the wizard (AWS_PROFILE, AWS_REGION, CLUSTER_NAME preselect)
#   make dry-run        walk every screen without touching AWS
#   make doctor         preflight checks only
#   make plan           setup-aws bootstrap --plan: what Provision would create, read-only
#   make test           unit tests of both Go modules
#   make upstream-check SUBSTRATE_GKE=../../substrate-gke
#                       verbatim files still match substrate-gke; list upstream
#                       commits to carry over (see upstream.lock)

GO ?= go
# Pinned toolchain, and module downloads straight from their source.
export GOTOOLCHAIN ?= local
export GOFLAGS ?= -mod=mod

BIN := bin/substrate-eks

.PHONY: build run dry-run doctor plan test upstream-check

build:
	$(GO) -C installer build -o ../$(BIN) .

run: build
	./$(BIN)

dry-run: build
	./$(BIN) --dry-run

doctor: build
	./$(BIN) --doctor

plan:
	$(GO) -C tools/setup-aws run . bootstrap --plan

test:
	$(GO) -C tools/setup-aws vet ./...
	$(GO) -C tools/setup-aws test ./...
	$(GO) -C installer vet ./...
	$(GO) -C installer test ./...
	@test -z "$$(gofmt -l installer tools)" || { gofmt -l installer tools; echo "gofmt: files above need formatting"; exit 1; }

upstream-check:
	@test -n "$(SUBSTRATE_GKE)" || { echo "set SUBSTRATE_GKE to a substrate-gke checkout"; exit 2; }
	git -C "$(SUBSTRATE_GKE)" fetch --quiet origin main
	SUBSTRATE_GKE="$(abspath $(SUBSTRATE_GKE))" $(GO) -C installer test -count=1 -run 'Upstream' -v .
