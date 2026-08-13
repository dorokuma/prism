#!/bin/bash
# test_pre_commit.sh — pre-commit hook 的行为测试（无副作用：全部在临时 git
# 仓库中执行，不读取调用方工作树的暂存内容；直接调用 hook 时也 cd 进临时仓库
# 并清掉 GIT_DIR/GIT_INDEX_FILE/GIT_WORK_TREE，确保 git diff --cached 永远
# 只看临时仓库）。
#
# 环境隔离：脚本内每一条 git 命令（包括 git -C 的用例）都经过 gitenv，显式
# 清掉 GIT_DIR/GIT_INDEX_FILE/GIT_WORK_TREE —— GIT_DIR 优先于 -C，若调用方
# 环境继承了这三个变量，git -C $TMP/repo 会被重定向到调用方仓库。第 9 个用例
# 专门验证在继承伪 GIT_* 环境时脚本仍然只操作临时仓库。
# 运行：bash scripts/test_pre_commit.sh   （退出 0 = 全部通过）
set -euo pipefail

SRC="$(cd "$(dirname "$0")" && pwd)/pre-commit"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fails=0
pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; fails=$((fails + 1)); }

# gitenv: 清掉可能指向调用方仓库的 git 环境变量后执行命令。GIT_DIR /
# GIT_INDEX_FILE / GIT_WORK_TREE 一旦被继承就会覆盖 git -C 的目录解析
# （GIT_DIR 优先于 -C），所以每一条 git 命令都必须经过这里；否则在设置了
# 这些变量的调用方环境中运行本脚本会操作调用方仓库。
gitenv() {
    env -u GIT_DIR -u GIT_INDEX_FILE -u GIT_WORK_TREE "$@"
}

# g: 在临时仓库内执行 git（清掉继承的 GIT_* 环境变量）。
g() {
    gitenv git -C "$TMP/repo" "$@"
}

# 建一个全新 git 仓库并装上被测 hook。core.hooksPath 显式指回仓库内 hooks
# （宿主全局配置可能指向别处，否则 git 不会运行被测 hook）。
gitenv git init -q "$TMP/repo"
g config user.name "test"
g config user.email "test@example.com"
g config core.hooksPath ".git/hooks"
install -m 755 "$SRC" "$TMP/repo/.git/hooks/pre-commit"

# commit_staged: 把当前暂存内容提交；返回 hook 是否放行（0=放行，1=拦截）。
commit_staged() {
    if g commit -q -m "test commit" >/dev/null 2>&1; then
        return 0
    fi
    return 1
}

# 1. 新增文件含 sk-* 密钥 → 拦截。
echo "sk-abcdefghijklmnopqrstuvwxyz" > "$TMP/repo/leak.txt"
g add leak.txt
if commit_staged; then
    fail "commit with an sk-* key must be blocked"
else
    pass "sk-* key blocked"
fi
g reset -q

# 2. 长 hex 串（旧误报规则）→ 放行。
printf '%064d\n' 0 > "$TMP/repo/hex.txt"
g add hex.txt
if commit_staged; then
    pass "long hex string allowed (false-positive rule removed)"
else
    fail "long hex string must NOT be blocked"
fi

# 3. 长 base64 串（旧误报规则）→ 放行。
echo "YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXphYmNkZWZnaGlqa2xtbm9wcXJzdHV2d3h5eg==" > "$TMP/repo/b64.txt"
g add b64.txt
if commit_staged; then
    pass "long base64 string allowed (false-positive rule removed)"
else
    fail "long base64 string must NOT be blocked"
fi

# 4. 文件名含空格 + 新增行含 GitHub PAT → 拦截（文件名引号处理）。
printf 'token: ghp_%s\n' "abcdefghijklmnopqrstuvwxyz1234567890" > "$TMP/repo/pat file.txt"
g add "pat file.txt"
if commit_staged; then
    fail "commit with a ghp_* PAT (in a spaced file name) must be blocked"
else
    pass "ghp_* PAT in a spaced file name blocked"
fi
g reset -q

# 5. 只扫描新增行：基线文件中已存在密钥行（历史遗留，用 --no-verify 提交，
#    模拟 hook 生效前留下的行），后续修改不碰该行 → 放行；同一文件新增
#    一行含密钥 → 拦截。
printf 'old: sk-oldsecretoldsecretoldsecret99\n' > "$TMP/repo/existing.txt"
g add existing.txt
g commit -q --no-verify -m "baseline" || fail "baseline commit (--no-verify) must pass"
printf 'old: sk-oldsecretoldsecretoldsecret99\nkeep: hello\n' > "$TMP/repo/existing.txt"
g add existing.txt
commit_staged || fail "a diff adding only non-key lines must pass (the old key line is untouched)"
printf 'old: sk-oldsecretoldsecretoldsecret99\nkeep: hello\nnew: sk-newsecretnewsecretnewsecret88\n' > "$TMP/repo/existing.txt"
g add existing.txt
if commit_staged; then
    fail "a NEW key line in a modified file must be blocked"
else
    pass "only added lines are scanned (new key line blocked)"
fi
g reset -q

# 6. 无私密数据 → 放行（正常提交）。
echo "hello world" > "$TMP/repo/ok.txt"
g add ok.txt
if commit_staged; then
    pass "clean commit allowed"
else
    fail "clean commit must pass"
fi

# 7. 无暂存文件 → hook 直接放行。必须在临时仓库目录内、且清掉可能指向
#    调用方仓库的 git 环境变量后执行：hook 内部用 `git diff --cached`
#    读取“当前目录所在的仓库”的暂存区，若在调用方目录直接运行会读到
#    调用方工作树的暂存内容（甚至被调用方未提交的密钥误拦截/误放行）。
if (cd "$TMP/repo" && gitenv "$SRC" >/dev/null 2>&1); then
    pass "no staged files: hook exits 0 (run inside the temp repo)"
else
    fail "no staged files: hook must exit 0"
fi

# 8. 同一直接调用，但在调用方目录（本脚本所在目录）外运行必须仍只看到临时
#    仓库：在临时仓库放一个 sk-* 密钥文件并暂存，然后从仓库内直接调用
#    hook —— 它必须基于临时仓库的暂存区拦截，而不是读到调用方工作树。
echo "sk-abcdefghijklmnopqrstuvwxyz" > "$TMP/repo/leak2.txt"
g add leak2.txt
if (cd "$TMP/repo" && gitenv "$SRC" >/dev/null 2>&1); then
    fail "direct hook run inside the temp repo must scan the temp repo's staged diff (staged sk-* key must block)"
else
    pass "direct hook run scans only the temp repo's staged content"
fi
g reset -q

# 9. 继承伪 GIT_* 环境（模拟调用方设置了 GIT_DIR / GIT_INDEX_FILE /
#    GIT_WORK_TREE 指向一个无关的 decoy 仓库）：脚本内所有 git 命令（包括
#    git -C 的用例）都必须清掉这三个变量后仍只操作临时仓库。若某条命令没有
#    清理，git -C 会被 GIT_DIR/GIT_INDEX_FILE 重定向到 decoy，提交的是
#    decoy 的暂存区 → 本用例会误放行而失败；同时验证 decoy 从未被写入
#    （它没有产生任何提交）。
#    decoy 的暂存内容必须是能成功 commit 的无密钥内容：若它也有 sk-* 之类
#    会命中 secret hook 的内容，被重定向的提交会同样被 secret hook 拦截，
#    用例就会因为“提交被拦”而误判 PASS（假绿）。只有临时仓库的暂存区含
#    密钥：正确路径才拦截，错误路径（打到 decoy）才提交成功并被判 FAIL。
DECOY="$(mktemp -d)"
gitenv git init -q "$DECOY"
gitenv git -C "$DECOY" config user.name "decoy"
gitenv git -C "$DECOY" config user.email "decoy@example.com"
echo "decoy file content" > "$DECOY/decoy.txt"
gitenv git -C "$DECOY" add decoy.txt
echo "sk-abcdefghijklmnopqrstuvwxyz" > "$TMP/repo/leak3.txt"
g add leak3.txt
if (
    cd "$TMP/repo" \
    && GIT_DIR="$DECOY/.git" GIT_INDEX_FILE="$DECOY/.git/index" GIT_WORK_TREE="$DECOY" commit_staged
); then
    fail "with fake GIT_* env inherited, the commit must still scan the TEMP repo's staged leak (it must not be redirected to the decoy repo)"
else
    pass "fake GIT_* env inherited: git commands still operate only on the temp repo (staged leak blocked)"
fi
if gitenv git -C "$DECOY" rev-parse --verify HEAD >/dev/null 2>&1; then
    fail "the decoy repo must never receive a commit (the script must not touch the caller's repo)"
else
    pass "decoy repo untouched (no commit was ever made there)"
fi
g reset -q

echo
if [ "$fails" -eq 0 ]; then
    echo "ALL PASS"
    exit 0
fi
echo "$fails test(s) FAILED"
exit 1
