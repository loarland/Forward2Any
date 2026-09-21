#!/usr/bin/env bash
#
# Forward2Any 端到端验证。
#
# 起一个 mock 接收器 + 一个真实服务实例，把「接收 -> 匹配规则 -> 转发 -> 落库」
# 整条链路真跑一遍，顺带验证鉴权、循环拦截、过滤器和各页面的渲染。
#
# 用法：bash scripts/e2e.sh
# 依赖：go、curl、python3

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$(mktemp -d)"

APP_PORT="${E2E_APP_PORT:-18080}"
MOCK_PORT="${E2E_MOCK_PORT:-19090}"
PROXY_PORT="${E2E_PROXY_PORT:-19091}"
ADMIN_USER="admin"
ADMIN_PASS="e2e-password-123"
BASE="http://127.0.0.1:${APP_PORT}"
JAR="$WORK/cookies.txt"
RECEIVED="$WORK/received.txt"
PROXY_LOG="$WORK/proxy.log"

PIDS=()

cleanup() {
  if [ "${#PIDS[@]}" -gt 0 ]; then
    for pid in "${PIDS[@]}"; do
      kill "$pid" 2>/dev/null || true
      wait "$pid" 2>/dev/null || true
    done
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT

fail() { echo "❌ $*" >&2; [ -f "$WORK/app.log" ] && { echo "--- 服务日志 ---"; tail -30 "$WORK/app.log"; }; exit 1; }
pass() { echo "✅ $*"; }

# 表单提交：带上会话 cookie，结果靠调用方自己去页面里核实。
post_form() {
  curl -fsS -c "$JAR" -b "$JAR" -o /dev/null -X POST "$@"
}

# 等待条件成立，最多 10 秒。
wait_for() {
  local i=0
  while [ "$i" -lt 50 ]; do
    if eval "$1" >/dev/null 2>&1; then return 0; fi
    sleep 0.2
    i=$((i + 1))
  done
  return 1
}

echo "== 准备 =="
cd "$ROOT"

cat > "$WORK/mock.py" <<'PY'
import http.server, sys
OUT = sys.argv[2]

class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get('Content-Length') or 0)
        body = self.rfile.read(n)
        with open(OUT, 'ab') as f:
            f.write(b'PATH ' + self.path.encode() + b'\n')
            for k in ('Content-Type', 'X-F2A-Token', 'X-F2A-Hops', 'X-F2A-Trace', 'X-Target'):
                v = self.headers.get(k)
                if v:
                    f.write(('%s: %s\n' % (k, v)).encode())
            f.write(b'BODY ' + body + b'\n---\n')
        # 路径里带 /bot 的当成 Telegram Bot API：回它那套信封（成不成看 ok），
        # 好让端到端真的走一遍「解析响应、判定成功」这段。
        if '/bot' in self.path:
            resp = b'{"ok":true,"result":{"message_id":1,"chat":{"id":-1001234567890}}}'
            self.send_response(200)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(resp)))
            self.end_headers()
            self.wfile.write(resp)
            return
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b'ok')

    def log_message(self, *a):
        pass

http.server.HTTPServer(('127.0.0.1', int(sys.argv[1])), H).serve_forever()
PY

python3 "$WORK/mock.py" "$MOCK_PORT" "$RECEIVED" &
PIDS+=("$!")
pass "mock 接收器已启动（端口 $MOCK_PORT）"

go build -o "$WORK/f2a" ./cmd/f2a

F2A_DATA_DIR="$WORK/data" \
F2A_PORT="$APP_PORT" \
F2A_ADMIN_USER="$ADMIN_USER" \
F2A_ADMIN_PASSWORD="$ADMIN_PASS" \
F2A_BASE_URL="$BASE" \
  "$WORK/f2a" > "$WORK/app.log" 2>&1 &
PIDS+=("$!")

wait_for "curl -fsS $BASE/healthz" || fail "服务未在 10 秒内就绪"
pass "服务已就绪（端口 $APP_PORT）"

echo
echo "== 登录 =="
curl -fsS -c "$JAR" -b "$JAR" -o /dev/null \
  -d "username=$ADMIN_USER&password=$ADMIN_PASS" "$BASE/login"
grep -q f2a_session "$JAR" || fail "登录后没有拿到会话 cookie"
pass "登录成功"

# 错误密码必须被拒（页面会返回 200 并显示错误，所以看内容而不是状态码）
curl -s -o "$WORK/badlogin.html" -d "username=$ADMIN_USER&password=wrong" "$BASE/login"
grep -q "用户名或密码错误" "$WORK/badlogin.html" || fail "错误密码竟然登录成功了"
pass "错误密码被拒绝"

echo
echo "== 建源与规则 =="
# 全新的临时数据库，自增 id 从 1 开始：1 号是接收源，2 号是目标源。
post_form \
  --data-urlencode "name=GitHub 接收" \
  --data-urlencode "kind=webhook" \
  --data-urlencode "usage=in" \
  --data-urlencode "enabled=1" \
  --data-urlencode "slug=gh-e2e" \
  --data-urlencode "auth_mode=token" \
  --data-urlencode "auth_header=X-F2A-Token" \
  --data-urlencode "auth_secret=e2e-secret" \
  --data-urlencode "ip_allow=" \
  --data-urlencode "headers={}" \
  --data-urlencode "http_method=POST" \
  "$BASE/sources"

curl -fsS -b "$JAR" -c "$JAR" "$BASE/sources" > "$WORK/sources.html"
grep -q "gh-e2e" "$WORK/sources.html" || fail "接收源没有创建成功"
grep -q "$BASE/hook/gh-e2e" "$WORK/sources.html" || fail "页面没有显示回调地址"
pass "接收源已创建，回调地址已生成"

post_form \
  --data-urlencode "name=Mock 目标" \
  --data-urlencode "kind=webhook" \
  --data-urlencode "usage=out" \
  --data-urlencode "enabled=1" \
  --data-urlencode "slug=" \
  --data-urlencode "auth_mode=none" \
  --data-urlencode "ip_allow=" \
  --data-urlencode "url=http://127.0.0.1:$MOCK_PORT/sink" \
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
pass "规则已创建（接收源 1 → 目标源 2，过滤 action eq push）"

echo
echo "== 接收与转发 =="
code=$(curl -s -o "$WORK/hook.out" -w '%{http_code}' -X POST \
  -H 'Content-Type: application/json' \
  -H 'X-F2A-Token: e2e-secret' \
  -d '{"action":"push","repository":{"full_name":"a/b"}}' \
  "$BASE/hook/gh-e2e")
[ "$code" = "202" ] || fail "接收端点应返回 202，实际 $code（响应：$(cat "$WORK/hook.out")）"
grep -q "accepted 1" "$WORK/hook.out" || fail "应当入队 1 条投递，实际：$(cat "$WORK/hook.out")"
pass "接收端点返回 202 并入队 1 条"

wait_for "test -s $RECEIVED" || fail "mock 没有收到转发"
grep -q 'PATH /sink' "$RECEIVED" || fail "转发路径不对：$(cat "$RECEIVED")"
grep -q 'BODY {"action":"push","repository":{"full_name":"a/b"}}' "$RECEIVED" \
  || fail "转发内容不是原样透传：$(cat "$RECEIVED")"
grep -q 'X-F2A-Hops: gh-e2e' "$RECEIVED" || fail "转发请求缺少跳链头"
grep -q 'X-Target: mock' "$RECEIVED" || fail "源上配置的固定请求头没有带上"
pass "mock 收到了原样透传的报文，跳链与自定义请求头都在"

echo
echo "== 鉴权与边界 =="
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H 'X-F2A-Token: wrong' -d '{"action":"push"}' "$BASE/hook/gh-e2e")
[ "$code" = "401" ] || fail "错误密钥应返回 401，实际 $code"
pass "错误密钥被拒绝（401）"

code=$(curl -s -o "$WORK/loop.out" -w '%{http_code}' -X POST \
  -H 'X-F2A-Token: e2e-secret' \
  -H 'X-F2A-Hops: other,gh-e2e' \
  -d '{"action":"push"}' "$BASE/hook/gh-e2e")
[ "$code" = "200" ] || fail "循环请求应返回 200，实际 $code"
grep -q "loop dropped" "$WORK/loop.out" || fail "循环请求没有被拦截"
pass "循环转发被拦截"

out=$(curl -s -X POST -H 'X-F2A-Token: e2e-secret' \
  -d '{"action":"pull"}' "$BASE/hook/gh-e2e")
grep -q "accepted 0" <<<"$out" || fail "不匹配过滤条件的请求不应入队，实际：$out"
pass "过滤条件未命中时不转发"

code=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H 'X-F2A-Token: e2e-secret' -d '{}' "$BASE/hook/does-not-exist")
[ "$code" = "404" ] || fail "不存在的源应返回 404，实际 $code"
pass "未知路径返回 404"

echo
echo "== 结果落库与页面渲染 =="
curl -fsS -b "$JAR" -c "$JAR" "$BASE/deliveries?status=success" > "$WORK/deliveries.html"
grep -q "转发到 mock" "$WORK/deliveries.html" || fail "投递日志里没有成功记录"
grep -q "成功" "$WORK/deliveries.html" || fail "投递日志状态列没渲染出来"
pass "投递日志记录了成功投递"

for page in / /sources /rules /deliveries /settings /deliveries/1; do
  code=$(curl -s -o "$WORK/page.html" -w '%{http_code}' -b "$JAR" "$BASE$page")
  [ "$code" = "200" ] || fail "页面 $page 返回 $code"
  grep -qi "template" "$WORK/page.html" && grep -qi "error" "$WORK/page.html" \
    && fail "页面 $page 渲染出错：$(head -c 300 "$WORK/page.html")"
done
pass "概览 / 源 / 规则 / 投递日志 / 详情 / 设置 全部正常渲染"

# 外观（配色 + 亮暗）：三个环节都要对上 —— 设置页能选、服务端渲染的 <link> 和
# <html data-theme> 跟着变、值非法时不能被拼进 href。
curl -fsS -b "$JAR" "$BASE/settings" > "$WORK/theme-settings.html"
grep -q 'name="theme_color"' "$WORK/theme-settings.html" || fail "设置页没有配色下拉框"
grep -q 'name="theme_mode"' "$WORK/theme-settings.html" || fail "设置页没有亮暗下拉框"
theme_opts=$(grep -o 'name="theme_color"' -A 40 "$WORK/theme-settings.html" \
  | grep -c 'value="[a-z]*"')
[ "$theme_opts" -ge 20 ] || fail "配色选项只有 $theme_opts 个，像是没生成"
pass "设置页能选配色（$theme_opts 种）与亮暗"

# 登录页不需要会话，拿它当「服务端渲染结果」的观察窗
curl -fsS "$BASE/login" > "$WORK/theme-before.html"
grep -q "/static/palettes/blue.css" "$WORK/theme-before.html" || fail "默认配色应当是 blue"
grep -q "data-theme=" "$WORK/theme-before.html" && fail "auto 档不该给 <html> 写 data-theme"
pass "默认外观：blue 配色 + 跟随系统"

post_form \
  --data-urlencode "web_port=$APP_PORT" \
  --data-urlencode "base_url=$BASE" \
  --data-urlencode "admin_user=$ADMIN_USER" \
  --data-urlencode "retry_max=5" \
  --data-urlencode "retry_backoff_seconds=10" \
  --data-urlencode "payload_max_bytes=65536" \
  --data-urlencode "log_retention_days=30" \
  --data-urlencode "theme_color=jade" \
  --data-urlencode "theme_mode=dark" \
  "$BASE/settings"

curl -fsS "$BASE/login" > "$WORK/theme-after.html"
grep -q "/static/palettes/jade.css" "$WORK/theme-after.html" || fail "换配色后 <link> 没跟着变"
grep -q 'data-theme="dark"' "$WORK/theme-after.html" || fail "换暗色后 <html> 没写 data-theme"
pass "换配色与亮暗后，服务端渲染立刻跟着变（不用重启）"

code=$(curl -s -o "$WORK/palette.css" -w '%{http_code}' "$BASE/static/palettes/jade.css")
[ "$code" = "200" ] || fail "配色文件取不到：$code"
grep -q -- "--pico-primary" "$WORK/palette.css" || fail "配色文件里没有主色变量"
pass "配色文件能正常取到，且含主色变量"

# 非法值必须被挡下：它要进 <link href>，不能回显任意字符串
post_form \
  --data-urlencode "web_port=$APP_PORT" \
  --data-urlencode "base_url=$BASE" \
  --data-urlencode "admin_user=$ADMIN_USER" \
  --data-urlencode "theme_color=../../etc/passwd" \
  --data-urlencode "theme_mode=banana" \
  "$BASE/settings"
curl -fsS "$BASE/login" > "$WORK/theme-bad.html"
grep -q "/static/palettes/jade.css" "$WORK/theme-bad.html" || fail "非法配色应当被忽略、保持原值"
grep -q "etc/passwd" "$WORK/theme-bad.html" && fail "非法配色被拼进了页面"
grep -q 'data-theme="dark"' "$WORK/theme-bad.html" || fail "非法亮暗值应当被忽略、保持原值"
pass "非法配色/亮暗值被挡下，原值不变"

# 规则表单的过滤条件编辑器：可视化那半是 JS 铺的，但脚手架和操作符表必须在 HTML 里，
# 不然 JS 一挂（或没加载）就没得选了。
curl -fsS -b "$JAR" -c "$JAR" "$BASE/rules/1/edit" > "$WORK/rule_edit.html"
grep -q 'data-filter-editor' "$WORK/rule_edit.html" || fail "规则表单缺少过滤条件编辑器容器"
grep -q 'data-filter-row-tpl' "$WORK/rule_edit.html" || fail "规则表单缺少条件行模板"
grep -q 'name="filters"' "$WORK/rule_edit.html" || fail "过滤条件没有可提交的字段"
for op in eq ne gt lt contains not_contains exists not_exists regex in; do
  grep -q "value=\"$op\"" "$WORK/rule_edit.html" || fail "操作符下拉里缺少 $op"
done
# 已保存的条件要回填到文本框里（可视化行由 JS 从这段文本铺出来）
grep -q 'action eq push' "$WORK/rule_edit.html" || fail "过滤条件没有回填到表单"
pass "规则表单的过滤条件编辑器渲染正常，操作符齐全"

# 校验失败时整张表单要回填，不能只留一个错误提示。
curl -s -b "$JAR" -c "$JAR" -X POST \
  --data-urlencode "name=回填测试" \
  --data-urlencode "enabled=1" \
  --data-urlencode "from_source_ids=1" \
  --data-urlencode "to_source_ids=2" \
  --data-urlencode "filters=action eq push
这一行是坏的" \
  --data-urlencode "body_template={{.Payload.action}}" \
  --data-urlencode "subject_template=主题" \
  --data-urlencode "headers_template={}" \
  "$BASE/rules" > "$WORK/rule_err.html"
grep -q "过滤器第 2 行" "$WORK/rule_err.html" || fail "过滤条件语法错误没有报出行号"
grep -q 'value="回填测试"' "$WORK/rule_err.html" || fail "校验失败后规则名称被清空了"
grep -q 'name="from_source_ids" value="1" checked' "$WORK/rule_err.html" || fail "校验失败后接收源选择被清空了"
grep -q 'name="to_source_ids" value="2" checked' "$WORK/rule_err.html" || fail "校验失败后目标源选择被清空了"
grep -q '{{.Payload.action}}' "$WORK/rule_err.html" || fail "校验失败后报文体模板被清空了"
grep -q '这一行是坏的' "$WORK/rule_err.html" || fail "校验失败后用户写的过滤文本被丢掉了"
pass "规则表单校验失败后会整张回填"

# 「选源」两栏：只列用途对得上的源，并且整栏不能套在一个大 <label> 里 ——
# 按 HTML 规则一个 label 只认它里面第一个控件，那样点栏目标题、点列表空白都会去勾第一项
# （这个误勾在 Chrome 里实测到过，所以这里拿嵌套层数兜住它）。
python3 - "$WORK/rule_edit.html" <<'PICKER'
import re
import sys

html = open(sys.argv[1]).read()
bad = []
start, mid = html.find('data-picker="from"'), html.find('data-picker="to"')
if start < 0 or mid < start:
    bad.append('页面里没有找到两栏选源列表')
    frm = to = ''
else:
    frm, to = html[start:mid], html[mid:]


def row(name):
    """行定位串。带结尾空格是为了只匹配这一行的 data-search，而不是别处的源名。"""
    return 'data-search="%s ' % name


# 这个实例里「GitHub 接收」只接收、「Mock 目标」只发送
if row('GitHub 接收') not in frm:
    bad.append('接收源栏没列出「GitHub 接收」')
if row('Mock 目标') in frm:
    bad.append('只发送的源出现在接收源栏里')
if row('Mock 目标') not in to:
    bad.append('目标源栏没列出「Mock 目标」')
if row('GitHub 接收') in to:
    bad.append('只接收的源出现在目标源栏里')
# 被藏起来的源要说一声，不然用户只会觉得「我的源怎么没了」
if '另有 1 个源用途不含接收' not in frm:
    bad.append('接收源栏没说明有源因为用途不符没列出来')
if '另有 1 个源用途不含发送' not in to:
    bad.append('目标源栏没说明有源因为用途不符没列出来')

for col in (frm, to):
    for r in re.findall(r'(?s)<label class="pick".*?</label>', col):
        if r.count('<input') != 1:
            bad.append('一行里包了 %d 个控件：%s' % (r.count('<input'), r[:60]))
        if '<label' in r[len('<label'):]:
            bad.append('选源行里出现了嵌套 label')

depth = mx = 0
for m in re.findall(r'<label\b|</label>', html):
    depth += 1 if m == '<label' else -1
    mx = max(mx, depth)
if mx > 1:
    bad.append('规则表单里有嵌套的 label（最深 %d 层）' % mx)

if bad:
    print('\n'.join('  - ' + b for b in bad), file=sys.stderr)
    sys.exit(1)
PICKER
pass "规则表单的选源两栏按用途过滤，且没有嵌套 label"

# 列表页的筛选条：源和规则是客户端筛选（列表已在页面里），投递日志是服务端筛选。
curl -fsS -b "$JAR" -c "$JAR" "$BASE/sources" > "$WORK/sources_list.html"
curl -fsS -b "$JAR" -c "$JAR" "$BASE/rules" > "$WORK/rules_list.html"
for f in sources_list rules_list; do
  grep -q 'data-list-filter' "$WORK/$f.html" || fail "$f 缺少筛选条"
  grep -q 'data-lf-search' "$WORK/$f.html" || fail "$f 缺少搜索框"
  grep -q 'data-row' "$WORK/$f.html" || fail "$f 的行没有打上 data-row 标记"
  grep -q 'data-lf-count' "$WORK/$f.html" || fail "$f 缺少结果计数"
done
grep -q 'data-kind="webhook"' "$WORK/sources_list.html" || fail "源的行上没有类型属性，筛选没法生效"
grep -q 'data-enabled="1"' "$WORK/rules_list.html" || fail "规则的行上没有状态属性，筛选没法生效"
pass "源 / 规则的筛选条与行标记都渲染出来了"

# 源卡片上的 curl 示例：要能一键复制（按钮指的 id 必须真的存在），
# 示例里的地址、鉴权头也要跟这个源对得上。
python3 - "$WORK/sources_list.html" <<'CURLCOPY'
import re
import sys

html = open(sys.argv[1]).read()
bad = []
box = re.search(r'(?s)<details class="code-block">.*?</details>', html)
if not box:
    bad.append('源卡片上没有 curl 示例的代码块')
else:
    box = box.group(0)
    btn = re.search(r'<button[^>]*data-copy-from="#([^"]+)"', box)
    if not btn:
        bad.append('curl 示例没有一键复制的按钮')
    elif ('id="%s"' % btn.group(1)) not in box:
        bad.append('复制按钮指向 #%s，但页面上没有这个 id' % btn.group(1))
    if not re.search(r'(?s)<summary>.*data-copy-from.*</summary>', box):
        bad.append('复制按钮不在 summary 里，收起来时点不到')
    pre = re.search(r'(?s)<pre class="code" id="curl-\d+">(.*?)</pre>', box)
    if not pre:
        bad.append('示例命令没有渲染成 pre.code')
    else:
        text = pre.group(1)
        for want in ('curl -X POST', '/hook/gh-e2e', 'X-F2A-Token: e2e-secret', 'application/json'):
            if want not in text:
                bad.append('示例命令里缺少 %s' % want)
    # 回调地址那个复制按钮是另一套写法，别在改这段时弄丢
    if 'data-copy="' not in html:
        bad.append('回调地址的复制按钮不见了')
if bad:
    print('\n'.join('  - ' + b for b in bad), file=sys.stderr)
    sys.exit(1)
CURLCOPY
pass "curl 示例带一键复制，按钮指的 id 存在且内容对得上这个源"

# 投递日志的关键字搜索走服务端（记录会一直涨、还要分页），得真查一次库。
curl -fsS -b "$JAR" -c "$JAR" --get --data-urlencode "q=mock" "$BASE/deliveries" > "$WORK/dl_hit.html"
curl -fsS -b "$JAR" -c "$JAR" --get --data-urlencode "q=根本没有这个名字" "$BASE/deliveries" > "$WORK/dl_miss.html"
grep -q "转发到 mock" "$WORK/dl_hit.html" || fail "按规则名搜索投递日志没有命中"
grep -q "筛选出" "$WORK/dl_hit.html" || fail "筛选后页头没有显示筛选状态"
grep -q "/deliveries/1" "$WORK/dl_miss.html" && fail "搜不到东西时不该还列出记录"
grep -q "没有符合条件的记录" "$WORK/dl_miss.html" || fail "搜不到时没有给出空状态提示"
# 搜索框要把关键字带回来，不然一翻页/一刷新就白搜了
grep -q 'name="q" value="mock"' "$WORK/dl_hit.html" || fail "搜索框没有回填关键字"
pass "投递日志的关键字搜索与空状态正常"

# 未登录访问后台必须被重定向到登录页
code=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/settings")
[ "$code" = "303" ] || fail "未登录访问后台应重定向，实际 $code"
pass "未登录会被挡到登录页"

# 导出配置应当是一份能解析的 JSON
curl -fsS -b "$JAR" "$BASE/settings/export" > "$WORK/export.json"
python3 -c "
import json, sys
d = json.load(open('$WORK/export.json'))
assert d['version'] == 1, d['version']
assert len(d['sources']) == 2, len(d['sources'])
assert len(d['rules']) == 1, len(d['rules'])
assert d['rules'][0]['from'] == [0], d['rules'][0]['from']
assert d['rules'][0]['to'] == [1], d['rules'][0]['to']
assert 'admin_pass_hash' not in json.dumps(d), '导出文件不应包含密码哈希'
" || fail "导出的配置内容不对"
pass "配置导出正常，且不含密码哈希"

echo
echo "== 代理 =="
# 真起一个正向代理：只有请求确实落在它上面，才算「走了代理」。
# 目标地址是明文 http，所以正向代理收到的是绝对地址（POST http://host:port/path）。
cat > "$WORK/proxy.py" <<'PY'
import http.server, sys, urllib.request

OUT = sys.argv[2]

class P(http.server.BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.1'

    def do_POST(self):
        with open(OUT, 'a') as f:
            f.write('PROXY %s\n' % self.path)
        n = int(self.headers.get('Content-Length') or 0)
        body = self.rfile.read(n)
        skip = ('host', 'content-length', 'proxy-connection', 'connection')
        req = urllib.request.Request(self.path, data=body, method='POST',
            headers={k: v for k, v in self.headers.items() if k.lower() not in skip})
        # 空 ProxyHandler：别让环境变量里的代理把这一跳又接走
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        try:
            with opener.open(req) as r:
                data, code = r.read(), r.status
        except Exception as e:
            data, code = str(e).encode(), 502
        self.send_response(code)
        self.send_header('Content-Length', str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, *a):
        pass

srv = http.server.HTTPServer(('127.0.0.1', int(sys.argv[1])), P)
with open(sys.argv[3], 'w') as f:
    f.write('ready')
srv.serve_forever()
PY

# 端口先确认是空的：不然探活会被别的进程骗过去（这个坑真踩过一次 ——
# 一个残留的 mock 占着端口，任何请求都回 200，看起来「代理起来了」）。
python3 -c "
import socket, sys
s = socket.socket()
try:
    s.bind(('127.0.0.1', $PROXY_PORT))
except OSError:
    sys.exit(1)
finally:
    s.close()
" || fail "端口 $PROXY_PORT 已被占用，换个端口：E2E_PROXY_PORT=19xxx bash scripts/e2e.sh"

python3 "$WORK/proxy.py" "$PROXY_PORT" "$PROXY_LOG" "$WORK/proxy.ready" &
PIDS+=("$!")
# 探活看的是「我们自己写的就绪标记」，不是「端口上有响应」。
wait_for "test -s $WORK/proxy.ready" || fail "正向代理没起来"
pass "正向代理已启动（端口 $PROXY_PORT）"

# 设置页能配代理，保存后要回填
curl -fsS -b "$JAR" "$BASE/settings" > "$WORK/proxy-empty.html"
grep -q 'name="proxy_type"' "$WORK/proxy-empty.html" || fail "设置页没有代理类型下拉框"
for t in none http https socks5; do
  grep -q "value=\"$t\"" "$WORK/proxy-empty.html" || fail "代理类型下拉里缺少 $t"
done
post_form \
  --data-urlencode "web_port=$APP_PORT" \
  --data-urlencode "base_url=$BASE" \
  --data-urlencode "admin_user=$ADMIN_USER" \
  --data-urlencode "proxy_type=http" \
  --data-urlencode "proxy_addr=127.0.0.1:$PROXY_PORT" \
  "$BASE/settings"
curl -fsS -b "$JAR" "$BASE/settings" > "$WORK/proxy-set.html"
grep -q 'value="127.0.0.1:'"$PROXY_PORT"'"' "$WORK/proxy-set.html" || fail "设置页没有回填已保存的代理地址"
pass "设置页能选 HTTP / HTTPS / SOCKS5，保存后回填正常"

# 一个勾了代理的发送源 + 一条规则
post_form \
  --data-urlencode "name=代理目标" \
  --data-urlencode "kind=webhook" \
  --data-urlencode "usage=out" \
  --data-urlencode "enabled=1" \
  --data-urlencode "slug=" \
  --data-urlencode "auth_mode=none" \
  --data-urlencode "ip_allow=" \
  --data-urlencode "url=http://127.0.0.1:$MOCK_PORT/proxy-sink" \
  --data-urlencode "http_method=POST" \
  --data-urlencode "headers={}" \
  --data-urlencode "use_proxy=1" \
  "$BASE/sources"

post_form \
  --data-urlencode "name=走代理转发的规则" \
  --data-urlencode "enabled=1" \
  --data-urlencode "from_source_ids=1" \
  --data-urlencode "to_source_ids=3" \
  --data-urlencode "filters=action eq proxy" \
  --data-urlencode "body_template=" \
  --data-urlencode "subject_template=" \
  --data-urlencode "headers_template={}" \
  "$BASE/rules"

curl -fsS -b "$JAR" "$BASE/sources/3/edit" > "$WORK/proxy-form.html"
tag=$(python3 -c "
import re
h = open('$WORK/proxy-form.html').read()
m = re.search(r'<input[^>]*name=\"use_proxy\"[^>]*>', h)
print(m.group(0) if m else '')
")
[ -n "$tag" ] || fail "源表单里没有代理开关"
case "$tag" in *checked*) ;; *) fail "勾了代理的源没有回填成选中：$tag" ;; esac
case "$tag" in *disabled*) fail "已经配了代理，开关不该是禁用的：$tag" ;; esac
pass "源表单的「通过代理发送」能勾能回填"

# 勾了 → 走代理（代理日志里出现绝对地址），目标也真的收到了
curl -s -o /dev/null -X POST -H 'X-F2A-Token: e2e-secret' -d '{"action":"proxy"}' "$BASE/hook/gh-e2e"
wait_for "grep -q 'PATH /proxy-sink' $RECEIVED" || fail "勾了代理的源没有把请求投出去"
grep -q "PROXY http://127.0.0.1:$MOCK_PORT/proxy-sink" "$PROXY_LOG" \
  || fail "请求没有经过代理：$(cat "$PROXY_LOG" 2>/dev/null)"
pass "勾了「通过代理发送」的源确实走了代理"

# 取消勾选 → 直连（代理日志不再增长，目标照样收到）
post_form \
  --data-urlencode "name=代理目标" \
  --data-urlencode "kind=webhook" \
  --data-urlencode "usage=out" \
  --data-urlencode "enabled=1" \
  --data-urlencode "slug=" \
  --data-urlencode "auth_mode=none" \
  --data-urlencode "ip_allow=" \
  --data-urlencode "url=http://127.0.0.1:$MOCK_PORT/proxy-sink" \
  --data-urlencode "http_method=POST" \
  --data-urlencode "headers={}" \
  "$BASE/sources/3"

before=$(grep -c PROXY "$PROXY_LOG" || true)
curl -s -o /dev/null -X POST -H 'X-F2A-Token: e2e-secret' -d '{"action":"proxy"}' "$BASE/hook/gh-e2e"
hit=0
i=0
while [ "$i" -lt 50 ]; do
  [ "$(grep -c 'PATH /proxy-sink' "$RECEIVED" || true)" -ge 2 ] && { hit=1; break; }
  sleep 0.2
  i=$((i + 1))
done
[ "$hit" = "1" ] || fail "取消勾选后直连的投递没有到达目标"
[ "$(grep -c PROXY "$PROXY_LOG" || true)" = "$before" ] || fail "取消勾选后仍然走了代理"
pass "取消勾选后改回直连"

# 用途改成「接收」→ 勾选被忽略，连投递都不会发生
post_form \
  --data-urlencode "name=代理目标" \
  --data-urlencode "kind=webhook" \
  --data-urlencode "usage=in" \
  --data-urlencode "enabled=1" \
  --data-urlencode "slug=proxy-sink-in" \
  --data-urlencode "auth_mode=none" \
  --data-urlencode "ip_allow=" \
  --data-urlencode "url=http://127.0.0.1:$MOCK_PORT/proxy-sink" \
  --data-urlencode "http_method=POST" \
  --data-urlencode "headers={}" \
  --data-urlencode "use_proxy=1" \
  "$BASE/sources/3"

before=$(grep -c PROXY "$PROXY_LOG" || true)
out=$(curl -s -X POST -H 'X-F2A-Token: e2e-secret' -d '{"action":"proxy"}' "$BASE/hook/gh-e2e")
grep -q "accepted 0" <<<"$out" || fail "用途改成接收后不该再投递，实际：$out"
[ "$(grep -c PROXY "$PROXY_LOG" || true)" = "$before" ] || fail "用途不含发送的源仍然走了代理"
pass "用途不含发送时忽略代理开关（不投递、也不走代理）"

# 非法代理地址必须被挡下，且不能改掉已存的配置
curl -s -b "$JAR" -c "$JAR" -o "$WORK/proxy-bad.html" -X POST \
  --data-urlencode "web_port=$APP_PORT" \
  --data-urlencode "base_url=$BASE" \
  --data-urlencode "admin_user=$ADMIN_USER" \
  --data-urlencode "proxy_type=socks5" \
  --data-urlencode "proxy_addr=not-an-address" \
  "$BASE/settings"
grep -q "代理地址" "$WORK/proxy-bad.html" || fail "非法代理地址没有报错"
curl -fsS -b "$JAR" "$BASE/settings" > "$WORK/proxy-after-bad.html"
grep -q 'value="127.0.0.1:'"$PROXY_PORT"'"' "$WORK/proxy-after-bad.html" \
  || fail "非法提交之后代理配置被改掉了"
pass "非法代理地址被挡下，原值不变"

echo
echo "== Telegram 发送 =="
# 先用界面把几种填错的情况挡一遍：这些错误必须在保存时就说清楚，
# 而不是等到投递失败才发现（投递日志里只剩一句 Telegram 的报错）。
# 保存失败时页面顶部会渲染一条 .alert.err；保存成功是 303 + 空响应体（没有这条）。
# 「被挡下」= 响应里带着错误条 —— 只看状态码不够，校验失败的响应也是 200。
# 调用方传的字段放在前面：表单取值取的是第一个，这样才能覆盖掉下面的默认值。
tg_reject() {
  curl -s -b "$JAR" -c "$JAR" -o "$WORK/tg-reject.html" -X POST \
    "$@" \
    --data-urlencode "name=Telegram 测试" \
    --data-urlencode "kind=telegram" \
    --data-urlencode "usage=out" \
    --data-urlencode "enabled=1" \
    --data-urlencode "tg_token=123456:e2e-token" \
    --data-urlencode "tg_chat_id=-1001234567890" \
    --data-urlencode "tg_endpoint=http://127.0.0.1:$MOCK_PORT/bot" \
    "$BASE/sources"
  grep -q 'class="alert err"' "$WORK/tg-reject.html"
}

tg_reject --data-urlencode "usage=in" || fail "Telegram 类型不该允许用作接收源"
grep -q "只能用作发送源" "$WORK/tg-reject.html" || fail "接收用途的 Telegram 源没有给出原因"
tg_reject --data-urlencode "usage=out" --data-urlencode "tg_token=" \
  || fail "缺 Bot Token 时不该保存成功"
grep -q "Bot Token" "$WORK/tg-reject.html" || fail "缺 Bot Token 时没有说明是哪个字段"
tg_reject --data-urlencode "usage=out" --data-urlencode "tg_endpoint=https://api.telegram.org" \
  || fail "端点没以 /bot 结尾时不该保存成功（token 要直接拼在它后面）"
grep -q "/bot" "$WORK/tg-reject.html" || fail "端点出错时没有给出正确形状"
tg_reject --data-urlencode "usage=out" --data-urlencode "tg_thread_id=第一话题" \
  || fail "话题 ID 不是数字时不该保存成功"
pass "Telegram 源的用途、Token、端点、话题 ID 都会在保存时校验"

# 请求端点指向 mock：这样能验证真实的请求形状（路径里带 token、body 是 Bot API 的 JSON），
# 又不用真去连 api.telegram.org。
post_form \
  --data-urlencode "name=Telegram 目标" \
  --data-urlencode "kind=telegram" \
  --data-urlencode "usage=out" \
  --data-urlencode "enabled=1" \
  --data-urlencode "tg_token=123456:e2e-token" \
  --data-urlencode "tg_chat_id=-1001234567890" \
  --data-urlencode "tg_thread_id=42" \
  --data-urlencode "tg_endpoint=http://127.0.0.1:$MOCK_PORT/bot" \
  "$BASE/sources"

curl -fsS -b "$JAR" -c "$JAR" "$BASE/sources" > "$WORK/tg-sources.html"
grep -q 'data-kind="telegram"' "$WORK/tg-sources.html" || fail "源列表里没有 Telegram 类型的行"
grep -q "Telegram 目标" "$WORK/tg-sources.html" || fail "Telegram 源没有出现在列表里"
grep -q 'value="telegram">Telegram' "$WORK/tg-sources.html" || fail "类型筛选里没有 Telegram 选项"
grep -q 'Chat ID' "$WORK/tg-sources.html" || fail "源卡片上没有显示 Chat ID"
curl -fsS -b "$JAR" "$BASE/sources/4/edit" > "$WORK/tg-form.html"
for v in 'value="123456:e2e-token"' 'value="-1001234567890"' 'value="42"' \
         'value="http://127.0.0.1:'"$MOCK_PORT"'/bot"'; do
  grep -q "$v" "$WORK/tg-form.html" || fail "Telegram 参数没有回填：$v"
done
pass "Telegram 源已创建，源卡片显示 Chat ID，编辑页能回填全部参数"

post_form \
  --data-urlencode "name=转发到 Telegram" \
  --data-urlencode "enabled=1" \
  --data-urlencode "from_source_ids=1" \
  --data-urlencode "to_source_ids=4" \
  --data-urlencode "filters=action eq tg" \
  --data-urlencode "body_template=" \
  --data-urlencode "subject_template=" \
  --data-urlencode "headers_template={}" \
  "$BASE/rules"

curl -s -o /dev/null -X POST -H 'X-F2A-Token: e2e-secret' -d '{"action":"tg"}' "$BASE/hook/gh-e2e"
wait_for "grep -q 'PATH /bot123456:e2e-token/sendMessage' $RECEIVED" || fail "Telegram 请求没有发出去"
grep -q 'PATH /bot123456:e2e-token/sendMessage' "$RECEIVED" || fail "Bot API 的路径不对：$(cat "$RECEIVED")"
grep -q 'Content-Type: application/json' "$RECEIVED" || fail "Bot API 请求的 Content-Type 不对"
grep -qF '"chat_id":-1001234567890' "$RECEIVED" || fail "chat_id 没有按 JSON 数字发出"
grep -qF '"message_thread_id":42' "$RECEIVED" || fail "话题 ID 没有带上"
grep -qF '"text":"{\"action\":\"tg\"}"' "$RECEIVED" || fail "正文没有原样发出去：$(cat "$RECEIVED")"
pass "转发到 Telegram：路径带 token、chat_id / message_thread_id / 正文都对"

# Bot API 用 HTTP 200 + {"ok":false} 报错，所以「收到回包」不等于成功 —— 得看日志里记成了什么。
tg_ok=0
i=0
while [ "$i" -lt 50 ]; do
  curl -fsS -b "$JAR" --get --data-urlencode "q=Telegram 目标" --data-urlencode "status=success" \
    "$BASE/deliveries" > "$WORK/tg-dl.html" || true
  if grep -qE '/deliveries/[0-9]+' "$WORK/tg-dl.html"; then tg_ok=1; break; fi
  sleep 0.2
  i=$((i + 1))
done
[ "$tg_ok" = "1" ] || fail "Telegram 投递没有被记成成功"
pass "Telegram 回包按 ok:true 判定，投递记成成功"

# 代理对 Telegram 同样有效：勾上之后请求要落在正向代理上。
post_form \
  --data-urlencode "name=Telegram 目标" \
  --data-urlencode "kind=telegram" \
  --data-urlencode "usage=out" \
  --data-urlencode "enabled=1" \
  --data-urlencode "tg_token=123456:e2e-token" \
  --data-urlencode "tg_chat_id=-1001234567890" \
  --data-urlencode "tg_thread_id=42" \
  --data-urlencode "tg_endpoint=http://127.0.0.1:$MOCK_PORT/bot" \
  --data-urlencode "use_proxy=1" \
  "$BASE/sources/4"

curl -s -o /dev/null -X POST -H 'X-F2A-Token: e2e-secret' -d '{"action":"tg"}' "$BASE/hook/gh-e2e"
wait_for "grep -q 'PROXY http://127.0.0.1:$MOCK_PORT/bot123456:e2e-token/sendMessage' $PROXY_LOG" \
  || fail "勾了代理的 Telegram 源没有走代理：$(cat "$PROXY_LOG" 2>/dev/null)"
pass "Telegram 源勾上「通过代理发送」后确实走了代理"

# 死目标那条规则用的是它自己的源 id；Telegram 源占了 4 号，所以往后挪一位。
echo
echo "== 失败重试与手动重放 =="
# 把重试参数调小，好让这段检查在几秒内跑完
post_form \
  --data-urlencode "web_port=$APP_PORT" \
  --data-urlencode "base_url=$BASE" \
  --data-urlencode "admin_user=$ADMIN_USER" \
  --data-urlencode "retry_max=2" \
  --data-urlencode "retry_backoff_seconds=1" \
  --data-urlencode "payload_max_bytes=65536" \
  --data-urlencode "log_retention_days=30" \
  "$BASE/settings"

# 目标指向一个没人监听的端口，投递必然失败
post_form \
  --data-urlencode "name=死目标" \
  --data-urlencode "kind=webhook" \
  --data-urlencode "usage=out" \
  --data-urlencode "enabled=1" \
  --data-urlencode "slug=" \
  --data-urlencode "auth_mode=none" \
  --data-urlencode "ip_allow=" \
  --data-urlencode "url=http://127.0.0.1:19099/dead" \
  --data-urlencode "http_method=POST" \
  --data-urlencode "headers={}" \
  "$BASE/sources"

post_form \
  --data-urlencode "name=必然失败的规则" \
  --data-urlencode "enabled=1" \
  --data-urlencode "from_source_ids=1" \
  --data-urlencode "to_source_ids=5" \
  --data-urlencode "filters=action eq boom" \
  --data-urlencode "body_template=" \
  --data-urlencode "subject_template=" \
  --data-urlencode "headers_template={}" \
  "$BASE/rules"

curl -s -o /dev/null -X POST -H 'X-F2A-Token: e2e-secret' \
  -d '{"action":"boom"}' "$BASE/hook/gh-e2e"

dead_ok=0
i=0
while [ "$i" -lt 75 ]; do
  curl -fsS -b "$JAR" "$BASE/deliveries?status=dead" > "$WORK/dead.html" || true
  # 不能只找规则名：筛选下拉框里也有规则名，那样一条记录都没有也会「通过」。
  # 必须同时出现指向详情页的链接，才说明表格里真有行。
  if grep -qE '/deliveries/[0-9]+' "$WORK/dead.html" && grep -q "必然失败的规则" "$WORK/dead.html"; then
    dead_ok=1
    break
  fi
  sleep 0.2
  i=$((i + 1))
done
[ "$dead_ok" = "1" ] || fail "重试次数用尽后应当在日志里出现「已放弃」记录"
pass "投递失败会按退避重试，次数用尽后标记为已放弃"

# 用 -m1 而不是 head -1：head 提前关闭管道会让 grep 吃到 SIGPIPE，
# 在 set -e + pipefail 下会直接把脚本干掉，且不留任何提示。
DEAD_ID=$(grep -m1 -oE '/deliveries/[0-9]+' "$WORK/dead.html" | grep -oE '[0-9]+$' || true)
[ -n "$DEAD_ID" ] || fail "没能从页面里取到失败记录的 id"
post_form "$BASE/deliveries/$DEAD_ID/replay"
curl -fsS -b "$JAR" "$BASE/deliveries/$DEAD_ID" > "$WORK/replay.html"
grep -q "必然失败的规则" "$WORK/replay.html" || fail "重放后详情页打不开"
grep -q "已重新入队\|重新投递" "$WORK/replay.html" || fail "重放后详情页没有显示操作入口"
pass "手动重放已重新入队（记录 #$DEAD_ID）"

echo
echo "== 配置导入 =="
# 先塞一条导出文件里没有的规则，确认导入是真的「整体替换」而不是合并
post_form \
  --data-urlencode "name=导入前乱加的规则" \
  --data-urlencode "enabled=1" \
  --data-urlencode "from_source_ids=1" \
  --data-urlencode "to_source_ids=2" \
  --data-urlencode "filters=" \
  --data-urlencode "body_template=" \
  --data-urlencode "subject_template=" \
  --data-urlencode "headers_template={}" \
  "$BASE/rules"

curl -fsS -c "$JAR" -b "$JAR" -o /dev/null \
  -F "file=@$WORK/export.json" "$BASE/settings/import"

curl -fsS -b "$JAR" -c "$JAR" "$BASE/rules" > "$WORK/rules2.html"
curl -fsS -b "$JAR" -c "$JAR" "$BASE/sources" > "$WORK/sources2.html"

if grep -q "导入前乱加的规则" "$WORK/rules2.html"; then
  fail "导入应当整体替换，但导出文件里没有的规则还在"
fi
grep -q "转发到 mock" "$WORK/rules2.html" || fail "导入后规则丢了"
grep -q "gh-e2e" "$WORK/sources2.html" || fail "导入后源丢了"
pass "导入整体替换生效，源与规则都恢复成导出时的样子"

echo
echo "== 默认密码与强制改密 =="
# 另起一个实例，故意不给 F2A_ADMIN_PASSWORD，走默认密码路径。
# 环境变量要清掉，否则会被上面那个实例的设置带走。
DP_PORT=$((APP_PORT + 2))
DP_BASE="http://127.0.0.1:$DP_PORT"
DP_JAR="$WORK/dp-cookies.txt"

env -u F2A_ADMIN_PASSWORD \
  F2A_DATA_DIR="$WORK/dp-data" \
  F2A_PORT="$DP_PORT" \
  F2A_BASE_URL="$DP_BASE" \
  "$WORK/f2a" > "$WORK/dp.log" 2>&1 &
DP_PID=$!
PIDS+=("$DP_PID")

dp_ok=0
i=0
while [ "$i" -lt 50 ]; do
  curl -fsS "$DP_BASE/healthz" >/dev/null 2>&1 && { dp_ok=1; break; }
  sleep 0.2
  i=$((i + 1))
done
[ "$dp_ok" = "1" ] || { cat "$WORK/dp.log"; fail "使用默认密码的实例没起来"; }

grep -q "当前使用默认管理员密码" "$WORK/dp.log" || fail "启动日志里应当提示正在使用默认密码"
pass "未提供 F2A_ADMIN_PASSWORD 时启用默认密码（admin / f2a），并在日志里告警"

# 默认密码必须能登录
curl -fsS -c "$DP_JAR" -b "$DP_JAR" -o /dev/null \
  -d "username=admin&password=f2a" "$DP_BASE/login"
grep -q f2a_session "$DP_JAR" || fail "默认密码 f2a 登不进去"
pass "默认密码 admin / f2a 可以登录"

# 但后台其它页面要被挡回设置页
for p in / /sources /rules /deliveries; do
  loc=$(curl -s -o /dev/null -w '%{redirect_url}' -b "$DP_JAR" "$DP_BASE$p")
  case "$loc" in
    *"/settings?ok=must_change_password"*) ;;
    *) fail "使用默认密码时 $p 应当被挡回设置页，实际跳转到 '$loc'" ;;
  esac
done
pass "改密前后台其它页面全部被挡回设置页"

# 设置页本身要能打开，否则没法改密
code=$(curl -s -o "$WORK/dp-settings.html" -w '%{http_code}' -b "$DP_JAR" "$DP_BASE/settings")
[ "$code" = "200" ] || fail "设置页应当可访问，实际 $code"
grep -q "仍在使用默认密码" "$WORK/dp-settings.html" || fail "设置页应当显示默认密码告警条"
pass "设置页可访问并显示了告警"

# 新密码不能还填默认密码（校验结果只出现在 POST 的响应里，所以要看这次响应）
curl -s -b "$DP_JAR" -c "$DP_JAR" -o "$WORK/dp-reject.html" -X POST \
  --data-urlencode "web_port=$DP_PORT" \
  --data-urlencode "base_url=$DP_BASE" \
  --data-urlencode "admin_user=admin" \
  --data-urlencode "new_password=f2a" \
  --data-urlencode "confirm_password=f2a" \
  --data-urlencode "retry_max=5" --data-urlencode "retry_backoff_seconds=10" \
  --data-urlencode "payload_max_bytes=65536" --data-urlencode "log_retention_days=30" \
  "$DP_BASE/settings"
grep -q "不能和默认密码相同" "$WORK/dp-reject.html" \
  || fail "新密码填成默认密码时应当被拒绝"
pass "新密码不允许与默认密码相同"

# 被拒之后仍然应当处于「必须改密」状态
loc=$(curl -s -o /dev/null -w '%{redirect_url}' -b "$DP_JAR" "$DP_BASE/")
case "$loc" in
  *"/settings?ok=must_change_password"*) ;;
  *) fail "改密被拒后仍应保持锁定，实际跳转到 '$loc'" ;;
esac

# 改成真密码后应当解锁
curl -fsS -b "$DP_JAR" -c "$DP_JAR" -o /dev/null -X POST \
  --data-urlencode "web_port=$DP_PORT" \
  --data-urlencode "base_url=$DP_BASE" \
  --data-urlencode "admin_user=admin" \
  --data-urlencode "new_password=brand-new-password" \
  --data-urlencode "confirm_password=brand-new-password" \
  --data-urlencode "retry_max=5" --data-urlencode "retry_backoff_seconds=10" \
  --data-urlencode "payload_max_bytes=65536" --data-urlencode "log_retention_days=30" \
  "$DP_BASE/settings"

code=$(curl -s -o /dev/null -w '%{http_code}' -b "$DP_JAR" "$DP_BASE/")
[ "$code" = "200" ] || fail "改完密码后首页应当可访问，实际 $code"
curl -fsS -b "$DP_JAR" "$DP_BASE/settings" > "$WORK/dp-settings2.html"
grep -q "仍在使用默认密码" "$WORK/dp-settings2.html" && fail "改完密码后不该再显示告警条"
pass "改掉默认密码后后台立即解锁，告警条消失"

# 旧密码必须失效
curl -s -o /dev/null -c "$WORK/dp-old.txt" -d "username=admin&password=f2a" "$DP_BASE/login"
grep -q f2a_session "$WORK/dp-old.txt" && fail "旧密码 f2a 改密后仍然能登录"
pass "改密后旧密码 f2a 失效"

kill "$DP_PID" 2>/dev/null || true
wait "$DP_PID" 2>/dev/null || true

echo
echo "== 端口热切换 =="
NEW_PORT=$((APP_PORT + 1))
post_form \
  --data-urlencode "web_port=$NEW_PORT" \
  --data-urlencode "base_url=http://127.0.0.1:$NEW_PORT" \
  --data-urlencode "admin_user=$ADMIN_USER" \
  --data-urlencode "retry_max=2" \
  --data-urlencode "retry_backoff_seconds=1" \
  --data-urlencode "payload_max_bytes=65536" \
  --data-urlencode "log_retention_days=30" \
  "$BASE/settings"

switched=0
i=0
while [ "$i" -lt 50 ]; do
  if curl -fsS "http://127.0.0.1:$NEW_PORT/healthz" >/dev/null 2>&1; then
    switched=1
    break
  fi
  sleep 0.2
  i=$((i + 1))
done
[ "$switched" = "1" ] || fail "改端口后应当在新端口上开始监听（无需重启进程）"
pass "监听端口已从 $APP_PORT 热切换到 $NEW_PORT，无需重启进程"

echo
echo "全部通过 🎉"
