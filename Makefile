.PHONY: verify fmt fmt-check mod-verify test coverage vet build image image-controller image-node image-combined image-version-check release-workflow-test shellcheck actionlint helm-lint helm-template linux-mount-integration

CONTROLLER_VERSION_FILE ?= versions/controller
NODE_VERSION_FILE ?= versions/node
CONTROLLER_VERSION ?= $(shell cat $(CONTROLLER_VERSION_FILE))
NODE_VERSION ?= $(shell cat $(NODE_VERSION_FILE))
CONTROLLER_IMAGE ?= shiftpv-controller:$(CONTROLLER_VERSION)
NODE_IMAGE ?= shiftpv-node:$(NODE_VERSION)
IMAGE ?= shiftpv:dev
COVERAGE_MIN ?= 80
COVERAGE_PACKAGES := ./src/csi/... ./src/kubernetes/... ./src/mobility/... ./src/node/... ./src/volume/... ./test/...

verify: fmt-check mod-verify coverage vet build image-version-check release-workflow-test shellcheck actionlint helm-lint helm-template

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

image: image-controller image-node

image-controller: image-version-check
	docker build --target controller --build-arg CONTROLLER_VERSION=$(CONTROLLER_VERSION) -f build/package/Dockerfile -t $(CONTROLLER_IMAGE) .

image-node: image-version-check
	docker build --target node --build-arg NODE_VERSION=$(NODE_VERSION) -f build/package/Dockerfile -t $(NODE_IMAGE) .

image-combined: image-version-check
	docker build --target combined --build-arg CONTROLLER_VERSION=$(CONTROLLER_VERSION) --build-arg NODE_VERSION=$(NODE_VERSION) -f build/package/Dockerfile -t $(IMAGE) .

image-version-check:
	@for file in $(CONTROLLER_VERSION_FILE) $(NODE_VERSION_FILE); do \
		test -f "$$file" || { echo "image version file not found: $$file" >&2; exit 1; }; \
		grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$$' "$$file" || { echo "image version must use numeric major.minor.patch format: $$file" >&2; exit 1; }; \
	done

release-workflow-test:
	./test/release/resolve-image-release.sh

shellcheck:
	shellcheck build/ci/resolve-image-release.sh test/release/resolve-image-release.sh test/e2e/kind/run.sh test/e2e/kind/filesystem-faults.sh test/e2e/kind/mobility/run.sh test/integration/linux-mount/run.sh

actionlint:
	@if command -v actionlint >/dev/null 2>&1; then \
		actionlint; \
	else \
		echo "actionlint not installed; skipping local workflow lint"; \
	fi

linux-mount-integration:
	./test/integration/linux-mount/run.sh

helm-lint:
	helm lint charts/shiftpv

helm-template:
	helm template shiftpv charts/shiftpv --namespace shiftpv-system --kube-version 1.35.8 >/dev/null
	@rendered="$$(helm template shiftpv charts/shiftpv --namespace shiftpv-system --kube-version 1.35.8 \
		--set controller.image.repository=controller --set controller.image.tag=test \
		--set node.image.repository=node --set node.image.tag=test \
		--set mobility.helperImage=helper:test)"; \
		printf '%s\n' "$$rendered" | grep -q 'image: "controller:test"'; \
		printf '%s\n' "$$rendered" | grep -q 'image: "node:test"'; \
		printf '%s\n' "$$rendered" | grep -q -- '--mobility-helper-image=helper:test'; \
		printf '%s\n' "$$rendered" | grep -q 'mountPropagation: HostToContainer'
