# The sessionhub server in a container. Clients (hooks, MCP, the herdr plugin)
# run on your machines, not here. See docs/self-hosting.md, "Run the server
# with Docker".
FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/sessionhub ./cmd/sessionhub \
 && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/sessionhub /usr/local/bin/sessionhub
# The database and move bundles live in /data, owned by the nonroot user.
COPY --from=build --chown=nonroot:nonroot /out/data /data
ENV SESSIONHUB_LISTEN=0.0.0.0:8787 \
    SESSIONHUB_DB=/data/sessionhub.db \
    SESSIONHUB_SERVER_CONFIG=/data/server.toml
VOLUME /data
EXPOSE 8787
ENTRYPOINT ["/usr/local/bin/sessionhub"]
CMD ["server"]
