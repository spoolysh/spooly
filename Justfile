default:
    @just --list

# Run the API and dashboard, and the mail protocols (POP3, IMAP)
serve:
    go run ./cmd/spooly serve --migrate --mail

# Run only the mail protocols (POP3, IMAP)
mail:
    go run ./cmd/spooly mail

# Build the binary
build:
    go build -o spooly ./cmd/spooly

# Generate proto code
generate:
    buf generate

# Run database migrations
migrate:
    go run ./cmd/spooly migrate

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
    rm -f spooly
    rm -f *.db *.db-journal *.db-wal *.db-shm
