# Simple Makefile for the autoscaler app

BIN ?= autoscaler
IMAGE ?= autoscaler:latest
PORT ?= 8000
PLATFORM ?= linux/amd64

.PHONY: all build run clean fmt tidy docker-build docker-buildx docker-run

all: build

build:
	@mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/$(BIN) .

run:
	PORT=$(PORT) go run .

clean:
	rm -rf bin

fmt:
	go fmt ./...

tidy:
	go mod tidy

docker-build:
	DOCKER_BUILDKIT=1 docker build -t $(IMAGE) .

# Requires Docker Buildx
docker-buildx:
	docker buildx build --platform $(PLATFORM) -t $(IMAGE) --load .

docker-run:
	docker run --rm -p $(PORT):$(PORT) -e PORT=$(PORT) $(IMAGE)

