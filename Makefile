.PHONY: verify fmt fmt-check mod-verify test coverage vet build image shellcheck helm-lint helm-template linux-mount-integration

VERSION ?= dev
IMAGE ?= shiftpv:dev
COVERAGE_MIN ?= 80
COVERAGE_PACKAGES := ./src/csi/... ./src/kubernetes/... ./src/node/... ./src/volume/... ./test/...

verify: fmt-check mod-verify coverage vet build shellcheck helm-lint helm-template

fmt:
	gofmt -w $$(find src test -name '*.go' -type f)

fmt-check:
	@test -z "$$(gofmt -l src test)" || { gofmt -l src test; exit 1; }

mod-verify:
	go mod verify

test: coverage

coverage:
	mkdir -p .tmp
	go test -race -covermode=atomic -coverprofile=.tmp/coverage.out $(COVERAGE_PACKAGES)
	go tool cover -func=.tmp/coverage.out | tee .tmp/coverage.txt
	@total=$$(awk '/^total:/ {gsub(/%/, "", $$3); print $$3}' .tmp/coverage.txt); \
		awk -v total="$${total}" -v minimum="$(COVERAGE_MIN)" 'BEGIN { \
			if (total + 0 < minimum + 0) { \
				printf "coverage %.1f%% is below %.1f%%\n", total, minimum; \
				exit 1; \
			} \
			printf "coverage %.1f%% meets %.1f%% minimum\n", total, minimum; \
		}'

vet:
	go vet ./...

build:
	go build ./src/cmd/controller ./src/cmd/node

image:
	docker build --build-arg VERSION=$(VERSION) -f build/package/Dockerfile -t $(IMAGE) .

shellcheck:
	shellcheck test/e2e/kind/run.sh test/e2e/kind/filesystem-faults.sh test/integration/linux-mount/run.sh

linux-mount-integration:
	./test/integration/linux-mount/run.sh

helm-lint:
	helm lint charts/shiftpv

helm-template:
	helm template shiftpv charts/shiftpv --namespace shiftpv-system --kube-version 1.35.8 >/dev/null
