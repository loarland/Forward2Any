# ---- 构建 ----
# --platform=$BUILDPLATFORM：构建阶段跑在**构建机自己的架构**上，用 GOARCH=$TARGETARCH 交叉编译。
# 多架构（amd64/arm64）时不用 QEMU 去模拟整个 Go 工具链，快得多；
# 纯 Go 的 SQLite 驱动 + CGO_ENABLED=0，交叉编译没有 cgo 那些坑。
# 普通 docker build 时 TARGETOS/TARGETARCH 就是本机架构，行为跟以前一样。
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS
ARG TARGETARCH
# 版本号由 CI 传进来（打标签时是 1.0.1，main 上是 sha-xxxxxxxx）；
# 本地 docker build 不传就是 dev。`f2a version` 和启动日志都用它。
ARG VERSION=dev
WORKDIR /src

# 先只拷依赖描述文件，这一层能吃到缓存
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# CGO_ENABLED=0：SQLite 用的是纯 Go 实现，所以能静态编译进 distroless
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=$VERSION" -o /out/f2a ./cmd/f2a \
    && mkdir -p /out/data

# ---- 运行 ----
# distroless static 自带 CA 证书（出站 HTTPS 要用），且没有 shell —— 攻击面小
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/f2a /f2a
# 分发的镜像里带上许可证原文（Apache-2.0 第 4 条要求随分发附带）
COPY --from=build /src/LICENSE /LICENSE
# 提前把 /data 建出来并交给 nonroot，这样命名卷会继承属主、非 root 也能写
COPY --from=build --chown=nonroot:nonroot /out/data /data

VOLUME /data
EXPOSE 16000
ENV F2A_DATA_DIR=/data \
    F2A_PORT=16000

# 镜像里没有 curl，只能让二进制自己探自己；
# 不带 --url 时它会读数据库里的实际端口，改过端口也不会误报。
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD ["/f2a", "healthcheck"]

USER nonroot
ENTRYPOINT ["/f2a"]
