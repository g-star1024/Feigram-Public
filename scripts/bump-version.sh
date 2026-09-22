#!/usr/bin/env bash
# 一键升级版本号：同步修改版本五处真源，避免漏改导致
# 「包名是新版本、包内仍为旧版本（飞牛判定同版本 / cmd 跳过重启）」的事故。
#
# 五处：
#   ① scripts/build-native-fpk.sh        VERSION 默认值
#   ② fnos-native-package/manifest       version + changelog
#   ③ fnos-native-package/cmd/main       APP_VERSION + APP_CHANGELOG
#   ④ server/src/releaseContent.js       插入新公告（置顶）
#   ⑤ docs/release-notes.md              插入新版本发布说明（置顶）
#
# 用法：
#   bash scripts/bump-version.sh <version> [summary] [bullets_file]
#     version      语义版本，如 2.3.0
#     summary      单行更新摘要（用于 manifest / cmd changelog）
#     bullets_file 可选：每行一个更新要点，用于应用内公告与发布说明
#                  （不含前导 "- "，脚本自动补）
#   也可用环境变量 CHANGELOG_SUMMARY / CHANGELOG_BULLETS_FILE 传参。
set -euo pipefail

VERSION="${1:-}"
SUMMARY="${2:-${CHANGELOG_SUMMARY:-}}"
BULLETS_FILE="${3:-${CHANGELOG_BULLETS_FILE:-}}"

if [ -z "$VERSION" ]; then
  echo "用法：bash scripts/bump-version.sh <version> [summary] [bullets_file]" >&2
  exit 1
fi
if ! printf '%s' "$VERSION" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$'; then
  echo "版本号格式无效：${VERSION}（应为 X.Y.Z）" >&2
  exit 1
fi

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SUMMARY="${SUMMARY:-Feigram ${VERSION} 常规更新。}"

PY="${PYTHON:-$(command -v python3 || echo /Users/huluobo/.workbuddy/binaries/python/versions/3.13.12/bin/python3)}"

export FG_ROOT="$ROOT" FG_VERSION="$VERSION" FG_SUMMARY="$SUMMARY" FG_BULLETS_FILE="${BULLETS_FILE}"
"$PY" <<'PYEOF'
import datetime
import json
import os
import re
import sys

root = os.environ["FG_ROOT"]
version = os.environ["FG_VERSION"]
summary = os.environ["FG_SUMMARY"]
bullets_file = os.environ.get("FG_BULLETS_FILE", "")

bullets = []
if bullets_file:
    with open(bullets_file, "r", encoding="utf-8") as handle:
        bullets = [line.strip() for line in handle if line.strip()]
if not bullets:
    bullets = [summary.rstrip("。")]

created = datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.000Z")


def read(path):
    with open(path, "r", encoding="utf-8") as handle:
        return handle.read()


def write(path, content):
    with open(path, "w", encoding="utf-8") as handle:
        handle.write(content)


def replace_once(content, pattern, repl, path):
    new, count = re.subn(pattern, repl, content, count=1, flags=re.M)
    if count != 1:
        sys.exit(f"替换失败（{path}）：{pattern}")
    return new


# ① build-native-fpk.sh
p = os.path.join(root, "scripts", "build-native-fpk.sh")
c = replace_once(
    read(p),
    r'^VERSION="\$\{VERSION:-[^}]*\}"',
    f'VERSION="${{VERSION:-{version}}}"',
    p,
)
write(p, c)

# ② manifest
p = os.path.join(root, "fnos-native-package", "manifest")
c = read(p)
c = replace_once(c, r'^version=.*$', f'version={version}', p)
c = replace_once(c, r'^changelog=.*$', f'changelog={summary}', p)
write(p, c)

# ③ cmd/main
p = os.path.join(root, "fnos-native-package", "cmd", "main")
c = read(p)
c = replace_once(c, r'^export APP_VERSION="[^"]*"', f'export APP_VERSION="{version}"', p)
c = replace_once(c, r'^export APP_CHANGELOG="[^"]*"', f'export APP_CHANGELOG="{summary}"', p)
write(p, c)

# ④ releaseContent.js —— 在 announcements 数组开头插入新公告
body_lines = ",\n      ".join(json.dumps(b, ensure_ascii=False) for b in bullets)
entry = (
    '  {\n'
    f'    id: "release-{version}",\n'
    f'    title: "Feigram {version} 更新",\n'
    f'    version: "{version}",\n'
    '    level: "success",\n'
    f'    createdAt: "{created}",\n'
    '    body: [\n'
    f'{body_lines}\n'
    '    ].join("\\n")\n'
    '  },\n'
)
p = os.path.join(root, "server", "src", "releaseContent.js")
c = read(p)
marker = "const announcements = [\n"
if marker not in c:
    sys.exit("找不到 announcements 数组起点")
c = c.replace(marker, marker + entry, 1)
write(p, c)

# ⑤ release-notes.md —— 在首个「## 版本」前插入新版本
notes_bullets = "\n".join(f"- {b}" for b in bullets)
section = (
    f"## 版本 {version}\n\n"
    f"{notes_bullets}\n\n"
)
p = os.path.join(root, "docs", "release-notes.md")
c = read(p)
m = re.search(r'^## 版本 ', c, flags=re.M)
if not m:
    sys.exit("release-notes.md 中找不到版本章节")
c = c[:m.start()] + section + c[m.start():]
write(p, c)

print(f"已升级到 {version}（五处真源）")
PYEOF

echo
echo "完成。下一步：bash scripts/build-native-fpk.sh 构建后，用 scripts/verify-fpk.sh 核证。"
