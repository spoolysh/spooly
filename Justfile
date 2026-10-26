default:
    @just --list

# Run the API and dashboard, and the mail protocols (POP3)
serve:
    go run ./cmd/boxy serve --migrate --mail

# Run only the mail protocols (POP3)
mail:
    go run ./cmd/boxy mail --migrate

# Build the binary
build:
    go build -o boxy ./cmd/boxy

# Generate proto code
generate:
    buf generate

# Run database migrations
migrate:
    go run ./cmd/boxy migrate

# Run tests
test:
    go test ./...

# Tidy dependencies
tidy:
    go mod tidy

# Lint proto files
lint:
    buf lint

# Clean build artifacts
clean:
    rm -f boxy
    rm -f *.db *.db-journal *.db-wal *.db-shm
