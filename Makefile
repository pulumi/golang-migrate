VERSION ?= $(shell git describe --tags 2>/dev/null | cut -c 2-)
TEST_FLAGS ?=
COVERAGE_DIR ?= .coverage
CLI_BUILD_OUTPUT ?= /go/bin/migratecli

build:
	CGO_ENABLED=0 go build -ldflags='-X main.Version=$(VERSION)' ./cmd/migrate

build-cli:
	cd ./cmd/migrate && CGO_ENABLED=0 go build -a -o $(CLI_BUILD_OUTPUT) -ldflags='-X main.Version=$(VERSION) -extldflags "-static"' .

test-short:
	$(MAKE) test-with-flags --ignore-errors TEST_FLAGS='-short'

test:
	@-rm -r $(COVERAGE_DIR)
	@mkdir $(COVERAGE_DIR)
	$(MAKE) test-with-flags TEST_FLAGS='-v -race -covermode atomic -coverprofile $$(COVERAGE_DIR)/combined.txt -bench=. -benchmem -timeout 20m'

test-with-flags:
	@go test $(TEST_FLAGS) ./...

kill-orphaned-docker-containers:
	docker rm -f $(shell docker ps -aq --filter label=migrate_test)

html-coverage:
	go tool cover -html=$(COVERAGE_DIR)/combined.txt

# example: make release V=0.0.0
release:
	git tag v$(V)
	@read -p "Press enter to confirm and push to origin ..." && git push origin v$(V)

.PHONY: build build-cli test-short test test-with-flags html-coverage \
        release kill-orphaned-docker-containers

SHELL = /bin/sh
