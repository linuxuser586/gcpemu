# #862: the Conformance suite's test backend, published with each release
# as docker.io/linuxuser586/gcpemu-echo. The build context is dist-echo/
# after `task echo:dist`:
#
#   docker buildx build --platform linux/amd64,linux/arm64 -f cmd/gcpemu-echo/Dockerfile dist-echo
#
# It listens on PORT (8080 by default) and reports the headers named in
# ECHO_HEADERS:
#
#   docker run -e ECHO_HEADERS=x-test,cookie -p 8080:8080 IMAGE
FROM gcr.io/distroless/static-debian13:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99

ARG TARGETARCH
COPY --chmod=0755 gcpemu-echo-linux-${TARGETARCH} /gcpemu-echo

EXPOSE 8080

ENTRYPOINT ["/gcpemu-echo"]
