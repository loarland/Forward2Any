# ---- 构建 ----
FROM golang:1.27-alpine AS build
WORKDIR /src

# 先只拷依赖描述文件，这一层能吃到缓存
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# CGO_ENABLED=0：SQLite 用的是纯 Go 实现，所以能静态编译进 distroless
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/f2a ./cmd/f2a \
    && mkdir -p /out/data

# ---- 运行 ----
# distroless static 自带 CA 证书（出站 HTTPS 要用），且没有 shell —— 攻击面小
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/f2a /f2a
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
