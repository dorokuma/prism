package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/usage"
)

// usagePresets are the preset subcommands of `prism usage`; the empty string
// is the bare `prism usage` default. Each preset fixes the group_by list
// (and optionally the failed filter); --by and --failed override them.
var usagePresets = map[string]struct {
	groupBy []string
	failed  bool
}{
	"":          {groupBy: []string{"model"}},
	"models":    {groupBy: []string{"model"}},
	"keys":      {groupBy: []string{"key_id"}},
	"accounts":  {groupBy: []string{"account"}},
	"providers": {groupBy: []string{"provider"}},
	"days":      {groupBy: []string{"day"}},
	"hours":     {groupBy: []string{"hour"}},
	"errors":    {groupBy: []string{"model"}, failed: true},
}

// usageOptions carries the parsed flag values for `prism usage`.
type usageOptions struct {
	preset   string
	since    string
	until    string
	by       string
	model    string
	key      string
	account  string
	provider string
	failed   bool
	limit    int
	json     bool
	watch    string
	db       string
	noColor  bool
}

// defaultUsageDBPath is used when neither --db nor the config yield a path.
const defaultUsageDBPath = "/var/lib/prism/usage.db"

// runUsage implements `prism usage [preset] [flags]`. main dispatches to it
// the same way it dispatches `prism setup`: a hardcoded os.Args[1] branch
// before any config load, so the command works even when the prism service
// is not running. The database is read directly through a read-only
// connection (mode=ro, WAL), so it is safe to run while the service is
// actively writing.
func runUsage(args []string) error {
	return runUsageWith(args, os.Stdout, time.Now())
}

// runUsageWith is runUsage with injectable output and clock; the CLI entry
// uses os.Stdout / time.Now, tests use a buffer and a fixed time.
func runUsageWith(args []string, out io.Writer, now time.Time) error {
	o, q, dbPath, err := parseUsageArgs(args, now)
	if err != nil {
		// -h/--help: the flag package already printed the usage; treat it as
		// a successful invocation, not an error.
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	// Friendly hint instead of a raw Go error when usage was never enabled:
	// a missing file is the normal first-run state, not a crash.
	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("usage 数据库不存在: %s\n该功能尚未开启——请在 config.yaml 中把 usage.enabled 设为 true 并重启 prism 服务（或使用 --db 指定已有数据库）", dbPath)
		}
		return fmt.Errorf("无法访问 usage 数据库 %s: %v", dbPath, err)
	}

	store := usage.NewReadOnlyStore(dbPath)
	if err := store.Open(); err != nil {
		return fmt.Errorf("无法以只读方式打开 usage 数据库 %s: %v", dbPath, err)
	}
	defer store.Close()

	period := usage.DescribePeriod(q.From, q.To, now.Unix())
	render := func() error {
		ov, err := store.Overview(context.Background(), q)
		if err != nil {
			return fmt.Errorf("usage 汇总查询失败: %v", err)
		}
		rows, err := store.Summary(context.Background(), q)
		if err != nil {
			var qe *usage.QueryError
			if errors.As(err, &qe) {
				return fmt.Errorf("%s", qe.Msg)
			}
			return fmt.Errorf("usage 分组查询失败: %v", err)
		}
		if rows == nil {
			rows = []usage.SummaryRow{}
		}
		if o.json {
			return writeUsageJSON(out, q, period, ov, rows)
		}
		color := wantColor(out, o.noColor)
		_, err = io.WriteString(out, usage.RenderUsageReport(ov, rows, q.GroupBy, usage.ReportOptions{
			Period: period,
			Color:  color,
		}))
		return err
	}

	if o.watch == "" {
		return render()
	}
	iv, err := time.ParseDuration(o.watch)
	if err != nil || iv <= 0 {
		return fmt.Errorf("无效的 --watch 间隔 %q（如 5s、1m）", o.watch)
	}
	return watchLoop(render, out, iv, nil)
}

// parseUsageArgs parses the preset and flags into a SummaryQuery, resolving
// times, the group_by list and the database path. It is separated from
// runUsageWith so tests can drive it directly.
func parseUsageArgs(args []string, now time.Time) (usageOptions, usage.SummaryQuery, string, error) {
	var o usageOptions
	preset := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		preset = args[0]
		args = args[1:]
	}
	o.preset = preset

	fs := flag.NewFlagSet("usage", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&o.since, "since", "", "起始时间：24h/7d/30m 相对写法、08-01 月日、2026-08-01 完整日期（默认今天零点）")
	fs.StringVar(&o.until, "until", "", "结束时间，格式同上（默认现在）")
	fs.StringVar(&o.by, "by", "", "覆盖分组，逗号分隔：model/provider/account/key_id/stream/success/hour/day")
	fs.StringVar(&o.model, "model", "", "只统计指定模型")
	fs.StringVar(&o.key, "key", "", "只统计指定调用方 key_id")
	fs.StringVar(&o.account, "account", "", "只统计指定上游账号")
	fs.StringVar(&o.provider, "provider", "", "只统计指定 provider")
	fs.BoolVar(&o.failed, "failed", false, "只看失败请求")
	fs.IntVar(&o.limit, "limit", 20, "明细最多显示行数（上限 1000）")
	fs.BoolVar(&o.json, "json", false, "输出原始 JSON 而不是表格")
	fs.StringVar(&o.watch, "watch", "", "定时重绘间隔，如 5s、1m（清屏后重画）")
	fs.StringVar(&o.db, "db", "", "数据库路径（默认从配置读，读不到用 /var/lib/prism/usage.db）")
	fs.BoolVar(&o.noColor, "no-color", false, "强制关闭颜色")
	fs.Usage = func() { printUsageHelp(fs) }
	if err := fs.Parse(args); err != nil {
		return o, usage.SummaryQuery{}, "", err
	}
	if fs.NArg() > 0 {
		return o, usage.SummaryQuery{}, "", fmt.Errorf("无法识别的参数 %q（preset 请放在 flags 之前，如: prism usage models --since 7d）", fs.Arg(0))
	}
	spec, ok := usagePresets[preset]
	if !ok {
		return o, usage.SummaryQuery{}, "", fmt.Errorf("未知的 usage 子命令 %q，支持: models / keys / accounts / providers / days / hours / errors", preset)
	}
	groupBy := spec.groupBy
	if o.by != "" {
		groupBy = nil
		for _, g := range strings.Split(o.by, ",") {
			g = strings.TrimSpace(g)
			if g == "" {
				continue
			}
			if !usage.ValidGroupBy(g) {
				return o, usage.SummaryQuery{}, "", fmt.Errorf("无效的 --by 分组 %q，可用: model/provider/account/key_id/stream/success/hour/day", g)
			}
			groupBy = append(groupBy, g)
		}
		if len(groupBy) == 0 {
			return o, usage.SummaryQuery{}, "", fmt.Errorf("--by 为空")
		}
	}
	if o.limit < 1 {
		return o, usage.SummaryQuery{}, "", fmt.Errorf("--limit 必须大于 0")
	}
	// Values above 1000 are clamped to the usage package cap, matching the
	// HTTP path: Summary itself clamps limit > summaryMaxLimit to 1000.
	if o.limit > 1000 {
		o.limit = 1000
	}
	if o.json && o.watch != "" {
		return o, usage.SummaryQuery{}, "", fmt.Errorf("--json 与 --watch 不能同时使用")
	}
	if o.watch != "" {
		if iv, err := time.ParseDuration(o.watch); err != nil || iv <= 0 {
			return o, usage.SummaryQuery{}, "", fmt.Errorf("无效的 --watch 间隔 %q（如 5s、1m）", o.watch)
		}
	}

	// Time range. Days go through AddDate (calendar arithmetic), hours and
	// minutes through Add, so month/year boundaries are handled by the time
	// package instead of hand-rolled seconds math.
	loc := now.Location()
	from := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc) // 默认今天零点
	if o.since != "" {
		var err error
		from, err = parseTimeArg(o.since, now)
		if err != nil {
			return o, usage.SummaryQuery{}, "", err
		}
	}
	to := now
	if o.until != "" {
		var err error
		to, err = parseTimeArg(o.until, now)
		if err != nil {
			return o, usage.SummaryQuery{}, "", err
		}
	}
	if from.After(to) {
		return o, usage.SummaryQuery{}, "", fmt.Errorf("--since (%s) 晚于 --until (%s)", formatTimeArg(from), formatTimeArg(to))
	}

	var failed *bool
	if o.failed || spec.failed {
		f := false
		failed = &f
	}
	q := usage.SummaryQuery{
		From:     from.Unix(),
		To:       to.Unix(),
		GroupBy:  groupBy,
		Model:    o.model,
		Provider: o.provider,
		Account:  o.account,
		KeyID:    o.key,
		Success:  failed,
		Limit:    o.limit,
	}

	dbPath := o.db
	if dbPath == "" {
		if cfg, err := config.LoadConfig("config.yaml"); err == nil && cfg.Usage.DBPath != "" {
			dbPath = cfg.Usage.DBPath
		} else {
			dbPath = defaultUsageDBPath
		}
	}
	return o, q, dbPath, nil
}

// writeUsageJSON emits the raw report as JSON. The output contains ONLY the
// JSON document (no table characters, no summary lines), so it can be piped
// straight into jq or another tool.
func writeUsageJSON(out io.Writer, q usage.SummaryQuery, period string, ov *usage.Overview, rows []usage.SummaryRow) error {
	doc := struct {
		Period   string             `json:"period"`
		From     int64              `json:"from"`
		To       int64              `json:"to"`
		GroupBy  []string           `json:"group_by"`
		Overview *usage.Overview    `json:"overview"`
		Rows     []usage.SummaryRow `json:"rows"`
	}{
		Period:   period,
		From:     q.From,
		To:       q.To,
		GroupBy:  q.GroupBy,
		Overview: ov,
		Rows:     rows,
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	_, err = out.Write(append(b, '\n'))
	return err
}

// relTimeArg matches the relative time forms: "30m", "24h", "7d", "90s".
var relTimeArg = regexp.MustCompile(`^(\d+)([smhd])$`)

// parseTimeArg parses a --since/--until value against now:
//
//   - relative: "30m" / "24h" / "7d" — subtracted from now. Days use
//     AddDate (calendar arithmetic), so "7d" from March 3 lands on
//     February 24 and "30d" in January lands in December of the previous
//     year; hours/minutes/seconds use Add. No hand-rolled seconds math.
//   - month-day: "08-01" — the current year; when the date is still in the
//     future it falls back to the previous year.
//   - full date: "2026-08-01" — midnight local time.
func parseTimeArg(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if m := relTimeArg.FindStringSubmatch(s); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return time.Time{}, fmt.Errorf("无法解析时间 %q", s)
		}
		switch m[2] {
		case "d":
			return now.AddDate(0, 0, -n), nil
		case "h":
			return now.Add(-time.Duration(n) * time.Hour), nil
		case "m":
			return now.Add(-time.Duration(n) * time.Minute), nil
		default: // "s"
			return now.Add(-time.Duration(n) * time.Second), nil
		}
	}
	if t, err := time.ParseInLocation("2006-01-02", s, now.Location()); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("01-02", s, now.Location()); err == nil {
		t = time.Date(now.Year(), t.Month(), t.Day(), 0, 0, 0, 0, now.Location())
		if t.After(now) {
			t = t.AddDate(-1, 0, 0)
		}
		return t, nil
	}
	return time.Time{}, fmt.Errorf("无法解析时间 %q（支持 30m/24h/7d 相对写法、08-01 月日、2026-08-01 完整日期）", s)
}

// formatTimeArg renders a resolved time for error messages.
func formatTimeArg(t time.Time) string {
	return t.Format("2006-01-02 15:04")
}

// watchLoop re-renders every interval. The first paint does not clear the
// screen; every redraw afterwards clears it first ("清屏后重画"). It returns
// when stop is closed (nil stop means run forever). The interval and the
// output target are injectable so tests never wait real seconds.
func watchLoop(render func() error, out io.Writer, interval time.Duration, stop <-chan struct{}) error {
	const clearScreen = "\x1b[2J\x1b[H"
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	first := true
	for {
		if stop != nil {
			select {
			case <-stop:
				return nil
			default:
			}
		}
		if !first {
			if _, err := io.WriteString(out, clearScreen); err != nil {
				return err
			}
		}
		if err := render(); err != nil {
			return err
		}
		first = false
		select {
		case <-ticker.C:
		case <-stop:
			return nil
		}
	}
}

// wantColor decides whether the report gets ANSI colors: on by default only
// when out is a terminal character device (os.Stdout on a TTY), off for
// pipes/redirects, and --no-color forces it off. Detection uses
// ModeCharDevice from os.Stdout.Stat() — no third-party dependency.
func wantColor(out io.Writer, noColor bool) bool {
	if noColor {
		return false
	}
	f, ok := out.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// printUsageHelp writes the usage command help.
func printUsageHelp(fs *flag.FlagSet) {
	fmt.Fprintf(fs.Output(), `用法: prism usage [preset] [flags]

统计 token 用量。直接以只读方式读取 usage 数据库（WAL + mode=ro），
prism 服务未运行也能查询历史，服务运行中查询不干扰写入。

preset（预设分组，不用记 group_by）:
  prism usage            默认：今天，按模型分组
  prism usage models     按模型
  prism usage keys       按调用方 key_id
  prism usage accounts   按上游账号
  prism usage providers  按 provider
  prism usage days       按天
  prism usage hours      按小时
  prism usage errors     只看失败请求（等价 success=false，按模型分组）

flags:
`)
	fs.PrintDefaults()
}
