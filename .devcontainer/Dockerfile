FROM mcr.microsoft.com/devcontainers/go:dev-1-bookworm

RUN apt-get install -y --no-install-recommends \
        git ca-certificates bash-completion && \
    rm -rf /var/lib/apt/lists/*

RUN go install github.com/goreleaser/goreleaser/v2@latest && \
    chown -R vscode:vscode /go

ENV CGO_ENABLED=0

# git tag v0.7.20
# goreleaser release --snapshot --clean --skip=publish
# 