VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
BINARY  := naozhi
MAIN    := ./cmd/naozhi/

.PHONY: build vet test lint vuln deploy release clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags='$(LDFLAGS)' -o bin/$(BINARY) $(MAIN)

deploy: build
	sudo systemctl restart naozhi
	@sleep 1
	@sudo systemctl is-active --quiet naozhi && echo "✓ naozhi deployed ($(VERSION))" || (echo "✗ naozhi failed to start"; sudo journalctl -u naozhi --no-pager -n 10; exit 1)

vet:
	go vet ./...

# Static analysis. Install: go install honnef.co/go/tools/cmd/staticcheck@latest
lint:
	staticcheck ./...

# CVE scan against the Go vulnerability DB. Install:
# go install golang.org/x/vuln/cmd/govulncheck@latest
vuln:
	govulncheck ./...

test:
	go test -race ./...

# Cross-compile all supported platforms. Windows omitted: internal/shim
# depends on POSIX-only syscalls (Kill, Setsid) not present on windows/*.
release: clean
	@mkdir -p dist
	@for target in \
		linux/amd64 linux/arm64 \
		darwin/amd64 darwin/arm64; do \
		GOOS=$${target%/*} GOARCH=$${target#*/}; \
		OUT="dist/$(BINARY)-$$GOOS-$$GOARCH"; \
		echo "Building $$OUT"; \
		CGO_ENABLED=0 GOOS=$$GOOS GOARCH=$$GOARCH \
			go build -trimpath -ldflags='$(LDFLAGS)' -o "$$OUT" $(MAIN); \
	done
	@cd dist && sha256sum naozhi-* > checksums.txt
	@echo "Done. Artifacts in dist/"

clean:
	rm -rf dist/
