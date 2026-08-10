package render

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func sp(n int) string { return strings.Repeat(" ", n) }

// assertAllLinesWidth fails if any line of out (split on "\n") has a display
// width different from want. This is the real alignment check: it verifies
// every row of the table occupies the same terminal columns.
func assertAllLinesWidth(t *testing.T, out string, want int) {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	for i, line := range lines {
		if w := DisplayWidth(line); w != want {
			t.Errorf("line %d %q: display width = %d, want %d", i, line, w, want)
		}
	}
}

func TestTableASCII(t *testing.T) {
	tbl := &Table{
		Columns: []Column{
			{Title: "MODEL", Align: AlignLeft},
			{Title: "REQS", Align: AlignRight},
			{Title: "TOKENS", Align: AlignRight},
			{Title: "COST", Align: AlignRight},
		},
		Rows: [][]string{
			{"deepseek-v4-flash", "1,284", "1.54M", "$0.263"},
			{"deepseek-v4-pro", "412", "539k", "$0.273"},
			{"glm-5.2", "87", "150k", "$0.300"},
		},
	}
	want := strings.Join([]string{
		"  " + "MODEL" + sp(12) + "   " + " REQS" + "   " + "TOKENS" + "   " + "  COST",
		"  " + "deepseek-v4-flash" + "   " + "1,284" + "   " + " 1.54M" + "   " + "$0.263",
		"  " + "deepseek-v4-pro" + sp(2) + "   " + "  412" + "   " + "  539k" + "   " + "$0.273",
		"  " + "glm-5.2" + sp(10) + "   " + "   87" + "   " + "  150k" + "   " + "$0.300",
	}, "\n") + "\n"
	got := tbl.Render()
	if got != want {
		t.Fatalf("Render() mismatch\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
	assertAllLinesWidth(t, got, 45)
}

func TestTableChineseAlignment(t *testing.T) {
	tbl := &Table{
		Columns: []Column{
			{Title: "模型", Align: AlignLeft},
			{Title: "数量", Align: AlignRight},
		},
		Rows: [][]string{
			{"苹果", "12"},
			{"香蕉", "123"},
			{"a", "1234"},
		},
	}
	want := strings.Join([]string{
		"  " + "模型" + "   " + "数量",
		"  " + "苹果" + "   " + "  12",
		"  " + "香蕉" + "   " + " 123",
		"  " + "a" + sp(3) + "   " + "1234",
	}, "\n") + "\n"
	got := tbl.Render()
	if got != want {
		t.Fatalf("Render() mismatch\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
	assertAllLinesWidth(t, got, 13)
}

func TestTableEmojiAndCJKPunctAlignment(t *testing.T) {
	tbl := &Table{
		Columns: []Column{{Title: "名称", Align: AlignLeft}},
		Rows: [][]string{
			{"苹果，🍎"},
			{"香蕉，🍌"},
			{"长名称abc"},
		},
	}
	want := strings.Join([]string{
		"  " + "名称" + sp(5),
		"  " + "苹果，🍎" + sp(1),
		"  " + "香蕉，🍌" + sp(1),
		"  " + "长名称abc",
	}, "\n") + "\n"
	got := tbl.Render()
	if got != want {
		t.Fatalf("Render() mismatch\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
	assertAllLinesWidth(t, got, 11)
}

func TestTableTruncation(t *testing.T) {
	tbl := &Table{
		Columns: []Column{
			{Title: "模型", Align: AlignLeft, MaxWidth: 10},
			{Title: "数值", Align: AlignRight, MaxWidth: 8},
		},
		Rows: [][]string{
			{"deepseek-v4-flash中文", "123456789"},
			{"短", "42"},
		},
	}
	want := strings.Join([]string{
		"  " + "模型" + sp(6) + "   " + sp(4) + "数值",
		"  " + "deepseek-…" + "   " + "1234567…",
		"  " + "短" + sp(8) + "   " + sp(6) + "42",
	}, "\n") + "\n"
	got := tbl.Render()
	if got != want {
		t.Fatalf("Render() mismatch\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
	assertAllLinesWidth(t, got, 23)
	if !strings.Contains(got, "…") {
		t.Error("expected ellipsis in truncated output")
	}
	if !utf8.ValidString(got) {
		t.Error("output is not valid UTF-8")
	}
	if strings.ContainsRune(got, '\uFFFD') {
		t.Error("output contains U+FFFD: a multi-byte character was split")
	}
}

func TestTableTruncationChineseStaysAligned(t *testing.T) {
	tbl := &Table{
		Columns: []Column{{Title: "文本", Align: AlignLeft, MaxWidth: 7}},
		Rows:    [][]string{{"中中中中中"}, {"abc"}},
	}
	want := strings.Join([]string{
		"  " + "文本" + sp(3),
		"  " + "中中中…",
		"  " + "abc" + sp(4),
	}, "\n") + "\n"
	got := tbl.Render()
	if got != want {
		t.Fatalf("Render() mismatch\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
	assertAllLinesWidth(t, got, 9)
}

func TestTableTotalRow(t *testing.T) {
	tbl := &Table{
		Columns: []Column{
			{Title: "名称", Align: AlignLeft},
			{Title: "请求", Align: AlignRight},
			{Title: "金额", Align: AlignRight},
		},
		Rows: [][]string{
			{"a", "1", "$1.000"},
			{"bb", "22", "$2.000"},
		},
		TotalRow: []string{"合计", "23", "$3.000"},
	}
	want := strings.Join([]string{
		"  " + "名称" + "   " + "请求" + "   " + "  金额",
		"  " + "a" + sp(3) + "   " + sp(3) + "1" + "   " + "$1.000",
		"  " + "bb" + sp(2) + "   " + sp(2) + "22" + "   " + "$2.000",
		"  " + strings.Repeat("-", 20),
		"  " + "合计" + "   " + sp(2) + "23" + "   " + "$3.000",
	}, "\n") + "\n"
	got := tbl.Render()
	if got != want {
		t.Fatalf("Render() mismatch\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
	assertAllLinesWidth(t, got, 22)
}

func TestTableTotalRowWithoutRows(t *testing.T) {
	tbl := &Table{
		Columns:  []Column{{Title: "x", Align: AlignLeft}},
		TotalRow: []string{"合计"},
	}
	want := "  x   \n  ----\n  合计\n"
	if got := tbl.Render(); got != want {
		t.Fatalf("Render() = %q, want %q", got, want)
	}
}

func TestTableEmptyRows(t *testing.T) {
	tbl := &Table{Columns: []Column{{Title: "模型"}}}
	if got := tbl.Render(); got != "  (no data)\n" {
		t.Fatalf("Render() with empty rows = %q, want %q", got, "  (no data)\n")
	}
	empty := &Table{}
	if got := empty.Render(); got != "  (no data)\n" {
		t.Fatalf("Render() with no columns = %q, want %q", got, "  (no data)\n")
	}
}

func TestTableMissingCells(t *testing.T) {
	tbl := &Table{
		Columns: []Column{{Title: "名称"}, {Title: "数值", Align: AlignRight}},
		Rows:    [][]string{{"only"}},
	}
	want := "  名称   数值\n  only       \n"
	if got := tbl.Render(); got != want {
		t.Fatalf("Render() = %q, want %q", got, want)
	}
}

func TestTableColorDefaultStripsANSI(t *testing.T) {
	tbl := &Table{
		Columns: []Column{{Title: "名称", Align: AlignLeft}},
		Rows:    [][]string{{"\x1b[31m红\x1b[0m"}, {"green"}},
	}
	want := "  名称 \n  红   \n  green\n"
	got := tbl.Render()
	if got != want {
		t.Fatalf("Render() = %q, want %q", got, want)
	}
	if strings.ContainsRune(got, 0x1B) {
		t.Error("default output must not contain ANSI escapes")
	}
	assertAllLinesWidth(t, got, 7)
}

func TestTableColorEnabledKeepsANSIAndAligns(t *testing.T) {
	tbl := &Table{
		Columns: []Column{{Title: "名称", Align: AlignLeft}},
		Rows:    [][]string{{"\x1b[31m红\x1b[0m"}, {"green"}},
		Color:   true,
	}
	got := tbl.Render()
	if !strings.Contains(got, "\x1b[31m红\x1b[0m") {
		t.Fatalf("colored cell was stripped: %q", got)
	}
	assertAllLinesWidth(t, got, 7)
}

func TestTableCustomIndentGap(t *testing.T) {
	tbl := &Table{
		Columns: []Column{{Title: "a"}},
		Rows:    [][]string{{"b"}},
		Indent:  ">>",
		Gap:     "|",
	}
	want := ">>a\n>>b\n"
	if got := tbl.Render(); got != want {
		t.Fatalf("Render() = %q, want %q", got, want)
	}
}
