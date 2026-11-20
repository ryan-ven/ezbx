# EZBox gateway image: the ezbx binary alone, running as a non-root user.
#
# Build from the repository root:
#   docker build -f docker/gateway.Dockerfile -t ezbx-gateway:dev .
#
# The final image is "scratch": no shell, no package manager, no other files.
# Run it with a read-only root filesystem and every capability dropped.

FROM golang:1.27.4 AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
COPY providers ./providers
# CGO off gives a static binary that needs no C library at run time.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w -buildid=" -o /out/ezbx ./cmd/ezbx

FROM scratch
COPY --from=build /out/ezbx /ezbx
USER 65532:65532
EXPOSE 8000
LABEL ezbx.managed="true"
ENTRYPOINT ["/ezbx"]
CMD ["gateway", "serve", "--listen", "0.0.0.0:8000"]
