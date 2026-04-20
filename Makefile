IMAGE_NAME ?= zenfeed
REGISTRY ?= ghcr.io/bustesoul
FULL_IMAGE_NAME = $(REGISTRY)/$(IMAGE_NAME)


.PHONY: test push

test:
	go test -race -v -coverprofile=coverage.out -coverpkg=./... ./...

push:
	docker buildx create --use --name multi-platform-builder || true
	docker buildx build --platform linux/amd64,linux/arm64 \
		-t $(FULL_IMAGE_NAME):dev \
		--cache-from type=registry,ref=$(FULL_IMAGE_NAME):buildcache \
		--cache-to type=registry,ref=$(FULL_IMAGE_NAME):buildcache,mode=max \
		--push .
