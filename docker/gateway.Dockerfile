# EZBox gateway image: the ezbx binary alone, running as a non-root user.
#
# ezbx build builds it from the files carried inside the ezbx program. To
# build it by hand from the repository root (for development):
#   docker build -f docker/gateway.Dockerfile -t ezbx-gateway:dev .
#
# The final image is "scratch": no shell, no package manager, no other files.
# Run it with a read-only root filesystem and every capability dropped.

# Go, pinned by digest so a rebuild compiles with the same toolchain. The
# value is listed in ezbx.lock.
ARG GO_IMAGE=golang:1.27.4@sha256:205c7636d74e0ffbe3d007162df7d74f859f5a8c8322dfb9f907977686c126cf

FROM ${GO_IMAGE} AS build
WORKDIR /src
COPY go.mod go.sum ./
# Fetch the modules listed in go.sum first; their hashes are checked.
RUN go mod download
# source.go embeds docker/ and the other folders, so they must all be here.
COPY source.go ./
COPY cmd ./cmd
COPY internal ./internal
COPY providers ./providers
COPY harnesses ./harnesses
COPY catalog ./catalog
COPY docker ./docker
# CGO off gives a static binary that needs no C library at run time.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w -buildid=" -o /out/ezbx ./cmd/ezbx

FROM scratch
COPY --from=build /out/ezbx /ezbx
USER 65532:65532
# 8000 is the model route and 3128 the egress proxy. The admin API listens
# only on the container's own loopback and is never published.
EXPOSE 8000 3128
LABEL ezbx.managed="true"
# There is no shell or curl in the image, so the healthcheck uses ezbx itself.
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD ["/ezbx", "gateway", "health", "--addr", "127.0.0.1:8000"]
ENTRYPOINT ["/ezbx"]
CMD ["gateway", "serve", "--listen", "0.0.0.0:8000"]
