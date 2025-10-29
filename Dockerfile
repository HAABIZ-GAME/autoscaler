# syntax=docker/dockerfile:1.7

FROM --platform=$BUILDPLATFORM golang:1.25-bookworm AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/autoscaler .

FROM gcr.io/distroless/static:nonroot AS runtime
WORKDIR /home/service
COPY --from=build /out/autoscaler /usr/local/bin/autoscaler
EXPOSE 8000
ENV PORT=8000
ENTRYPOINT ["/usr/local/bin/autoscaler"]
