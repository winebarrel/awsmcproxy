.PHONY: all
all: vet test build

.PHONY: vet
vet:
	go vet ./...

.PHONY: test
test:
	go test -v $(TEST_OPTS) ./...

.PHONY: build
build:
	go build ./cmd/awsmcproxy

.PHONY: install
install:
	go install ./cmd/awsmcproxy

.PHONY: lint
lint:
	golangci-lint run
