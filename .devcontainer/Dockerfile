FROM mcr.microsoft.com/devcontainers/go:1-1.22-bookworm

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        git ca-certificates bash-completion && \
    rm -rf /var/lib/apt/lists/*

ENV CGO_ENABLED=0
