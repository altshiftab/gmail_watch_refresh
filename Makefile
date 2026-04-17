.DEFAULT_GOAL := all

.PHONY: all update build sbom service-build publish-build publish

ENV ?= prod

CNF_FILE = .env.$(ENV)

ifneq (,$(wildcard $(CNF_FILE)))
    include $(CNF_FILE)
    export $(shell sed 's/=.*//' $(CNF_FILE))
endif

update:
	gm

sbom:
	@echo "Generating SBOM..."
	sbom -go go.sum -docker Dockerfile -output sbom.json

service-build:
	@echo "Building..."
	GOEXPERIMENT=jsonv2 go build -o ../service

build: service-build sbom

all: update build

publish-build:
	@echo "Building for publish..."
	podman build . --tag gmail-watch-refresh

publish: publish-build sbom
	@echo "Publishing..."
	podman tag gmail-watch-refresh europe-north2-docker.pkg.dev/$(GCP_PROJECT_ID)/images/gmail-watch-refresh:latest && podman push europe-north2-docker.pkg.dev/$(GCP_PROJECT_ID)/images/gmail-watch-refresh:latest
	cosign attach sbom --sbom sbom.json europe-north2-docker.pkg.dev/$(GCP_PROJECT_ID)/images/gmail-watch-refresh:latest
