# substrate-eks native installer. The targets follow substrate-gke's Makefile.
#
#   make build          build bin/substrate-eks
#   make run            the wizard (AWS_PROFILE, AWS_REGION, CLUSTER_NAME preselect)
#   make run-demo       the wizard, ending with the counter demo on gVisor and microVM
#   make dry-run        walk every screen without touching AWS
#   make doctor         preflight checks only
#   make plan           setup-aws bootstrap --plan: what Provision would create, read-only
#   make verify         check the counter demo: suspend and resume on every sandbox class
#   make teardown       remove what the installer created (KEEP_CLUSTER=true keeps the cluster)
#   make test           vet, unit tests and gofmt of both Go modules
#   make parity         deploy/eks renders what patches/substrate-v1.0.0-eks.patch does
#   make upstream-check SUBSTRATE_GKE=../../substrate-gke
#                       verbatim files still match substrate-gke; list upstream
#                       commits to carry over (see upstream.lock)

GO ?= go
# The pinned local toolchain, and modules fetched straight from their source.
export GOTOOLCHAIN ?= local
export GOPROXY ?= direct
export GOSUMDB ?= off
export GOFLAGS ?= -mod=mod

BIN := bin/substrate-eks
MODULES := tools/setup-aws installer

.PHONY: build run run-demo dry-run doctor plan verify teardown test parity upstream-check

build:
	$(GO) -C installer build -o ../$(BIN) .

run: build
	./$(BIN)

run-demo: build
	./$(BIN) --demo

dry-run: build
	./$(BIN) --dry-run

doctor: build
	./$(BIN) --doctor

plan:
	$(GO) -C tools/setup-aws run . bootstrap --plan

verify: build
	./$(BIN) --verify

# The values are your wizard answers; its Complete screen prints this command.
teardown: build
	@test -n "$(CLUSTER_NAME)" && test -n "$(AWS_REGION)" || { echo "set CLUSTER_NAME and AWS_REGION (and AWS_PROFILE)"; exit 2; }
	./$(BIN) --teardown --cluster "$(CLUSTER_NAME)" --region "$(AWS_REGION)" --profile "$(AWS_PROFILE)" \
		$(if $(filter true,$(KEEP_CLUSTER)),--keep-cluster)

test:
	for m in $(MODULES); do $(GO) -C $$m vet ./... && $(GO) -C $$m test ./... || exit 1; done
	@test -z "$$(gofmt -l $(MODULES))" || { gofmt -l $(MODULES); echo "gofmt: files above need formatting"; exit 1; }

# Clones agent-substrate/substrate at v1.0.0; PARITY_REPO=<local clone> saves the fetch.
parity:
	SUBSTRATE_PARITY=1 $(GO) -C installer test -count=1 -run '^TestRenderParity$$' -v ./internal/eksoverlay/

upstream-check:
	@test -n "$(SUBSTRATE_GKE)" || { echo "set SUBSTRATE_GKE to a substrate-gke checkout"; exit 2; }
	git -C "$(SUBSTRATE_GKE)" fetch --quiet origin main
	SUBSTRATE_GKE="$(abspath $(SUBSTRATE_GKE))" $(GO) -C installer test -count=1 -run 'Upstream' -v .
