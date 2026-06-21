VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
BINARY  := naozhi
MAIN    := ./cmd/naozhi/

.PHONY: build vet test lint vuln deploy release clean lint-server lint-server-fail lint-fact-table lint-fact-table-fail lint-router lint-router-fail release-gate release-gate-live

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

# Router 字段 `// 读写:` 注释漂移检测（router-split P0 安全网，RFC
# router-god-object-split）。AST 解析 Router 结构每个字段的声明访问域，再扫
# 所有 router_*.go 实际 r.<field> 访问对账，漂移即报。
# CI 跑 lint-router-fail（P0 安全网已上膛，漂移即 exit 1 卡 PR）；
# lint-router（warn 模式）保留供本地快速扫描，不卡本地构建。
lint-router:
	go run ./tools/check-router-fields -mode warn

lint-router-fail:
	go run ./tools/check-router-fields -mode fail

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

# Post-deploy screenshot gate. Boots a throwaway dashboard-only instance from a
# freshly built binary, drives every top-level view with Playwright, and asserts
# render + zero-console-error per view. Non-zero exit blocks release. This is the
# CI-equivalent gate runnable locally without a systemd deployment.
# Playwright deps: (cd test/e2e && npm install)
release-gate: build
	NAOZHI_GATE_BIN=bin/$(BINARY) ./scripts/gate-instance.sh

# Gate an already-running instance (e.g. after `make deploy`). Reads the live
# dashboard token from the systemd EnvironmentFile. Targets 127.0.0.1:8180.
release-gate-live:
	@TOKEN=$$(sudo grep -hE '^NAOZHI_DASHBOARD_TOKEN=' /home/ec2-user/.naozhi/env 2>/dev/null | head -1 | cut -d= -f2-); \
	if [ -z "$$TOKEN" ]; then echo "✗ NAOZHI_DASHBOARD_TOKEN not found in /home/ec2-user/.naozhi/env"; exit 1; fi; \
	NAOZHI_BASE_URL=http://127.0.0.1:8180 NAOZHI_DASHBOARD_TOKEN="$$TOKEN" node scripts/release-gate.mjs

clean:
	rm -rf dist/
