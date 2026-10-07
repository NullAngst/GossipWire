# Build stage: compiles one static binary, no cgo.
# golang:1 follows the newest Go release, so a rebuild with --pull picks up
# Go's security fixes without anyone bumping a version here. go.mod only sets
# the minimum Go version, so newer releases build it fine.
FROM golang:1-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/gossipwire ./cmd/gossipwire \
 && mkdir -p /out/data

# Run stage: no shell, no package manager, runs as UID 65532.
# Holds only CA certificates, time zone data and a passwd file; the bot is
# a static binary, so nearly everything that needs patching lives in the
# build stage above.
FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/gossipwire /gossipwire
COPY --from=build --chown=65532:65532 /out/data /data
USER 65532:65532
EXPOSE 8081
ENTRYPOINT ["/gossipwire"]
