APP := ud-reselling-dyndns
IMAGE ?= ghcr.io/simoncahill/ud-reselling-dyndns:latest
GO ?= go
DOCKER ?= docker
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo devel)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: container standalone windows windows-arm64 clean test

container:
	$(DOCKER) build --build-arg VERSION=$(VERSION) --file Containerfile --tag $(IMAGE) .

standalone:
	mkdir -p bin
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o bin/$(APP) ./src

windows:
	mkdir -p bin/windows-amd64
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o bin/windows-amd64/$(APP).exe ./src

windows-arm64:
	mkdir -p bin/windows-arm64
	GOOS=windows GOARCH=arm64 CGO_ENABLED=0 $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o bin/windows-arm64/$(APP).exe ./src

test:
	$(GO) test ./...

clean:
	rm -rf bin
