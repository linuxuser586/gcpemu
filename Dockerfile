# NFR-PORT-004: the multi-arch OCI image. The build context is dist/ after
# `task release`, so the image holds the same static binaries as the
# GitHub release and its SHA256SUMS:
#
#   docker buildx build --platform linux/amd64,linux/arm64 -f Dockerfile dist
#
# Every Service starts by default. Cloud SQL, GKE and Cloud NAT need the
# host's Docker socket (Docker-outside-of-Docker, ADR 0003); without it they
# are skipped. The emulator runs unprivileged and needs only the socket's
# group, read the way the runtime mounts the socket, which works from bash,
# zsh and PowerShell on every host (Git Bash needs MSYS_NO_PATHCONV=1):
#
#   docker run -v /var/run/docker.sock:/var/run/docker.sock \
#     --group-add "$(docker run --rm -v /var/run/docker.sock:/s busybox:1.38 stat -c %g /s)" \
#     -p 4562:4562 ... IMAGE
FROM gcr.io/distroless/static-debian13:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99

ARG TARGETARCH
COPY --chmod=0755 gcpemu-linux-${TARGETARCH} /gcpemu

# Listeners must be reachable from outside the container. IAM stays in
# audit mode, and the Web console is not served beyond loopback.
ENV GCPEMU_BIND=0.0.0.0 \
    GCPEMU_DATA_DIR=/data

# Created owned by the nonroot user; mount a volume here to keep state.
WORKDIR /data

# API gateway, gcs, pubsub, ar, dns, metadata.
EXPOSE 4562 4443 8085 4561 5353/tcp 5353/udp 8988

HEALTHCHECK --interval=5s --timeout=5s --start-period=60s CMD ["/gcpemu", "status", "--ready"]

ENTRYPOINT ["/gcpemu"]
CMD ["start"]
