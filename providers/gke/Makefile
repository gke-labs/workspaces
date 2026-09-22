.PHONY: test build render plan

test:
	go test -race ./...
	go vet ./...

build:
	CGO_ENABLED=0 go build -trimpath -o bin/access-proxy ./cmd/access-proxy
	CGO_ENABLED=0 go build -trimpath -o bin/snapshot-addon ./cmd/snapshot-addon
	CGO_ENABLED=0 go build -trimpath -o bin/render ./cmd/render

render:
	kubectl kustomize manifests/proxy

plan:
	bash scripts/plan.sh "$(CONFIG)" "$(OUTPUT)"
