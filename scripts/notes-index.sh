#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
NOTES_DIR="${REPO_ROOT}/.agents/notes"
INDEX_FILE="${NOTES_DIR}/INDEX.md"

if [ ! -d "${NOTES_DIR}" ]; then
    echo "Notes directory not found: ${NOTES_DIR}" >&2
    exit 1
fi

mapfile -t FILES < <(find "${NOTES_DIR}" -maxdepth 1 -type f -name "*.md" \
    ! -name "_template.md" \
    ! -name "README.md" \
    ! -name "INDEX.md" \
    -exec basename {} \; | sort -r)

COUNT="${#FILES[@]}"

{
    echo "# 架构决策与踩坑笔记索引"
    echo ""
    echo "> 自动生成自 \`scripts/notes-index.sh\`（$(date +%Y-%m-%d)），请勿手动编辑。"
    echo ""
    if [ "${COUNT}" -eq 0 ]; then
        echo "暂无笔记。"
    else
        echo "| 日期/文件 | 模块 | 状态 | 结论 |"
        echo "|---|---|---|---|"
        for f in "${FILES[@]}"; do
            full_path="${NOTES_DIR}/${f}"
            module=$(sed -n '/^---$/,/^---$/p' "${full_path}" | grep -E '^模块:' | head -n1 | sed -E 's/^模块:[[:space:]]*"?([^"]*)"?/\1/' | sed -E 's/[[:space:]]*#.*//; s/["'\'']//g; s/^[[:space:]]*//; s/[[:space:]]*$//' || true)
            status=$(sed -n '/^---$/,/^---$/p' "${full_path}" | grep -E '^status:' | head -n1 | sed -E 's/^status:[[:space:]]*"?([^"]*)"?/\1/' | sed -E 's/[[:space:]]*#.*//; s/["'\'']//g; s/^[[:space:]]*//; s/[[:space:]]*$//' || true)
            conclusion=$(awk '/^## 一句话结论/{getline; while(/^$/){getline}; print; exit}' "${full_path}" | sed -E 's/^[-*][[:space:]]*//' || true)
            [ -z "${module}" ] && module="-"
            [ -z "${status}" ] && status="active"
            [ -z "${conclusion}" ] && conclusion="-"
            echo "| [${f}](${f}) | ${module} | ${status} | ${conclusion} |"
        done
    fi
} > "${INDEX_FILE}"

echo "生成笔记索引完成：${INDEX_FILE}（共 ${COUNT} 篇笔记）"
