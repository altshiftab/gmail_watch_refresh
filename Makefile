.DEFAULT_GOAL := all

.PHONY: all update build sbom sbom-image service-build guard-release-versions publish-build publish

ENV ?= prod

CNF_FILE = .env.$(ENV)

ifneq (,$(wildcard $(CNF_FILE)))
    include $(CNF_FILE)
    export $(shell sed 's/=.*//' $(CNF_FILE))
endif

SERVICE_NAME ?= gmail-watch-refresh
IMAGE ?= europe-north2-docker.pkg.dev/$(GCP_PROJECT_ID)/images/$(SERVICE_NAME):latest

update:
	gm

service-build:
	@echo "Building..."
	go build -o service

SBOM_FLAGS = --dockerfile Dockerfile --output sbom.json

# Follows service-build, because it reads the binary.
sbom: service-build
	@echo "Generating SBOM..."
	go tool sbom --go-binary service $(SBOM_FLAGS)

# Describes the image, reading the binary inside it; this is what publishing attaches.
sbom-image: publish-build
	@echo "Generating SBOM of the image..."
	go tool sbom --image $(SERVICE_NAME) $(SBOM_FLAGS)

build: service-build sbom

all: update build

publish-build:
	@echo "Building for publish..."
	podman build . --pull=newer --tag $(SERVICE_NAME)

# What is published is built from releases of my modules, never from a commit under test (a
# pseudo-version): https://wiki.vvvp.se/?page=go-module-versions. GOWORK=off reads the module graph the
# image is built from, and a failing go list fails the guard rather than passing it. A replaced module
# is checked as what replaces it: a directory in the repo has no version, and a replace onto a
# pseudo-version is caught.
guard-release-versions:
	@modules=$$(GOWORK=off go list -m -f '{{with .Replace}}{{.Path}} {{.Version}}{{else}}{{.Path}} {{.Version}}{{end}}' all) && \
	test_versions=$$(printf '%s\n' "$$modules" | grep -E '^github\.com/(Motmedel|altshiftab|vphpersson|intilgroup)/[^ ]* v[^ ]*[0-9]{14}-[0-9a-f]{12}$$' || true) && \
	if [ -n "$$test_versions" ]; then printf '[deploy] Built from test versions of my modules; release them first:\n%s\n' "$$test_versions" >&2; exit 1; fi

publish: guard-release-versions publish-build sbom-image
	@echo "Publishing..."
	podman tag $(SERVICE_NAME) $(IMAGE) && podman push $(IMAGE)
	cosign attach sbom --sbom sbom.json $(IMAGE)
