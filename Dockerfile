# NFR-PORT-004: the multi-arch OCI image. The build context is dist/ after
# `make release`, so the image holds the same static binaries as the
# GitHub release and its SHA256SUMS:
#
#   docker buildx build --platform linux/amd64,linux/arm64 -f Dockerfile dist
#
# Only the Services that need no container runtime start by default; Cloud
# SQL, GKE and Cloud NAT need a Docker socket, which the image does not
# support yet.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

ARG TARGETARCH
COPY --chmod=0755 gcpemu-linux-${TARGETARCH} /gcpemu

# Listeners must be reachable from outside the container. IAM stays in
# audit mode, and the Web console is not served beyond loopback.
ENV GCPEMU_BIND=0.0.0.0 \
    GCPEMU_SERVICES=iam,compute,dns,certs,ar,pubsub,secrets,gcs,lb,cdn \
    GCPEMU_DATA_DIR=/data

# Created owned by the nonroot user; mount a volume here to keep state.
WORKDIR /data

# API gateway, gcs, pubsub, ar, dns, metadata.
EXPOSE 4510 4443 8085 5000 5353/tcp 5353/udp 8988

HEALTHCHECK --interval=5s --timeout=5s --start-period=60s CMD ["/gcpemu", "status", "--ready"]

ENTRYPOINT ["/gcpemu"]
CMD ["start"]
