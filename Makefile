.PHONY: build gate dev

build:
	cd web && npm install && npm run build
	touch web/dist/.keep
	go build -o rainforest ./cmd/rainforest

gate:
	test -z "$$(gofmt -l cmd internal web/embed.go)"
	go vet ./...
	go test ./...
	cd web && npm test && npm run lint && npm run typecheck

dev:
	cd web && npm run dev
