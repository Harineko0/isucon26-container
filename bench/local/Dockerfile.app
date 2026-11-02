# ISUReal の Go アプリ (app/webapp/go) を本番と同じ配置 (/home/isucon/webapp) で動かす。
# ビルドコンテキストはリポジトリのルート (run.sh が指定する。-r で別の commit の木も渡せる)。
FROM golang:1.26 AS build
WORKDIR /src
COPY app/webapp/go/go.mod app/webapp/go/go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY app/webapp/go/ ./
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -o /out/isureal ./cmd/isureal

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends postgresql-client iproute2 ca-certificates curl util-linux \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -m -u 1001 isucon
COPY --chown=isucon:isucon app/webapp/sql /home/isucon/webapp/sql
COPY --chown=isucon:isucon app/webapp/public /home/isucon/webapp/public
COPY --from=build --chown=isucon:isucon /out/isureal /home/isucon/webapp/go/isureal
COPY --from=entrypoint app-entrypoint.sh /usr/local/bin/app-entrypoint.sh
RUN chmod +x /usr/local/bin/app-entrypoint.sh /home/isucon/webapp/sql/*.sh \
    && mkdir -p /home/isucon/imgstore /home/isucon/imgcache && chown -R isucon:isucon /home/isucon
WORKDIR /home/isucon/webapp/go
ENTRYPOINT ["/usr/local/bin/app-entrypoint.sh"]
