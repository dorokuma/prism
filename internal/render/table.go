package render

import "strings"

// Align controls horizontal alignment of a column's content.
type Align int

const (
	// AlignLeft left-aligns the column content.
	AlignLeft Align = iota
	// AlignRight right-aligns the column content.
	AlignRight
)

// Column describes one table column.
type Column struct {
	// Title is the header text.
	Title string
	// Align is the alignment applied to the header, the cells and the
	// total row.
	Align Align
	// MaxWidth caps the column width in display columns; 0 means
	// unlimited. Values wider than the cap are truncated with an
	// ellipsis, measured in display columns so multi-byte characters are
	// never cut in half.
	MaxWidth int
}

// Table renders rows into an aligned text table. Column widths are computed
// from DisplayWidth, so CJK, fullwidth and emoji content aligns correctly.
type Table struct {
	// Columns defines the table structure.
	Columns []Column
	// Rows holds one []string per line, one cell per column. Missing
	// cells render as empty; extra cells are ignored.
	Rows [][]string
	// TotalRow, when non-empty, renders an optional totals row below a
	// separator line that spans the full table width.
	TotalRow []string
	// Color, when true, keeps ANSI escape sequences found in cells
	// (alignment is still computed on the de-colored text, so colored
	// output stays aligned). When false, ANSI sequences are stripped so
	// piped output is plain text.
	Color bool
	// Indent is the left indent; empty defaults to two spaces.
	Indent string
	// Gap separates columns; empty defaults to three spaces.
	Gap string
}

const noDataLine = "(no data)"

// Render returns the whole table as a string ending with a newline. When
// there are no columns, or no rows and no total row, it returns a single
// friendly hint line instead of an empty table.
func (t *Table) Render() string {
	indent, gap := t.Indent, t.Gap
	if indent == "" {
		indent = "  "
	}
	if gap == "" {
		gap = "   "
	}
	if len(t.Columns) == 0 || (len(t.Rows) == 0 && len(t.TotalRow) == 0) {
		return indent + noDataLine + "\n"
	}

	n := len(t.Columns)
	rows := make([][]string, len(t.Rows))
	for i, r := range t.Rows {
		cells := make([]string, n)
		for j := 0; j < n && j < len(r); j++ {
			cells[j] = t.prep(r[j])
		}
		rows[i] = cells
	}
	total := make([]string, n)
	for j := 0; j < n && j < len(t.TotalRow); j++ {
		total[j] = t.prep(t.TotalRow[j])
	}

	widths := make([]int, n)
	for j := range t.Columns {
		w := DisplayWidth(t.prep(t.Columns[j].Title))
		for _, r := range rows {
			if dw := DisplayWidth(r[j]); dw > w {
				w = dw
			}
		}
		if dw := DisplayWidth(total[j]); dw > w {
			w = dw
		}
		if m := t.Columns[j].MaxWidth; m > 0 && w > m {
			w = m
		}
		widths[j] = w
	}

	var b strings.Builder
	writeRow := func(cells []string) {
		b.WriteString(indent)
		for j, c := range cells {
			if j > 0 {
				b.WriteString(gap)
			}
			b.WriteString(cell(c, t.Columns[j], widths[j]))
		}
		b.WriteByte('\n')
	}

	titles := make([]string, n)
	for j := range t.Columns {
		titles[j] = t.prep(t.Columns[j].Title)
	}
	writeRow(titles)
	for _, r := range rows {
		writeRow(r)
	}
	if len(t.TotalRow) > 0 {
		totalWidth := 0
		for _, w := range widths {
			totalWidth += w
		}
		totalWidth += DisplayWidth(gap) * (n - 1)
		b.WriteString(indent)
		b.WriteString(strings.Repeat("-", totalWidth))
		b.WriteByte('\n')
		writeRow(total)
	}
	return b.String()
}

// prep returns the cell text that will be rendered, stripping ANSI escapes
// unless color output is explicitly enabled.
func (t *Table) prep(s string) string {
	if t.Color {
		return s
	}
	return StripANSI(s)
}

// cell truncates s to the column width when needed and pads it to width
// according to the column alignment.
func cell(s string, col Column, width int) string {
	if DisplayWidth(s) > width {
		s = Truncate(s, width)
	}
	d := width - DisplayWidth(s)
	if d <= 0 {
		return s
	}
	if col.Align == AlignRight {
		return strings.Repeat(" ", d) + s
	}
	return s + strings.Repeat(" ", d)
}
