GEN_DIR := gen/computepb
MODULE := github.com/rsturla/openshell-drivers-contrib
BUF_VERSION := 1.71.0
PROTOC_GEN_GO_VERSION := v1.36.11
PROTOC_GEN_GO_GRPC_VERSION := 1.6.2
GOLANGCI_LINT_VERSION := v2.12.2
GOVULNCHECK_VERSION := v1.1.4
ACTIONLINT_VERSION := v1.7.12
MARKDOWNLINT_VERSION := 0.20.0
VERSION ?= devel
LDFLAGS_EC2 := -X $(MODULE)/drivers/ec2.DriverVersion=$(VERSION)
LDFLAGS_KV  := -X $(MODULE)/drivers/kubevirt.DriverVersion=$(VERSION)

.PHONY: build build-ec2 build-kubevirt build-cross proto proto-check proto-lint check-proto-tools install-proto-tools install-golangci-lint install-govulncheck install-actionlint test test-unit test-integration test-fuzz lint lint-go lint-golangci markdownlint tidy-check gen-check govulncheck workflow-lint clean

build: build-ec2 build-kubevirt

build-ec2:
	go build -ldflags '$(LDFLAGS_EC2)' -o bin/openshell-driver-ec2 ./cmd/openshell-driver-ec2

build-kubevirt:
	go build -ldflags '$(LDFLAGS_KV)' -o bin/openshell-driver-kubevirt ./cmd/openshell-driver-kubevirt

build-cross:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags '$(LDFLAGS_EC2)' -o bin/openshell-driver-ec2-linux-amd64 ./cmd/openshell-driver-ec2
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags '$(LDFLAGS_EC2)' -o bin/openshell-driver-ec2-linux-arm64 ./cmd/openshell-driver-ec2
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags '$(LDFLAGS_KV)' -o bin/openshell-driver-kubevirt-linux-amd64 ./cmd/openshell-driver-kubevirt
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags '$(LDFLAGS_KV)' -o bin/openshell-driver-kubevirt-linux-arm64 ./cmd/openshell-driver-kubevirt

check-proto-tools:
	@test "$$(buf --version)" = "$(BUF_VERSION)" || \
		{ echo "buf $(BUF_VERSION) is required"; exit 1; }
	@test "$$(protoc-gen-go --version | awk '{print $$2}')" = "$(PROTOC_GEN_GO_VERSION)" || \
		{ echo "protoc-gen-go $(PROTOC_GEN_GO_VERSION) is required"; exit 1; }
	@test "$$(protoc-gen-go-grpc --version | awk '{print $$2}')" = "$(PROTOC_GEN_GO_GRPC_VERSION)" || \
		{ echo "protoc-gen-go-grpc $(PROTOC_GEN_GO_GRPC_VERSION) is required"; exit 1; }

install-proto-tools:
	go install github.com/bufbuild/buf/cmd/buf@v$(BUF_VERSION)
	go install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v$(PROTOC_GEN_GO_GRPC_VERSION)

install-golangci-lint:
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

install-govulncheck:
	go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)

install-actionlint:
	go install github.com/rhysd/actionlint/cmd/actionlint@$(ACTIONLINT_VERSION)

proto: check-proto-tools
	buf generate

proto-check: check-proto-tools
	@tmp=$$(mktemp -d); \
	trap 'rm -rf "$$tmp"' EXIT; \
	buf generate --output "$$tmp"; \
	diff -ru $(GEN_DIR) "$$tmp/$(GEN_DIR)"

proto-lint: check-proto-tools
	buf lint

test: test-unit

test-unit:
	go test -race -count=1 ./...

# Listener-based server tests automatically run where the sandbox permits
# socket creation; this target is retained as the CI/operator entry point.
test-integration:
	go test -race -count=1 ./cmd/... ./pkg/server/...

test-fuzz:
	go test -run=^$$ -fuzz=FuzzParseCPUQuantity -fuzztime=30s ./drivers/ec2/
	go test -run=^$$ -fuzz=FuzzParseMemoryQuantity -fuzztime=30s ./drivers/ec2/

lint: lint-go lint-golangci markdownlint

lint-go:
	@test -z "$$(gofmt -l $$(find . -name '*.go' -not -path './vendor/*'))" || \
		{ gofmt -d $$(find . -name '*.go' -not -path './vendor/*'); exit 1; }
	go vet ./...

lint-golangci:
	golangci-lint run ./...

tidy-check:
	go mod tidy -diff

gen-check: proto-check

markdownlint:
	npx --yes markdownlint-cli2@$(MARKDOWNLINT_VERSION) README.md 'docs/**/*.md'

govulncheck:
	govulncheck ./...

workflow-lint:
	actionlint

clean:
	rm -rf bin/ dist/
