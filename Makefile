VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -ldflags "-X main.version=$(VERSION)"

# Where to install, following `go install`: GOBIN when it is set — as an environment variable, via
# `go env -w GOBIN=...`, or on the make command line — and GOPATH/bin otherwise. The destination has
# to be a directory on PATH, because gh-write is invoked as a bare command in the Bash call the hook
# is watching. GOPATH/bin is on nobody's PATH by default, so hardcoding it leaves an operator whose
# tools live somewhere else (~/.local/bin, say, alongside cope-gate and basanite) copying two
# binaries by hand after every build, and running a stale one the first time they forget.
GOBIN ?= $(shell go env GOBIN)
ifeq ($(strip $(GOBIN)),)
GOBIN := $(shell go env GOPATH)/bin
endif

.PHONY: build install test check-readme

build:
	go build $(LDFLAGS) -o bin/ticketvoice .
	go build $(LDFLAGS) -o bin/gh-write ./cmd/gh-write

install:
	go build $(LDFLAGS) -o $(GOBIN)/ticketvoice .
	go build $(LDFLAGS) -o $(GOBIN)/gh-write ./cmd/gh-write

test:
	go test ./...

# Runs the tool's own gate against the one paragraph in README.md written in ticket-body register —
# the Why section — as if it were a Linear issue description. Matches cope's `make check-readme`
# (cope-gate --check README.md) and effigy's generate_readme.py: dogfood the mechanism on the docs,
# not a separate prose-quality pass by hand. The rest of the README is documentation, not a ticket,
# and would fail a 150-word budget by design — only this section is a fair target.
check-readme: build
	@awk '/^## Why$$/{f=1;next} /^## /{if (f) exit} f' README.md | ./bin/ticketvoice --check -
