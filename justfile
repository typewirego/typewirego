# typewirego development commands

staticcheck := "honnef.co/go/tools/cmd/staticcheck@v0.8.2"
govulncheck := "golang.org/x/vuln/cmd/govulncheck@v1.9.0"

# Run the tests under the race detector
test:
    go test -race -count=1 ./...

# gofmt check, go vet and staticcheck
lint:
    test -z "$(gofmt -l .)" || { gofmt -l .; exit 1; }
    go vet ./...
    go run {{staticcheck}} ./...

# Format every Go file
fmt:
    gofmt -w .

# Check for known vulnerabilities
vuln:
    go run {{govulncheck}} ./...

# Everything CI runs
check: lint test vuln
