BINARY := firerunner
PKG := ./...
# Coverage is gated on every package that can be meaningfully unit-tested: the
# functional core (pure logic + seam-injected I/O), the diag health checks, and
# the cache server's HTTP handlers. Only cmd/ (flag wiring + signal handling)
# and internal/listener (the actions/scaleset GitHub control-plane client) sit
# outside the gate — they are the imperative shell, needing a live GitHub + KVM
# host, and are exercised by `make test` and the e2e suite instead. The
# multi-tier orchestration logic they invoke (tier catalog parsing, warm-pool
# vs. slot-budget validation) lives in internal/config, which IS gated.
COVERPKG := ./internal/config/...,./internal/core/...,./internal/provisioner/...,./internal/scheduler/...,./internal/diag/...,./internal/cacheserver/...
COVERPKGDIRS := ./internal/config/... ./internal/core/... ./internal/provisioner/... ./internal/scheduler/... ./internal/diag/... ./internal/cacheserver/...
COVERPROFILE := cover.out
COVER_MIN ?= 70

.PHONY: build
build:
	go build -o $(BINARY) ./cmd/firerunner

.PHONY: vet
vet:
	go vet $(PKG)

.PHONY: test
test:
	go test -race $(PKG)

# Unit coverage across all packages (cross-package attribution via -coverpkg).
.PHONY: cover
cover:
	go test -race -covermode=atomic -coverpkg=$(COVERPKG) -coverprofile=$(COVERPROFILE) $(COVERPKGDIRS)
	go tool cover -func=$(COVERPROFILE) | tail -n 1

.PHONY: cover-html
cover-html: cover
	go tool cover -html=$(COVERPROFILE)

# Fail if total coverage is below COVER_MIN percent.
.PHONY: cover-check
cover-check: cover
	@total=$$(go tool cover -func=$(COVERPROFILE) | awk '/^total:/ {gsub(/%/,"",$$3); print $$3}'); \
	echo "total coverage: $$total% (min $(COVER_MIN)%)"; \
	awk -v t="$$total" -v m="$(COVER_MIN)" 'BEGIN { exit (t+0 < m+0) }' || \
		{ echo "coverage $$total% is below minimum $(COVER_MIN)%"; exit 1; }

# End-to-end tests require a real KVM host and are gated behind the `e2e` tag.
.PHONY: e2e
e2e:
	go test -tags e2e -count=1 ./test/e2e/...

.PHONY: clean
clean:
	rm -f $(BINARY) $(COVERPROFILE)
