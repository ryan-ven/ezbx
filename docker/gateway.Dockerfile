# EZBox gateway image: the ezbx binary alone, running as a non-root user.
#
# Build from the repository root:
#   docker build -f docker/gateway.Dockerfile -t ezbx-gateway:dev .
#
# The final image is "scratch": no shell, no package manager, no other files.
# Run it with a read-only root filesystem and every capability dropped.

FROM golang:1.27.4 AS build
WORKDIR /src
COPY go.mod go.sum ./
# Fetch the modules listed in go.sum first; their hashes are checked.
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY providers ./providers
COPY harnesses ./harnesses
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
