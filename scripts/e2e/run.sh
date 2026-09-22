#!/bin/bash
# E2E 聚合入口：依次执行全部用例，任一失败即整体失败。
# 用法：bash scripts/e2e/run.sh [用例编号...]
#   不带参数执行全部；如 bash scripts/e2e/run.sh 02 只跑首次登录用例。
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"

if [ "$#" -gt 0 ]; then
  CASES=("$@")
else
  CASES=(01-proxy 02-first-login 03-account-delete)
fi

overall=0
for name in "${CASES[@]}"; do
  script="${HERE}/${name}.sh"
  echo
  echo "##########################################################"
  echo "# 运行用例：${name}"
  echo "##########################################################"
  if bash "$script"; then
    echo "# 用例 ${name} 通过"
  else
    echo "# 用例 ${name} 失败"
    overall=1
  fi
done

echo
if [ "$overall" = "0" ]; then
  echo "ALL E2E PASSED"
else
  echo "E2E FAILURES PRESENT"
fi
exit $overall
