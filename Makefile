.PHONY: build test vet fmt run image gateway-image site-image ghcr-login regctl tags

# Registry configuration (override on the command line or via the environment).
KO_DOCKER_REPO ?= ghcr.io/vranyes/telnyx-hermes-bridge/gateway
KO_CONFIG_PATH ?= deploy
SITE_REPO ?= ghcr.io/vranyes/telnyx-hermes-bridge/site

# Image tags pushed by the CI targets. In GitHub Actions these default from the
# ambient GITHUB_* environment variables (tag push -> latest + tag name, branch
# push -> latest + short SHA); locally they fall back to latest + short SHA.
GITHUB_REF_TYPE ?=
GITHUB_REF_NAME ?=
ifeq ($(GITHUB_REF_TYPE),tag)
PUSH_TAGS = latest,$(GITHUB_REF_NAME)
else
PUSH_TAGS = latest,$(shell git rev-parse --short=8 HEAD)
endif

build:
	go build -o bin/gateway ./cmd/gateway

test:
	go test ./... -count=1 -race -timeout 120s

vet:
	go vet ./...

fmt:
	gofmt -w .
	go mod tidy

run:
	go run ./cmd/gateway

regctl: ## Install the regctl binary (docker-free registry client)
	@uname_s=$$(uname -s | tr '[:upper:]' '[:lower:]'); \
	uname_m=$$(uname -m); \
	case "$$uname_m" in x86_64|amd64) arch=amd64;; aarch64|arm64) arch=arm64;; *) arch=$$uname_m;; esac; \
	curl -fsSL -o /usr/local/bin/regctl \
		https://github.com/regclient/regclient/releases/download/v0.11.5/regctl-$$uname_s-$$arch; \
	chmod +x /usr/local/bin/regctl; \
	regctl version

ghcr-login: ## Login to GHCR via regctl; no-op outside CI unless GHCR_TOKEN is set
	@token="$${GHCR_TOKEN:-$${GITHUB_TOKEN:-}}"; \
	if [ -z "$$token" ]; then \
		echo "GHCR_TOKEN/GITHUB_TOKEN not set; skipping GHCR login"; \
	else \
		echo "$$token" | regctl registry login ghcr.io \
			--user "$${GHCR_USERNAME:-$${GITHUB_ACTOR:-}}" --pass-stdin; \
	fi

image gateway-image: ## Build and push the gateway image to GHCR with ko
	KO_DOCKER_REPO="$${KO_DOCKER_REPO:-$(KO_DOCKER_REPO)}" \
	KO_CONFIG_PATH="$${KO_CONFIG_PATH:-$(KO_CONFIG_PATH)}" \
		ko build --platform=linux/amd64,linux/arm64 ./cmd/gateway --push --bare --tags "$(PUSH_TAGS)"

site-image: ghcr-login ## Build the mkdocs site and push it to GHCR as a multi-arch index
	python3 -m pip install --quiet 'mkdocs-material==9.6.*'
	mkdocs build --strict --site-dir _site
	regctl image create --platform linux/amd64 ocidir://site:base-amd64
	regctl image create --platform linux/arm64 ocidir://site:base-arm64
	regctl image mod ocidir://site:base-amd64 --create ocidir://site:amd64 --layer-add "dir=_site"
	regctl image mod ocidir://site:base-arm64 --create ocidir://site:arm64 --layer-add "dir=_site"
	regctl index create --ref ocidir://site:amd64 --ref ocidir://site:arm64 ocidir://site:idx
	@repo="$${SITE_REPO:-$(SITE_REPO)}"; \
	first=; \
	for tag in $$(echo "$(PUSH_TAGS)" | tr ',' ' '); do \
		if [ -z "$$first" ]; then src="ocidir://site:idx"; first="$$tag"; else src="$$repo:$$first"; fi; \
		regctl image copy "$$src" "$$repo:$$tag"; \
	done

tags: ## Print the image tags that would be pushed
	@echo "$(PUSH_TAGS)"
