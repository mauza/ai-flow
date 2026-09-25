# ai-flow: local development and the kind cluster.
#
#   make dev-local   run the control plane on the host (nodes = child processes)
#   make dev-up      kind cluster + images + Helm release, UI on http://localhost:8080
#   make dev-reload  rebuild images and restart after code changes
#   make dev-down    delete the kind cluster (state in STATE_DIR is kept)
#   make dev-reset   delete the cluster and the state: tasks, flows, runs, transcripts
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
# Host folder holding the kind cluster's state; survives dev-down / dev-up.
STATE_DIR ?= $(HOME)/.local/share/ai-flow/$(CLUSTER)

-include .env
export

.PHONY: build build-linux ui test lint images kind-up dev-up dev-reload dev-down dev-reset dev-local secrets deploy logs

build: ui
	go build -ldflags '$(LDFLAGS)' -o bin/ai-flow ./cmd/ai-flow

build-linux: ui
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags '$(LDFLAGS)' -o bin/linux/ai-flow ./cmd/ai-flow

ui:
	@if [ -f web/package.json ]; then cd web && npm ci --no-audit --no-fund --loglevel=error && npm run build; fi

test:
	go test ./...

lint:
	go vet ./...
	helm lint deploy/chart -f deploy/chart/values-kind.yaml

images: build-linux
	docker build -f deploy/images/control-plane.Dockerfile -t ai-flow:dev .
	docker build -f deploy/images/agent.Dockerfile -t ai-flow-agent:dev .
	docker build -f deploy/images/agent-go.Dockerfile -t ai-flow-agent-go:dev .

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
