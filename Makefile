# ai-flow: local development and the kind cluster.
#
#   make dev-local   run the control plane on the host (nodes = child processes)
#   make dev-up      kind cluster + images + Helm release, UI on http://localhost:8080
#   make dev-reload  rebuild images and restart after code changes
#   make dev-down    delete the kind cluster (state in STATE_DIR is kept)
#   make dev-reset   delete the cluster and the state: tasks, flows, runs, transcripts
#   make release     push the images to REGISTRY tagged VERSION (clean tree only)
#
# Secrets come from .env (gitignored): GITHUB_TOKEN, LINEAR_API_KEY, ...

CLUSTER   ?= ai-flow
NAMESPACE ?= ai-flow
CONFIG    ?= deploy/config
VERSION   ?= $(shell git describe --always --dirty 2>/dev/null || echo dev)
LDFLAGS   := -s -w -X main.version=$(VERSION)
KCTX      ?= kind-$(CLUSTER)
KUBECTL   := kubectl --context $(KCTX)
HELM      := helm --kube-context $(KCTX)
REGISTRY  ?= docker.mau.guru/library
# Host folder holding the kind cluster's state; survives dev-down / dev-up.
STATE_DIR ?= $(HOME)/.local/share/ai-flow/$(CLUSTER)

-include .env
export

.PHONY: build build-linux build-eval ui test test-race test-eval test-templates eval-demo lint helm-check images kind-up dev-up dev-reload dev-down dev-reset dev-local secrets deploy logs release release-check

build: ui
	go build -ldflags '$(LDFLAGS)' -o bin/ai-flow ./cmd/ai-flow

build-linux: ui
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags '$(LDFLAGS)' -o bin/linux/ai-flow ./cmd/ai-flow

build-eval:
	go build -o bin/ai-flow-eval ./cmd/ai-flow-eval

ui:
	@if [ -f web/package.json ]; then cd web && npm ci --no-audit --no-fund --loglevel=error && npm run build; fi

test:
	go test ./...

test-race:
	go test -race ./...

test-eval:
	go test ./cmd/ai-flow-eval ./internal/eval

test-templates:
	go test ./internal/eval -run '^TestTemplatesValidate$$' -count=1

# These are hand-authored fixtures, not live model evaluations.
eval-demo:
	go run ./cmd/ai-flow-eval -suite examples/evals/baseline-suite.json

lint: helm-check
	go vet ./...

# Offline only: render both storage modes and both Garage secret branches.
# Existing names are dummy references; no secret values or cluster are needed.
helm-check:
	helm lint deploy/chart --strict
	helm lint deploy/chart -f deploy/chart/values-kind.yaml --strict
	helm template ai-flow deploy/chart > /dev/null
	helm template ai-flow deploy/chart -f deploy/chart/values-kind.yaml > /dev/null
	helm template ai-flow deploy/chart --set garage.existingSecret=ci-garage-secret --set secretName=ci-control-plane-secrets > /dev/null
	helm template ai-flow deploy/chart -f deploy/chart/values-kind.yaml --set garage.existingSecret=ci-garage-secret --set secretName=ci-control-plane-secrets > /dev/null

images: build-linux
	docker build -f deploy/images/control-plane.Dockerfile -t ai-flow:dev .
	docker build -f deploy/images/agent.Dockerfile -t ai-flow-agent:dev .
	docker build -f deploy/images/agent-go.Dockerfile -t ai-flow-agent-go:dev .

# Tags the dev images as $(REGISTRY)/<name>:$(VERSION) and pushes them. Refuses a
# dirty tree so a pushed tag always names a commit.
release: release-check images
	@for img in ai-flow ai-flow-agent ai-flow-agent-go; do \
		docker tag $$img:dev $(REGISTRY)/$$img:$(VERSION) && docker push $(REGISTRY)/$$img:$(VERSION) || exit 1; \
	done
	@echo "pushed $(REGISTRY)/{ai-flow,ai-flow-agent,ai-flow-agent-go}:$(VERSION)"

release-check:
	@case "$(VERSION)" in *-dirty|dev) echo "release: commit first (VERSION=$(VERSION))"; exit 1;; esac

# kind switches the current kube-context; switch back so nothing else moves.
kind-up:
	@mkdir -p $(STATE_DIR)/data $(STATE_DIR)/garage bin
	@sed 's#__STATE_DIR__#$(STATE_DIR)#' deploy/kind/kind.yaml > bin/kind.yaml
	@kind get clusters | grep -qx $(CLUSTER) || { \
		prev=$$(kubectl config current-context 2>/dev/null); \
		kind create cluster --name $(CLUSTER) --config bin/kind.yaml; \
		[ -n "$$prev" ] && kubectl config use-context "$$prev" >/dev/null; true; }
	@echo "state: $(STATE_DIR)"

secrets:
	$(KUBECTL) create namespace $(NAMESPACE) --dry-run=client -o yaml | $(KUBECTL) apply -f -
	$(KUBECTL) -n $(NAMESPACE) create secret generic ai-flow-secrets \
		--from-literal=GITHUB_TOKEN="$${GITHUB_TOKEN:-$$(gh auth token 2>/dev/null)}" \
		--from-literal=LINEAR_API_KEY="$${LINEAR_API_KEY}" \
		--from-literal=LITELLM_API_KEY="$${LITELLM_API_KEY}" \
		--dry-run=client -o yaml | $(KUBECTL) apply -f -

deploy:
	$(HELM) upgrade --install ai-flow deploy/chart -n $(NAMESPACE) --create-namespace \
		-f deploy/chart/values-kind.yaml \
		--set runAsUser=$$(id -u) --set runAsGroup=$$(id -g) \
		--set garage.runAsUser=$$(id -u) --set garage.runAsGroup=$$(id -g) \
		$(foreach f,$(wildcard $(CONFIG)/*.yaml),--set-file 'config.files.$(subst .,\.,$(notdir $(f)))=$(f)')
	$(KUBECTL) -n $(NAMESPACE) rollout status deploy/ai-flow --timeout=180s

dev-up: kind-up images
	kind load docker-image --name $(CLUSTER) ai-flow:dev ai-flow-agent:dev ai-flow-agent-go:dev
	$(MAKE) secrets deploy
	@echo "ai-flow is up: http://localhost:8080"

dev-reload: images
	kind load docker-image --name $(CLUSTER) ai-flow:dev ai-flow-agent:dev ai-flow-agent-go:dev
	$(MAKE) deploy
	$(KUBECTL) -n $(NAMESPACE) rollout restart deploy/ai-flow
	$(KUBECTL) -n $(NAMESPACE) rollout status deploy/ai-flow --timeout=180s

dev-down:
	kind delete cluster --name $(CLUSTER)
	@echo "state kept in $(STATE_DIR) (make dev-reset to delete it)"

dev-reset: dev-down
	rm -rf $(STATE_DIR)

logs:
	$(KUBECTL) -n $(NAMESPACE) logs deploy/ai-flow -f

dev-local: build
	./bin/ai-flow mcp-demo :8090 & trap "kill $$!" EXIT; \
	AI_FLOW_PI_EXT=$(CURDIR)/pi-ext/index.ts GITHUB_TOKEN="$${GITHUB_TOKEN:-$$(gh auth token 2>/dev/null)}" \
		./bin/ai-flow server -config $(CONFIG) -config deploy/local/environment.yaml
