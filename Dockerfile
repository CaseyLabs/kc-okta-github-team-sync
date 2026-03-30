FROM golang:1.25.6-bookworm

WORKDIR /workspace

# Allow non-root users to write to Go caches when running via docker compose.
RUN mkdir -p /go/pkg/mod /go/cache \
	&& chmod -R 0777 /go \
	&& groupadd -g 1000 app \
	&& useradd -m -u 1000 -g 1000 -s /bin/bash app

ENV GOMODCACHE=/go/pkg/mod
ENV GOCACHE=/go/cache
USER app
