CLUSTER    = temporal-local
CONTEXT    = kind-$(CLUSTER)
NAMESPACE  = temporal-workers
IMAGE      = temporal-worker:local
COMPOSE    = docker compose -f ../docker-compose.yml
NET        = temporal-net

# ── Full lifecycle ────────────────────────────────────────────────────────────

.PHONY: setup teardown

setup: temporal-up cluster build load deploy
	@echo ""
	@echo "  Local setup complete."
	@echo ""
	@echo "  Temporal UI  →  http://localhost:8080  (already exposed by docker-compose)"
	@echo "  Workers      →  kubectl get pods -n $(NAMESPACE) --context $(CONTEXT)"
	@echo ""
	@echo "  Submit a test workflow:  make run-workflow"
	@echo "  Tail worker logs:        make logs"

teardown:
	kind delete cluster --name $(CLUSTER) 2>/dev/null || true
	$(COMPOSE) down -v

# ── Temporal server ───────────────────────────────────────────────────────────

.PHONY: temporal-up temporal-down

temporal-up:
	$(COMPOSE) up -d
	@echo "Waiting for Temporal server to be ready..."
	@until docker exec temporal /bin/sh -c 'curl -sf http://localhost:7233/api/v1/namespaces 2>/dev/null || tctl --address localhost:7233 namespace list 2>/dev/null || true' 2>/dev/null; do \
	  sleep 3; \
	done; sleep 5
	@echo "Temporal is up."

temporal-down:
	$(COMPOSE) down -v

# ── Kind cluster ──────────────────────────────────────────────────────────────

.PHONY: cluster

cluster:
	@if kind get clusters 2>/dev/null | grep -q '^$(CLUSTER)$$'; then \
	  echo "Cluster $(CLUSTER) already exists, skipping creation."; \
	else \
	  kind create cluster --config kind-config.yaml; \
	fi
	@echo "Connecting Kind nodes to $(NET) so pods can reach temporal:7233..."
	@for node in $$(kind get nodes --name $(CLUSTER)); do \
	  docker network connect $(NET) $$node 2>/dev/null && echo "  connected $$node" || echo "  $$node already connected"; \
	done

# ── Docker image ──────────────────────────────────────────────────────────────

.PHONY: build load

build:
	docker build \
	  -f ../09_k8s/app/Dockerfile \
	  -t $(IMAGE) \
	  ..

load:
	kind load docker-image $(IMAGE) --name $(CLUSTER)

# ── K8s manifests ─────────────────────────────────────────────────────────────

.PHONY: deploy

deploy:
	kubectl apply -f k8s/ --context $(CONTEXT)
	kubectl rollout status deployment/temporal-orchestrator -n $(NAMESPACE) --context $(CONTEXT) --timeout=120s
	kubectl rollout status deployment/temporal-activities   -n $(NAMESPACE) --context $(CONTEXT) --timeout=120s

# Install metrics-server (needed for HPA) — Kind requires the insecure-tls flag.
.PHONY: metrics-server

metrics-server:
	kubectl apply -f https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml --context $(CONTEXT)
	kubectl patch deployment metrics-server -n kube-system --context $(CONTEXT) \
	  --type='json' \
	  -p='[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]'
	kubectl rollout status deployment/metrics-server -n kube-system --context $(CONTEXT) --timeout=60s

# ── Testing ───────────────────────────────────────────────────────────────────

.PHONY: run-workflow

# Runs the interactive starter from 08_template_runner against localhost:7233.
# Temporal is already exposed there via docker-compose port mapping.
run-workflow:
	cd .. && uv run python 08_template_runner/starter.py

# ── Observability ─────────────────────────────────────────────────────────────

.PHONY: logs logs-orchestrator logs-activities pods

pods:
	kubectl get pods -n $(NAMESPACE) --context $(CONTEXT) -o wide

logs-orchestrator:
	kubectl logs -n $(NAMESPACE) -l app=temporal-orchestrator -f --context $(CONTEXT) --max-log-requests 5

logs-activities:
	kubectl logs -n $(NAMESPACE) -l app=temporal-activities -f --context $(CONTEXT) --max-log-requests 5

logs: logs-orchestrator

# ── Redeploy after code changes ───────────────────────────────────────────────

.PHONY: redeploy

redeploy: build load
	kubectl rollout restart deployment/temporal-orchestrator -n $(NAMESPACE) --context $(CONTEXT)
	kubectl rollout restart deployment/temporal-activities   -n $(NAMESPACE) --context $(CONTEXT)
	kubectl rollout status  deployment/temporal-orchestrator -n $(NAMESPACE) --context $(CONTEXT) --timeout=120s
	kubectl rollout status  deployment/temporal-activities   -n $(NAMESPACE) --context $(CONTEXT) --timeout=120s
