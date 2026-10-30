default:
    @just --list

# Run the API and dashboard, and the mail protocols (POP3, IMAP)
serve:
    go run ./cmd/boxy serve --migrate --mail

# Run only the mail protocols (POP3, IMAP)
mail:
    go run ./cmd/boxy mail

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

# List direct dependencies with newer versions
deps:
    @go list -m -u -f '{{"{{"}}if and .Update (not .Indirect){{"}}"}}{{"{{"}}.Path{{"}}"}}: {{"{{"}}.Version{{"}}"}} -> {{"{{"}}.Update.Version{{"}}"}}{{"{{"}}end{{"}}"}}' all

# Lint proto files
lint:
    buf lint

# Check for known vulnerabilities in code paths we call
vuln:
    go run golang.org/x/vuln/cmd/govulncheck@latest ./...

# Clean build artifacts
clean:
    rm -f boxy
    rm -f *.db *.db-journal *.db-wal *.db-shm
