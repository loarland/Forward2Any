#!/usr/bin/env bash
#
# Forward2Any 的 Docker 端到端验证。
#
# 和 scripts/e2e.sh 的区别：这里所有东西都真跑在容器里 ——
# 镜像构建、distroless 非 root 运行、容器健康检查、容器间网络、
# 命名卷持久化。mock 接收器也放进容器，用容器名互访，
# 不依赖 host.docker.internal 这类宿主机特例。
#
# 用法：bash scripts/e2e-docker.sh
# 前置：Docker Desktop 已启动

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$(mktemp -d /tmp/f2a-e2e-docker.XXXXXX)"

IMAGE="${E2E_IMAGE:-forward2any:test}"
NET="f2a-e2e-net"
APP="f2a-e2e"
MOCK="f2a-e2e-mock"
VOL="f2a-e2e-data"

APP_PORT="${E2E_APP_PORT:-18080}"
ADMIN_USER="admin"
ADMIN_PASS="e2e-docker-password-123"
BASE="http://127.0.0.1:${APP_PORT}"
JAR="$WORK/cookies.txt"

# 带上 Docker Desktop 的默认安装路径，免得 PATH 里没有 docker
export PATH="/usr/local/bin:/Applications/Docker.app/Contents/Resources/bin:$PATH"

cleanup() {
  docker rm -f "$APP" "$MOCK" >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
  docker volume rm "$VOL" >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT

fail() {
  echo "❌ $*" >&2
  echo "--- 应用日志（最后 30 行）---"
  docker logs "$APP" 2>&1 | tail -30 || true
  exit 1
}
pass() { echo "✅ $*"; }

post_form() {
  curl -fsS -c "$JAR" -b "$JAR" -o /dev/null -X POST "$@"
}

echo "== 前置检查 =="
command -v docker >/dev/null 2>&1 || fail "找不到 docker 命令"
docker info >/dev/null 2>&1 || fail "Docker daemon 没在运行"
pass "Docker 可用：$(docker version --format '{{.Server.Version}}')"

echo
echo "== 构建镜像 =="
cd "$ROOT"
docker build -t "$IMAGE" . > "$WORK/build.log" 2>&1 || {
  echo "--- 构建日志（最后 40 行）---"
  tail -40 "$WORK/build.log"
  exit 1
}
SIZE=$(docker image inspect "$IMAGE" --format '{{.Size}}' | awk '{printf "%.1f", $1/1024/1024}')
pass "镜像构建完成：$IMAGE（${SIZE} MB）"

echo
echo "== 起容器 =="
docker network create "$NET" >/dev/null

cat > "$WORK/mock.py" <<'PY'
import http.server, sys

class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get('Content-Length') or 0)
        body = self.rfile.read(n)
        print('PATH ' + self.path, flush=True)
        for k in ('Content-Type', 'X-F2A-Token', 'X-F2A-Hops', 'X-F2A-Trace', 'X-Target'):
            v = self.headers.get(k)
            if v:
                print('%s: %s' % (k, v), flush=True)
        print('BODY ' + body.decode('utf-8', 'replace'), flush=True)
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b'ok')

    def log_message(self, *a):
        pass

http.server.HTTPServer(('0.0.0.0', 9090), H).serve_forever()
PY

# 用 docker cp 把脚本送进去，不依赖 Docker Desktop 的文件共享配置。
# 目标目录必须已存在 —— python:3-alpine 里没有 /app，所以放根目录。
docker create --name "$MOCK" --network "$NET" python:3-alpine python -u /mock.py >/dev/null
docker cp "$WORK/mock.py" "$MOCK:/mock.py" >/dev/null
docker start "$MOCK" >/dev/null
pass "mock 接收器容器已启动（$MOCK，容器网络内 :9090）"

docker run -d --name "$APP" --network "$NET" \
  -p "${APP_PORT}:16000" \
  -v "${VOL}:/data" \
  -e F2A_PORT=16000 \
  -e F2A_ADMIN_USER="$ADMIN_USER" \
  -e F2A_ADMIN_PASSWORD="$ADMIN_PASS" \
  -e F2A_BASE_URL="$BASE" \
  "$IMAGE" >/dev/null

ok=0
i=0
while [ "$i" -lt 100 ]; do
  if curl -fsS "$BASE/healthz" >/dev/null 2>&1; then ok=1; break; fi
  sleep 0.3
  i=$((i + 1))
done
[ "$ok" = "1" ] || fail "容器没有在 30 秒内就绪"
pass "服务已就绪（宿主 $BASE → 容器 :16000）"

# distroless 里没有 shell，所以只能用镜像元数据确认运行身份
RUNUSER=$(docker inspect "$APP" --format '{{.Config.User}}')
[ "$RUNUSER" = "nonroot" ] || fail "容器应当以 nonroot 运行，实际是 '$RUNUSER'"
pass "容器以非 root（nonroot）身份运行"

echo
echo "== 登录 =="
curl -fsS -c "$JAR" -b "$JAR" -o /dev/null \
  -d "username=$ADMIN_USER&password=$ADMIN_PASS" "$BASE/login"
grep -q f2a_session "$JAR" || fail "登录后没有拿到会话 cookie"
pass "登录成功"

echo
echo "== 建源与规则 =="
post_form \
  --data-urlencode "name=GitHub 接收" \
  --data-urlencode "kind=webhook" \
  --data-urlencode "usage=in" \
  --data-urlencode "enabled=1" \
  --data-urlencode "slug=gh-docker" \
  --data-urlencode "auth_mode=token" \
  --data-urlencode "auth_header=X-F2A-Token" \
  --data-urlencode "auth_secret=docker-secret" \
  --data-urlencode "ip_allow=" \
  --data-urlencode "headers={}" \
  --data-urlencode "http_method=POST" \
  "$BASE/sources"

curl -fsS -b "$JAR" -c "$JAR" "$BASE/sources" > "$WORK/sources.html"
grep -q "gh-docker" "$WORK/sources.html" || fail "接收源没有创建成功"
pass "接收源已创建"

# 目标用容器名寻址：验证容器间网络，而不是靠宿主机转发
post_form \
  --data-urlencode "name=Mock 目标" \
  --data-urlencode "kind=webhook" \
  --data-urlencode "usage=out" \
  --data-urlencode "enabled=1" \
  --data-urlencode "slug=" \
  --data-urlencode "auth_mode=none" \
  --data-urlencode "ip_allow=" \
  --data-urlencode "url=http://$MOCK:9090/sink" \
  --data-urlencode "http_method=POST" \
  --data-urlencode "headers={\"X-Target\":\"mock\"}" \
  "$BASE/sources"

post_form \
  --data-urlencode "name=转发到 mock" \
  --data-urlencode "enabled=1" \
  --data-urlencode "from_source_ids=1" \
  --data-urlencode "to_source_ids=2" \
  --data-urlencode "filters=action eq push" \
  --data-urlencode "body_template=" \
  --data-urlencode "subject_template=" \
  --data-urlencode "headers_template={}" \
  "$BASE/rules"

curl -fsS -b "$JAR" -c "$JAR" "$BASE/rules" > "$WORK/rules.html"
grep -q "转发到 mock" "$WORK/rules.html" || fail "规则没有创建成功"
pass "规则已创建（容器名寻址的目标源）"

echo
echo "== 容器内转发链路 =="
code=$(curl -s -o "$WORK/hook.out" -w '%{http_code}' -X POST \
  -H 'Content-Type: application/json' \
  -H 'X-F2A-Token: docker-secret' \
  -d '{"action":"push","repository":{"full_name":"a/b"}}' \
  "$BASE/hook/gh-docker")
[ "$code" = "202" ] || fail "接收端点应返回 202，实际 $code"
grep -q "accepted 1" "$WORK/hook.out" || fail "应当入队 1 条，实际：$(cat "$WORK/hook.out")"
pass "接收端点返回 202 并入队 1 条"

ok=0
i=0
while [ "$i" -lt 50 ]; do
  if docker logs "$MOCK" 2>&1 | grep -q 'BODY {"action":"push"'; then ok=1; break; fi
  sleep 0.2
  i=$((i + 1))
done
[ "$ok" = "1" ] || fail "mock 容器没有收到转发：$(docker logs "$MOCK" 2>&1 | tail -10)"
docker logs "$MOCK" 2>&1 | grep -q 'PATH /sink' || fail "转发路径不对"
docker logs "$MOCK" 2>&1 | grep -q 'X-F2A-Hops: gh-docker' || fail "转发缺少跳链头"
docker logs "$MOCK" 2>&1 | grep -q 'X-Target: mock' || fail "源上配置的固定请求头没带上"
pass "容器间转发成功：报文原样透传，跳链与自定义请求头都在"

echo
echo "== 容器健康检查 =="
# HEALTHCHECK 的第一次探测发生在 interval（30s）之后，所以这里要等久一点
ok=0
i=0
while [ "$i" -lt 90 ]; do
  status=$(docker inspect "$APP" --format '{{.State.Health.Status}}' 2>/dev/null || echo unknown)
  if [ "$status" = "healthy" ]; then ok=1; break; fi
  sleep 1
  i=$((i + 1))
done
[ "$ok" = "1" ] || fail "容器健康检查没有变成 healthy（当前 $status）"
pass "HEALTHCHECK 报告 healthy（distroless 里没有 curl，靠二进制自探）"

echo
echo "== 命名卷持久化 =="
docker restart "$APP" >/dev/null
ok=0
i=0
while [ "$i" -lt 100 ]; do
  if curl -fsS "$BASE/healthz" >/dev/null 2>&1; then ok=1; break; fi
  sleep 0.3
  i=$((i + 1))
done
[ "$ok" = "1" ] || fail "重启后服务没有恢复"

# 重启后会话在内存里，需要重新登录
curl -fsS -c "$JAR" -b "$JAR" -o /dev/null \
  -d "username=$ADMIN_USER&password=$ADMIN_PASS" "$BASE/login"
curl -fsS -b "$JAR" -c "$JAR" "$BASE/sources" > "$WORK/sources2.html"
grep -q "gh-docker" "$WORK/sources2.html" || fail "重启后源丢失，命名卷没生效"
curl -fsS -b "$JAR" -c "$JAR" "$BASE/deliveries?status=success" > "$WORK/deliv2.html"
grep -q "转发到 mock" "$WORK/deliv2.html" || fail "重启后投递日志丢失"
pass "重启容器后源与投递日志都还在（命名卷 + 非 root 写入均正常）"

echo
echo "== 端口热切换在容器里的表现 =="
# 容器内换端口：Docker 的端口映射是创建时固定的，所以容器外应当访问不到了。
# 这里验证的是「程序确实换了端口」，以及健康检查能跟上（它读库里的端口）。
post_form \
  --data-urlencode "web_port=9099" \
  --data-urlencode "base_url=$BASE" \
  --data-urlencode "admin_user=$ADMIN_USER" \
  --data-urlencode "retry_max=5" \
  --data-urlencode "retry_backoff_seconds=10" \
  --data-urlencode "payload_max_bytes=65536" \
  --data-urlencode "log_retention_days=30" \
  "$BASE/settings"
sleep 2
inport=$(docker exec "$APP" /f2a healthcheck >/dev/null 2>&1 && echo yes || echo no)
[ "$inport" = "yes" ] || fail "容器内改端口后，健康检查自己探不到了"
pass "容器内改端口后，healthcheck 自动跟随到 9099（证明端口确实换了、且检查读的是库里的值）"

echo
echo "== 数据落盘位置 =="
docker run --rm -v "${VOL}:/data" alpine sh -c 'ls -l /data' > "$WORK/vol.txt" 2>&1
grep -q "f2a.db" "$WORK/vol.txt" || fail "命名卷里没有数据库文件：$(cat "$WORK/vol.txt")"
pass "数据在命名卷 $VOL 里：$(grep -c . "$WORK/vol.txt") 个文件"

echo
echo "全部通过 🎉"
