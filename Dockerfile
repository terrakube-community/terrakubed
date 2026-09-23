# Build Stage
# Use BUILDPLATFORM so the builder runs natively (fast cross-compilation via Go,
# no QEMU emulation needed for the build step).
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS builder

ARG TARGETOS=linux
ARG TARGETARCH

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o terrakubed cmd/terrakubed/main.go

# Final Stage
FROM alpine:3.19

WORKDIR /app

# Install dependencies required by Registry (git, openssh-client)
# and Executor (curl, unzip, bash)
RUN apk add --no-cache git openssh-client curl unzip bash ca-certificates jq

# Alpine 3.19 ships OpenSSL 3.x, which disables the "legacy" provider by
# default. Without it, the system ssh client (used via GIT_SSH_COMMAND for
# terraform-init module downloads over git+ssh) fails to parse some
# older-format private keys — e.g. PKCS#1 "-----BEGIN RSA PRIVATE KEY-----"
# keys from older tooling — with "Load key ...: error in libcrypto". Java's
# executor never hit this: it used a pure-Java SSH/crypto stack (JGit +
# Apache MINA SSHD) instead of the system ssh binary, so it was unaffected
# by OpenSSL's provider changes. Enabling the legacy provider restores the
# same key formats Java could handle.
RUN printf '\n[openssl_init]\nproviders = provider_sect\n\n[provider_sect]\ndefault = default_sect\nlegacy = legacy_sect\n\n[default_sect]\nactivate = 1\n\n[legacy_sect]\nactivate = 1\n' >> /etc/ssl/openssl.cnf

# Ensure cache directory exists and is writable for Terraform
RUN mkdir -p /home/app/.terrakube/terraform-versions && \
  chmod -R 777 /home/app

COPY --from=builder /app/terrakubed .

# Expose all service ports
EXPOSE 8080
EXPOSE 8075
EXPOSE 8090

# Default to executor service type (can be overridden via env)
ENV SERVICE_TYPE=executor

CMD ["./terrakubed"]
