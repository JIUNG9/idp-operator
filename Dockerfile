# Build
FROM golang:1.26 AS build
WORKDIR /src

# Dependencies first, so a code change doesn't re-download the module graph.
COPY go.mod go.sum ./
RUN go mod download

COPY main.go ./
COPY api/ api/
COPY internal/ internal/

# Static binary: the runtime image has no libc to link against.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/manager .

# Run
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=build /out/manager /manager
USER 65532:65532
ENTRYPOINT ["/manager"]
