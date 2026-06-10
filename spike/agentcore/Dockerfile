# naozhi cloud-sandbox base image (Phase 0 spike, RFC §2.1 "烤进镜像" layer)
# Bakes: claude CLI binary + generic toolchain + bootstrap handler.
# Tenant layer (settings/skills/CLAUDE.md/prompt) is injected per-invocation.
#
# AgentCore Runtime requires linux/arm64 — build on the aarch64 host directly.

FROM golang:1.24 AS build
WORKDIR /src
COPY bootstrap/ .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /bootstrap .

FROM public.ecr.aws/amazonlinux/amazonlinux:2023
RUN dnf install -y git tar gzip unzip which findutils procps-ng python3 \
    && dnf clean all && rm -rf /var/cache/dnf

# claude CLI: ARM64 glibc binary copied from the host install (build context
# must contain ./claude — see build.sh).
COPY claude /usr/local/bin/claude
RUN chmod 0755 /usr/local/bin/claude

COPY --from=build /bootstrap /usr/local/bin/bootstrap

# Non-root user: claude CLI refuses --dangerously-skip-permissions as root.
RUN useradd -m -u 1001 agent
USER agent
WORKDIR /home/agent

ENV PORT=8080
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/bootstrap"]
