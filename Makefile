.DEFAULT_GOAL := all

.PHONY: all update build sbom sbom-image service-build publish-build publish

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

publish: publish-build sbom-image
	@echo "Publishing..."
	podman tag $(SERVICE_NAME) $(IMAGE) && podman push $(IMAGE)
	cosign attach sbom --sbom sbom.json $(IMAGE)
