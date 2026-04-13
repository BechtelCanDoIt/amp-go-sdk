.PHONY: build test lint clean docker-cli docker-chatbot

# Build all binaries
build:
	go build -o bin/amp-cli ./cmd/amp-cli/
	go build -o bin/chatbot ./examples/chatbot/

# Run tests with race detector (BRD §14)
test:
	go test -race -v ./...

# Static analysis
lint:
	go vet ./...

# Clean build artifacts
clean:
	rm -rf bin/

# Docker builds
docker-cli:
	docker build --target cli -t ghcr.io/wso2/amp-cli:latest .

docker-chatbot:
	docker build --target chatbot -t ghcr.io/wso2/amp-chatbot:latest .

docker-all: docker-cli docker-chatbot

# Run chatbot locally (requires env vars)
run-chatbot:
	go run ./examples/chatbot/

# Run CLI health check locally
run-health:
	go run ./cmd/amp-cli/ health

# Run CLI chat locally
run-chat:
	go run ./cmd/amp-cli/ chat --agent-url http://localhost:8080
