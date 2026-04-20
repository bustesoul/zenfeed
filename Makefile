VERSION ?= $(shell git describe --tags --always)
IMAGE_NAME ?= zenfeed
REGISTRY ?= ghcr.io/bustesoul
FULL_IMAGE_NAME = $(REGISTRY)/$(IMAGE_NAME)


.PHONY: test push dev-push

test:
	go test -race -v -coverprofile=coverage.out -coverpkg=./... ./...

push:
	docker buildx create --use --name multi-platform-builder || true
	docker buildx build --platform linux/amd64,linux/arm64 \
		--build-arg VERSION=$(VERSION) \
		-t $(FULL_IMAGE_NAME):$(VERSION) \
		-t $(FULL_IMAGE_NAME):latest \
		--push .

dev-push:
	docker buildx create --use --name multi-platform-builder || true
	docker buildx build --platform linux/amd64,linux/arm64 \
		--build-arg VERSION=$(VERSION) \
		-t $(FULL_IMAGE_NAME):$(VERSION) \
		--push .
