# syntax=docker/dockerfile:1

# ---- 构建阶段 ----
FROM golang:1.23-alpine AS build

WORKDIR /src

# 先只拷依赖描述，利用层缓存（本项目零外部依赖，这层很快就过）
COPY go.mod ./
RUN go mod download

# 再拷源码构建
COPY cmd ./cmd
COPY internal ./internal

# 版本号注入（发布构建由 CI 传 --build-arg VERSION=2026.10.19.1430）
# TARGETOS/TARGETARCH 由 buildx 注入：多架构构建时按目标平台编译，缺省回落本机架构。
ARG VERSION=dev
ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
      -trimpath \
      -ldflags "-s -w -X main.appVersion=${VERSION}" \
      -o /out/loomy2api ./cmd/server

# ---- 运行阶段 ----
FROM alpine:3.20

# ca-certificates：访问上游 HTTPS 必需；tzdata：硬冷却算「次日 04:00」要用本地时区
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -u 10001 loomy \
 && mkdir -p /data /app \
 && chown -R loomy:loomy /data /app

WORKDIR /app
COPY --from=build /out/loomy2api /app/loomy2api

# 配置与状态都落在 /data（挂卷持久化）：/data/config.json
# auth_dir / state_file 的相对路径会按 config.json 所在目录解析
ENV TZ=Asia/Shanghai

USER loomy
EXPOSE 17890
VOLUME ["/data"]

# 健康检查用 /healthz：无可用账号时返回 503，容器会被标记 unhealthy
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -qO- http://127.0.0.1:17890/healthz >/dev/null 2>&1 || exit 1

ENTRYPOINT ["/app/loomy2api"]
CMD ["-config", "/data/config.json"]
