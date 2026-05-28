VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
BINARY  := naozhi
MAIN    := ./cmd/naozhi/

.PHONY: build vet test lint vuln deploy release clean lint-server lint-server-fail lint-fact-table lint-fact-table-fail

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

# server-package contracts (server-split-phase4-design.md §六.2 / §九.2).
# warn mode: prints violations to stderr but exits 0; CI default during
# Phase 0-4. Switch to lint-server-fail after Phase 5 completes.
lint-server:
	go run ./tools/lint-server-handlers -mode warn

lint-server-fail:
	go run ./tools/lint-server-handlers -mode fail

# fact-table drift detection (server-split-phase4-design.md v0.6.1 §0 纪律 5).
# 扫 design / RFC markdown 中的关键数字 token 与 speech 表对账，漂移即报。
# warn mode: 不卡 PR；Phase 5 完工后切 lint-fact-table-fail。
lint-fact-table:
	go run ./tools/lint-fact-table -mode warn \
		docs/design/server-split-phase4-design.md \
		docs/design/server-split-phase4-baseline.md

lint-fact-table-fail:
	go run ./tools/lint-fact-table -mode fail \
		docs/design/server-split-phase4-design.md \
		docs/design/server-split-phase4-baseline.md

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
