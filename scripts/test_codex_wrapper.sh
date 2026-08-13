#!/bin/bash
# test_codex_wrapper.sh — codex-wrapper.sh 的最小行为测试（无副作用：不接触
# 真实 codex/systemctl/prism，全部走临时目录与假命令）。
# 运行：bash scripts/test_codex_wrapper.sh   （退出 0 = 全部通过）
set -euo pipefail

SRC="$(cd "$(dirname "$0")" && pwd)/codex-wrapper.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fails=0
pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; fails=$((fails + 1)); }

# bash 的绝对路径：run_wrapper 会修改 PATH，不能用相对查找再调 bash。
BASH_BIN="$(command -v bash)"

# 假 codex：记录被调用的参数并退出 0。
mkdir -p "$TMP/realbin" "$TMP/only-wrapper"
cat > "$TMP/realbin/codex" <<'EOF'
#!/bin/bash
echo "FAKE_CODEX called with: $*" >> "$CODEX_CALL_LOG"
exit 0
EOF
chmod +x "$TMP/realbin/codex"

# wrapper 本体（symlink 成 codex，模拟“wrapper 在 PATH 中先于真实 codex”）。
# 目录里同时放一个真实 readlink 的 symlink：wrapper 内部解析（script_path /
# resolve_real_codex）要用 readlink，而用例 6 的 PATH 只含本目录——这是临时
# 目录自身提供依赖，不依赖 /usr/bin 或 /bin 里有没有 codex。
ln -s "$SRC" "$TMP/only-wrapper/codex"
ln -s "$(command -v readlink)" "$TMP/only-wrapper/readlink"

# 每个用例独立环境：空 CODEX_HOME（无 config.toml → 不触发 re-gen），
# GENERATOR 指向不存在的路径也无妨（CONFIG 不存在即跳过）。PATH 保留系统
# 目录（wrapper 内部要用 readlink/python3），wrapper 目录放在最前。
run_wrapper() {
    local path_env="$1"
    shift
    CODEX_HOME="$TMP/home" \
    PRISM_TOOLS_JSON="$TMP/mcp_tools.json" \
    PRISM_GENERATOR="$TMP/no-such-generator.py" \
    CODEX_CALL_LOG="$TMP/call.log" \
    PATH="$path_env:$PATH" \
    "$BASH_BIN" "$SRC" "$@" >/dev/null 2>"$TMP/stderr.log"
}

# 1. wrapper 在 PATH 前面 + 真实 codex 在后面：必须跳过自身、调用真实 codex。
rm -f "$TMP/call.log"
if run_wrapper "$TMP/only-wrapper:$TMP/realbin" alpha beta; then
    if [ -f "$TMP/call.log" ] && grep -q "alpha beta" "$TMP/call.log"; then
        pass "self-symlink skipped; real codex exec'd with args"
    else
        fail "real codex was not invoked (call log: ${TMP/call.log:-<missing>})"
    fi
else
    fail "wrapper exited non-zero although a real codex exists"
fi

# 2. 只有 wrapper 自己（无真实 codex）：必须明确非零退出且不递归执行自身。
#    用最小 PATH（wrapper 目录 + readlink 所在系统目录），确保其他位置
#    的真实 codex 不会干扰本用例。
rm -f "$TMP/call.log"
if run_wrapper "$TMP/only-wrapper:/usr/bin:/bin"; then
    fail "wrapper must exit non-zero when no real codex exists"
else
    code=$?
    if [ "$code" -ne 127 ]; then
        fail "wrapper exit code = $code, want 127 (clear non-zero)"
    elif [ -f "$TMP/call.log" ]; then
        fail "wrapper recursively executed itself"
    elif grep -q "real codex binary not found" "$TMP/stderr.log"; then
        pass "no real codex: exit 127 with a clear message, no recursion"
    else
        fail "no real codex: stderr lacks the explanatory message: $(cat "$TMP/stderr.log")"
    fi
fi

# 3. CODEX_BIN 显式指定：直接使用，不做 PATH 解析。
rm -f "$TMP/call.log"
cat > "$TMP/realbin/other-codex" <<'EOF'
#!/bin/bash
echo "OTHER called with: $*" >> "$CODEX_CALL_LOG"
exit 0
EOF
chmod +x "$TMP/realbin/other-codex"
if (
    CODEX_HOME="$TMP/home" \
    PRISM_TOOLS_JSON="$TMP/mcp_tools.json" \
    PRISM_GENERATOR="$TMP/no-such-generator.py" \
    CODEX_CALL_LOG="$TMP/call.log" \
    CODEX_BIN="$TMP/realbin/other-codex" \
    PATH="$TMP/only-wrapper:$PATH" \
    "$BASH_BIN" "$SRC" x y >/dev/null 2>&1
); then
    if [ -f "$TMP/call.log" ] && grep -q "x y" "$TMP/call.log"; then
        pass "CODEX_BIN override used as-is"
    else
        fail "CODEX_BIN override not invoked"
    fi
else
    fail "wrapper with CODEX_BIN override exited non-zero"
fi

# 4. CODEX_BIN 显式指向 wrapper 自身：必须识别并 exit 127，绝不递归执行。
rm -f "$TMP/call.log"
if (
    CODEX_HOME="$TMP/home" \
    PRISM_TOOLS_JSON="$TMP/mcp_tools.json" \
    PRISM_GENERATOR="$TMP/no-such-generator.py" \
    CODEX_CALL_LOG="$TMP/call.log" \
    CODEX_BIN="$SRC" \
    PATH="$TMP/only-wrapper:$PATH" \
    "$BASH_BIN" "$SRC" self1 >/dev/null 2>"$TMP/stderr-self.log"
); then
    fail "wrapper with CODEX_BIN=self must exit 127, not exec itself"
else
    code=$?
    if [ "$code" -ne 127 ]; then
        fail "CODEX_BIN=self exit code = $code, want 127"
    elif [ -f "$TMP/call.log" ]; then
        fail "wrapper with CODEX_BIN=self recursively executed itself"
    elif grep -q "this wrapper itself" "$TMP/stderr-self.log"; then
        pass "CODEX_BIN=self: exit 127 with a clear message, no recursion"
    else
        fail "CODEX_BIN=self: stderr lacks the explanatory message: $(cat "$TMP/stderr-self.log")"
    fi
fi

# 5. CODEX_BIN 指向 wrapper 的 symlink：同样必须 exit 127（readlink -f 后
#    与脚本自身相同，检测不依赖路径写法）。
rm -f "$TMP/call.log"
ln -s "$SRC" "$TMP/realbin/codex-wrapper-link"
if (
    CODEX_HOME="$TMP/home" \
    PRISM_TOOLS_JSON="$TMP/mcp_tools.json" \
    PRISM_GENERATOR="$TMP/no-such-generator.py" \
    CODEX_CALL_LOG="$TMP/call.log" \
    CODEX_BIN="$TMP/realbin/codex-wrapper-link" \
    PATH="$TMP/only-wrapper:$PATH" \
    "$BASH_BIN" "$SRC" self2 >/dev/null 2>"$TMP/stderr-sym.log"
); then
    fail "wrapper with CODEX_BIN=symlink-to-self must exit 127"
else
    code=$?
    if [ "$code" -ne 127 ]; then
        fail "CODEX_BIN=symlink-to-self exit code = $code, want 127"
    elif [ -f "$TMP/call.log" ]; then
        fail "wrapper with CODEX_BIN=symlink-to-self recursively executed itself"
    elif grep -q "this wrapper itself" "$TMP/stderr-sym.log"; then
        pass "CODEX_BIN=symlink-to-self: exit 127 with a clear message, no recursion"
    else
        fail "CODEX_BIN=symlink-to-self: stderr lacks the explanatory message: $(cat "$TMP/stderr-sym.log")"
    fi
fi

# 6. CODEX_BIN 为不含斜杠的裸命令名（如 CODEX_BIN=codex）：必须先按
#    PATH 解析成真实路径再比较/执行。PATH 只含 wrapper 的临时目录（除
#    readlink 外没有别的工具，也不加 /usr/bin、/bin——不依赖系统目录里是否
#    存在真实 codex）时 → 必须 exit 127 且不递归（裸名解析跳过 wrapper，
#    找不到真实二进制）。
rm -f "$TMP/call.log"
if (
    CODEX_HOME="$TMP/home" \
    PRISM_TOOLS_JSON="$TMP/mcp_tools.json" \
    PRISM_GENERATOR="$TMP/no-such-generator.py" \
    CODEX_CALL_LOG="$TMP/call.log" \
    CODEX_BIN="codex" \
    PATH="$TMP/only-wrapper" \
    "$BASH_BIN" "$SRC" bare1 >/dev/null 2>"$TMP/stderr-bare.log"
); then
    fail "bare CODEX_BIN with only the wrapper in PATH must exit 127, not exec anything"
else
    code=$?
    if [ "$code" -ne 127 ]; then
        fail "bare CODEX_BIN (wrapper-only PATH) exit code = $code, want 127"
    elif [ -f "$TMP/call.log" ]; then
        fail "bare CODEX_BIN recursively executed itself"
    elif grep -q "real codex binary not found" "$TMP/stderr-bare.log"; then
        pass "bare CODEX_BIN with wrapper-only PATH: exit 127 via PATH resolution, no recursion"
    else
        fail "bare CODEX_BIN: stderr lacks the explanatory message: $(cat "$TMP/stderr-bare.log")"
    fi
fi

# 7. CODEX_BIN=codex 裸命令名，PATH 首项是 wrapper、后面有真实 codex：
#    必须跳过 wrapper、解析到真实路径并 exec（不能把裸名当相对路径，
#    也不能命中 wrapper 自身）。
rm -f "$TMP/call.log"
if (
    CODEX_HOME="$TMP/home" \
    PRISM_TOOLS_JSON="$TMP/mcp_tools.json" \
    PRISM_GENERATOR="$TMP/no-such-generator.py" \
    CODEX_CALL_LOG="$TMP/call.log" \
    CODEX_BIN="codex" \
    PATH="$TMP/only-wrapper:$TMP/realbin:$PATH" \
    "$BASH_BIN" "$SRC" bare2 >/dev/null 2>"$TMP/stderr-bare2.log"
); then
    if [ -f "$TMP/call.log" ] && grep -q "bare2" "$TMP/call.log"; then
        pass "bare CODEX_BIN resolved through PATH (wrapper skipped), real codex exec'd with args"
    else
        fail "bare CODEX_BIN: real codex not invoked (call log: ${TMP/call.log:-<missing>})"
    fi
else
    fail "bare CODEX_BIN with a real codex later in PATH must resolve and exec it (stderr: $(cat "$TMP/stderr-bare2.log"))"
fi

echo
if [ "$fails" -eq 0 ]; then
    echo "ALL PASS"
    exit 0
fi
echo "$fails test(s) FAILED"
exit 1
