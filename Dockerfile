# syntax=docker/dockerfile:1
# Multi-stage build → ~15MB static binary.
# Stage 1: build
FROM golang:1.27-alpine AS build
WORKDIR /src

# Cache deps separately.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG TARGETARCH=amd64
ENV CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH}
RUN go build -trimpath -ldflags="-s -w -X main.Version=${VERSION}" -o /out/server ./cmd/server

# Stage 2: runtime
FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/server /server
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/server"]
