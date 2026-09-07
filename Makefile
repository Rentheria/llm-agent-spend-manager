.PHONY: build test serve clean vet help

# Default target
help:
	@echo "llm-agent-spend-manager - Makefile"
	@echo ""
	@echo "Targets:"
	@echo "  build    Build the CLI binary"
	@echo "  test     Run all tests"
	@echo "  vet      Run go vet"
	@echo "  serve    Build and run the dashboard server (localhost:4600)"
	@echo "  clean    Remove built binaries"
	@echo "  help     Show this help message"

# Build the main binary
build:
	go build -o llm-agent-spend-manager ./cmd/llm-agent-spend-manager

# Run tests (single process at a time for shared environments)
test:
	go test -p 1 ./...

# Run go vet
vet:
	go vet ./...

# Build and run the serve command
serve: build
	./llm-agent-spend-manager serve

# Clean built binaries
clean:
	rm -f llm-agent-spend-manager
