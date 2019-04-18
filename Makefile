BUILD_DATE = `date +%FT%T%z`
BUILD_USER = $(USER)@`hostname`
VERSION = `git describe --tags`

# command to build and run on the local OS.
GO_BUILD = go build

# command to compiling the distributable. Specify GOOS and GOARCH for the
# target OS.
GO_DIST = CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO_BUILD) -a -tags netgo -ldflags "-w -X main.buildVersion=$(VERSION) -X main.buildDate=$(BUILD_DATE) -X main.buildUser=$(BUILD_USER)"

BINARY=smartcontractd

# tools
BINARY_CONTRACT_CLI=smartcontract

all: clean prepare deps test dist

ci: all lint

deps:
	go get -t ./...

dist: dist-smartcontractd dist-tools

dist-smartcontractd:
	$(GO_DIST) -o dist/$(BINARY) cmd/$(BINARY)/main.go

dist-tools: dist-cli

dist-cli:
	$(GO_DIST) -o dist/$(BINARY_CONTRACT_CLI) cmd/$(BINARY_CONTRACT_CLI)/main.go

prepare:
	mkdir -p dist tmp

prepare-win:
	mkdir dist | echo dist exists
	mkdir tmp | echo tmp exists

# build a version suitable for running locally
build-smartcontractd-win:
	go build -o dist\$(BINARY) cmd\$(BINARY)\main.go

build-win: prepare-win build-smartcontractd-win

tools:
	go get golang.org/x/tools/cmd/goimports
	go get github.com/golang/lint/golint

run:
	go run -race cmd/$(BINARY)/main.go

run-sync:
	go run cmd/$(BINARY_CONTRACT_CLI)/main.go sync

lint: golint vet goimports

vet:
	go vet

golint:
	ret=0 && test -z "$$(golint ./... | tee /dev/stderr)" || ret=1 ; exit $$ret

goimports:
	ret=0 && test -z "$$(goimports -l ./... | tee /dev/stderr)" || ret=1 ; exit $$ret

test: prepare
	go test ./...

test-win: prepare-win
	go test ./...

clean:
	rm -rf dist
