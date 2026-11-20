# Throwaway probe image for the isolation test. Not part of a box.
#
# Build from the repository root:
#   docker build -f docker/probe.Dockerfile -t ezbx-probe:dev .

FROM golang:1.27.4 AS build
WORKDIR /src
COPY go.mod go.sum ./
# Fetch the modules listed in go.sum first; their hashes are checked.
RUN go mod download
COPY tools ./tools
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w -buildid=" -o /out/isoprobe ./tools/isoprobe

FROM scratch
COPY --from=build /out/isoprobe /isoprobe
USER 65532:65532
LABEL ezbx.managed="true"
ENTRYPOINT ["/isoprobe"]
