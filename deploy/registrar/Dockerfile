# Build from the repository root:
#   docker build -f deploy/registrar/Dockerfile -t kwak-registrar:<tag> .
# The base is pinned to an exact tag (spec 11 shows golang:1.26).
FROM golang:1.26.7-alpine3.23 AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags="-s -w" \
      -o /kwak-registrar ./cmd/kwak-registrar

FROM scratch
COPY --from=build /kwak-registrar /kwak-registrar
USER 65532:65532
ENTRYPOINT ["/kwak-registrar"]
