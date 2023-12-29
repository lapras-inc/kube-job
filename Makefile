.PHONY: all mod build

OUTPUT = kube-job
OUTDIR = bin
BUILD_CMD = go build -a -tags netgo -installsuffix netgo -ldflags \
" \
  -extldflags '-static' \
  -X github.com/h3poteto/kube-job/cmd.version=$(shell git describe --tag --abbrev=0) \
  -X github.com/h3poteto/kube-job/cmd.revision=$(shell git rev-list -1 HEAD) \
  -X github.com/h3poteto/kube-job/cmd.build=$(shell git describe --tags) \
"
VERSION = $(shell git describe --tag --abbrev=0)

all: mac linux windows macarm

mod: go.mod
	go mod download

build: mod
	GOOS=linux GOARCH=amd64 $(BUILD_CMD) -o $(OUTPUT)

mac: mod
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 $(BUILD_CMD) -o $(OUTPUT)
	zip $(OUTDIR)/$(OUTPUT)_${VERSION}_darwin_amd64.zip $(OUTPUT)
macarm: mod
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(BUILD_CMD) -o $(OUTPUT)
	zip $(OUTDIR)/$(OUTPUT)_${VERSION}_darwin_arm64.zip $(OUTPUT)
linux: mod
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(BUILD_CMD) -o $(OUTPUT)
	zip $(OUTDIR)/$(OUTPUT)_${VERSION}_linux_amd64.zip $(OUTPUT)

windows: mod
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 $(BUILD_CMD) -o $(OUTPUT).exe
	zip $(OUTDIR)/$(OUTPUT)_${VERSION}_windows_amd64.zip $(OUTPUT).exe

test:
	mkdir -p coverage
	go test -coverprofile=coverage/coverage.out -cover -v ./pkg/...
	go tool cover -html=coverage.out -o coverage/reports.html

e2e: build
	which kind ginkgo > /dev/null
	kind get kubeconfig > "$(CURDIR)/.config.yml"
	KUBECONFIG="$(CURDIR)/.config.yml" ginkgo -r ./e2e
