BINARY := siptooth
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/volodymyrsmirnov/siptooth/internal/buildinfo.Version=$(VERSION)

.PHONY: build check test race fmt vet cross dist deploy

build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/siptooth

# Real file target: rebuilds only when a source file changed.
$(BINARY): $(shell find . -name '*.go' -not -path './.superpowers/*') go.mod go.sum
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/siptooth

test:
	go test ./...

race:
	go test -race -count=1 ./...

fmt:
	@test -z "$$(gofmt -l . | tee /dev/stderr)" || (echo "gofmt: files need formatting" && exit 1)

vet:
	go vet ./...

cross:
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o /dev/null ./...

check: fmt vet test cross

# Release binaries: dist/siptooth-linux-arm64, dist/siptooth-linux-amd64 and SHA256SUMS.
dist:
	rm -rf dist && mkdir -p dist
	for arch in arm64 amd64; do \
		GOOS=linux GOARCH=$$arch CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o dist/siptooth-linux-$$arch ./cmd/siptooth || exit 1; \
	done
	cd dist && shasum -a 256 siptooth-linux-* > SHA256SUMS

# Build for the Pi and copy the binary to ~/.local/bin/siptooth there. The running daemon keeps
# the old binary until restarted: `systemctl --user restart siptooth` on the Pi.
PI_HOST ?= pi@rpi-pt
deploy:
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o dist/siptooth-pi ./cmd/siptooth
	scp dist/siptooth-pi $(PI_HOST):.local/bin/siptooth.new
	ssh $(PI_HOST) 'chmod +x ~/.local/bin/siptooth.new && mv ~/.local/bin/siptooth.new ~/.local/bin/siptooth && ~/.local/bin/siptooth version'
