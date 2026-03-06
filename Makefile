.PHONY: build-web build

build-web:
	cd internal/dashboard/web && npm install && npm run build

build: build-web
	go build ./cmd/ai-flow/
