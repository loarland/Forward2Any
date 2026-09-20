#!/usr/bin/env python3
"""从 Pico 官方配色变体里抽出「主色族」，生成 static/palettes/*.css。

为什么要抽而不是直接塞 20 份 pico.<颜色>.min.css：那 20 份每份 87KB，
加起来 1.7MB 全要编进二进制，而每个变体之间**只差 7~9 个 CSS 变量**。
所以这里把差异部分单独提出来，每份约 1KB。

抽出来的值全部来自官方文件（不是自己编的），三个块的选择器也照着 Pico 原样写：
  :root:not([data-theme=dark])                         亮色
  [data-theme=dark]                                    显式深色
  @media (prefers-color-scheme:dark) :root:not([data-theme])   跟随系统

用法（只有换 Pico 版本时才需要重跑）：
    python3 scripts/gen-palettes.py
"""
import pathlib
import re
import subprocess
import sys

VER = "2.1.1"
BASE = "https://cdn.jsdelivr.net/npm/@picocss/pico@%s/css" % VER
# jsdelivr 上 pico.conditional.<颜色>.min.css 的全部颜色；default 是不带后缀那份
COLORS = [
    "default", "amber", "blue", "cyan", "fuchsia", "green", "grey", "indigo",
    "jade", "lime", "orange", "pink", "pumpkin", "purple", "red", "sand",
    "slate", "violet", "yellow", "zinc",
]
# 变体之间会变的属性。多取几个没关系（值都来自官方），漏了才会出错，
# 所以这里按「主色族全家人 + 两个跟着主色走的杂项」来取。
KEEP = re.compile(r"^--pico-(primary[a-z-]*|text-selection-color|switch-thumb-box-shadow)$")

OUT = pathlib.Path(__file__).resolve().parent.parent / "internal/web/ui/static/palettes"
CACHE = pathlib.Path("/tmp/pico-palettes")


def fetch(name: str) -> str:
    CACHE.mkdir(parents=True, exist_ok=True)
    f = CACHE / (name + ".css")
    if not f.exists():
        url = "%s/pico.conditional%s.min.css" % (BASE, "" if name == "default" else "." + name)
        subprocess.run(["curl", "-fsSL", "-o", str(f), url], check=True)
    return f.read_text()


def balanced_block(css: str, open_brace: int) -> str:
    """从某个 `{` 开始做配平扫描，返回块内文本。

    不去全文数括号：压缩过的文件里括号关系一错综，栈就错位了（第一版就是这么
    翻车的 —— 最后把三块的值全归到了同一类）。从已知的块首开始配平才是可靠的。
    """
    depth = 0
    for i in range(open_brace, len(css)):
        if css[i] == "{":
            depth += 1
        elif css[i] == "}":
            depth -= 1
            if depth == 0:
                body = css[open_brace + 1:i]
                if "{" in body or "}" in body:
                    raise ValueError("块里居然还有嵌套，解析假设不成立")
                return body
    raise ValueError("括号没配平")


def pick(body: str) -> dict[str, str]:
    """从块内文本里挑出主色族变量。"""
    out = {}
    for m in re.finditer(r"(--pico-[a-z0-9-]+)\s*:([^;]+);", body):
        if KEEP.match(m.group(1)):
            out[m.group(1)] = m.group(2).strip()
    return out


def extract(css: str) -> dict[str, dict[str, str]]:
    """取出「亮色 / 自动深色 / 显式深色」三块的主色族变量。

    Pico 的 conditional 构建里，这三块的选择器是固定的：
      :root:not([data-theme=dark]),[data-theme=light] { ... }        亮色
      @media only screen and (prefers-color-scheme:dark) { ... }     跟随系统
      [data-theme=dark] { ... }                                      显式深色
    """
    light_at = css.index(":root:not([data-theme=dark])")
    dark_at = css.index("[data-theme=dark]{")
    media_at = css.index("prefers-color-scheme:dark){")
    # 深色块要在 media 之前（用 index 找到的是正文里那个，不是 :not() 里的）
    auto_at = media_at + css[media_at:].index("{") + 1

    out = {
        "light": pick(balanced_block(css, css.index("{", light_at))),
        "auto-dark": pick(balanced_block(css, css.index("{", auto_at))),
        "dark": pick(balanced_block(css, dark_at + len("[data-theme=dark]"))),
    }
    for scheme, props in out.items():
        if len(props) < 8:
            raise ValueError("%s 只抽到 %d 个变量，Pico 的结构可能变了" % (scheme, len(props)))
    if out["light"] == out["dark"]:
        raise ValueError("亮色和深色抽出来一样，肯定解析错了")
    return out


def block(selector: str, props: dict[str, str], indent: str = "") -> str:
    lines = ["%s%s {" % (indent, selector)]
    lines += ["%s  %s: %s;" % (indent, k, props[k]) for k in sorted(props)]
    lines.append("%s}" % indent)
    return "\n".join(lines)


def main() -> int:
    OUT.mkdir(parents=True, exist_ok=True)
    for name in COLORS:
        try:
            v = extract(fetch(name))
        except ValueError as e:
            print("❌ %s：%s" % (name, e), file=sys.stderr)
            return 1
        body = [
            "/* Pico CSS v%s 的「%s」配色，由 scripts/gen-palettes.py 从官方文件抽出。" % (VER, name),
            "   只含主色族变量，叠在 pico.min.css 之后即可整套换色 —— 这样不用把 20 份",
            "   87KB 的完整主题都编进二进制。改 Pico 版本时重跑那个脚本，别手改这个文件。 */",
            "",
            block(":root:not([data-theme=dark])", v["light"]),
            "",
            block("[data-theme=dark]", v["dark"]),
            "",
            "@media (prefers-color-scheme: dark) {",
            block(":root:not([data-theme])", v["auto-dark"], indent="  "),
            "}",
            "",
        ]
        (OUT / (name + ".css")).write_text("\n".join(body))
    total = sum(f.stat().st_size for f in OUT.glob("*.css"))
    print("已生成 %d 份配色，共 %d 字节（平均 %d）" % (len(COLORS), total, total // len(COLORS)))
    return 0


if __name__ == "__main__":
    sys.exit(main())
