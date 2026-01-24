.PHONY: build-cli
build-cli: ## Build kube-etl binary.
	go build -o bin/kube-etl main.go

.PHONY: test
test: ## Run tests.
	go test ./...
