.PHONY: build-web build run

build-web:
	cd internal/dashboard/web && npm install && npm run build

build: build-web
	go build ./cmd/ai-flow/

run:
	./ai-flow -config config.yaml -db .local/ai-flow.db
