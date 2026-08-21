VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test vet fmt dist clean

build:
	go build -ldflags "$(LDFLAGS)" -o worklog ./cmd/worklog

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

# Cross-compile the single static binary for every target. modernc.org/sqlite is
# pure Go, so CGO stays off and no per-platform toolchain is needed.
dist:
	CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/worklog-linux-amd64      ./cmd/worklog
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/worklog-windows-amd64.exe ./cmd/worklog
	CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/worklog-darwin-arm64      ./cmd/worklog

clean:
	rm -f worklog worklog.exe
	rm -rf dist
