# ── 构建阶段 ──────────────────────────────────────────────
FROM golang:1.22-alpine AS build

WORKDIR /src

# 先只拷依赖清单：依赖不变时这一层能命中缓存，改代码不用重新下载模块
COPY go.mod go.sum ./
RUN go env -w GOPROXY=https://goproxy.cn,direct \
 && go mod download

COPY . .

# CGO_ENABLED=0 —— 纯静态二进制，才能放进 distroless/scratch 跑
# -s -w 去掉符号表和调试信息，镜像小一半
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w -X main.buildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    -o /out/gateway ./cmd/gateway

# ── 运行阶段 ──────────────────────────────────────────────
# distroless：没有 shell、没有包管理器、没有 curl。
# 公网部署的最小攻击面 —— 打进来了也没有工具可用。
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/gateway /usr/local/bin/gateway

USER nonroot:nonroot
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/gateway"]
