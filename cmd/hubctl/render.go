package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

// 输出格式取值。
const (
	formatJSON  = "json"
	formatTable = "table"
)

// ANSI 颜色码（非 TTY 时整体不输出，见 renderer.paint）。
const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiDim    = "\x1b[2m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiCyan   = "\x1b[36m"
)

// renderer 负责把「值」变成人读的表格或机器读的 JSON。
// 输出目标、格式、是否着色全部可注入，因此单测可以断言字节级结果。
type renderer struct {
	w      io.Writer
	format string
	color  bool
}

func newRenderer(w io.Writer, format string, color bool) *renderer {
	return &renderer{w: w, format: format, color: color}
}

func (r *renderer) isJSON() bool { return r.format == formatJSON }

// json 输出缩进 JSON（保证 `| jq` 友好且可被 json.Unmarshal）。
func (r *renderer) json(v any) error {
	enc := json.NewEncoder(r.w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// table 输出带表头的对齐表格。
func (r *renderer) table(header []string, rows [][]string) error {
	tw := tabwriter.NewWriter(r.w, 0, 0, 2, ' ', 0)
	for i, h := range header {
		if i > 0 {
			_, _ = fmt.Fprint(tw, "\t")
		}
		_, _ = fmt.Fprint(tw, r.paint(ansiBold+ansiCyan, h))
	}
	_, _ = fmt.Fprint(tw, "\n")
	for _, row := range rows {
		for i, c := range row {
			if i > 0 {
				_, _ = fmt.Fprint(tw, "\t")
			}
			_, _ = fmt.Fprint(tw, c)
		}
		_, _ = fmt.Fprint(tw, "\n")
	}
	return tw.Flush()
}

// line 写一行（流式事件、提示信息等）。
func (r *renderer) line(s string) error {
	_, err := io.WriteString(r.w, s+"\n")
	return err
}

func (r *renderer) paint(code, s string) string {
	if !r.color {
		return s
	}
	return code + s + ansiReset
}

// levelColor 按日志级别着色：error 红、warn 黄、其余默认。
func (r *renderer) levelColor(level, s string) string {
	switch strings.ToLower(level) {
	case "error":
		return r.paint(ansiRed, s)
	case "warn", "warning":
		return r.paint(ansiYellow, s)
	default:
		return s
	}
}

// stateColor 按插件状态着色：running 绿、disabled 暗、其余黄（failed 等）。
func (r *renderer) stateColor(state, s string) string {
	switch strings.ToLower(state) {
	case "running", "enabled", "ok":
		return r.paint(ansiGreen, s)
	case "disabled", "stopped":
		return r.paint(ansiDim, s)
	case "failed":
		return r.paint(ansiRed, s)
	default:
		return r.paint(ansiYellow, s)
	}
}

// ---------------------------------------------------------------------------
// 环境探测（可注入，便于测试）
// ---------------------------------------------------------------------------

// isTerminal 判断 w 是否是字符设备（控制台/TTY）。
//
// 用 os.ModeCharDevice 而非 syscall ioctl：前者在 Windows 与 Unix 上都成立，
// 且不需要任何外部依赖（golang.org/x/term 属外部包，本项目禁止）。
// 非 *os.File（管道、bytes.Buffer、测试用的 &bytes.Buffer{}）一律判为非 TTY。
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// resolveFormat 决定输出格式：显式 -fmt 优先；否则非 TTY（管道/重定向）自动 json。
func resolveFormat(explicit string, term bool) string {
	switch strings.ToLower(strings.TrimSpace(explicit)) {
	case formatJSON:
		return formatJSON
	case formatTable:
		return formatTable
	}
	if !term {
		return formatJSON
	}
	return formatTable
}

// resolveColor 决定是否在输出里写 ANSI 颜色。
// 优先级：--color=never|always > NO_COLOR 环境变量 > 是否 TTY。
func resolveColor(mode string, term bool) bool {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "never", "false", "0":
		return false
	case "always", "true", "1":
		return true
	}
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	return term
}

// ---------------------------------------------------------------------------
// 单元格格式化小工具
// ---------------------------------------------------------------------------

// fmtTime 把毫秒时间戳渲染成人读的本地时间；0 或负值时返回 "-"。
func fmtTime(ms int64) string {
	if ms <= 0 {
		return "-"
	}
	return time.UnixMilli(ms).Local().Format("2006-01-02 15:04:05")
}

// fmtAge 相对 now 的年龄（秒/分/时/天），用于 lastSeen 这类"多久之前"的列。
func fmtAge(ms int64, now time.Time) string {
	if ms <= 0 {
		return "-"
	}
	d := now.Sub(time.UnixMilli(ms))
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return strconv.FormatInt(int64(d/time.Second), 10) + "s"
	case d < time.Hour:
		return strconv.FormatInt(int64(d/time.Minute), 10) + "m"
	case d < 24*time.Hour:
		return strconv.FormatInt(int64(d/time.Hour), 10) + "h"
	default:
		return strconv.FormatInt(int64(d/(24*time.Hour)), 10) + "d"
	}
}

// clockTime 渲染流式事件的行首时间戳（注入的 now 决定，测试可固定）。
func clockTime(now time.Time) string {
	return now.Local().Format("15:04:05")
}

// boolText 布尔值的人读文案。
func boolText(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// joinList 把字符串切片拼成单列文本（空则 "-"）。
func joinList(items []string) string {
	if len(items) == 0 {
		return "-"
	}
	sorted := append([]string(nil), items...)
	sort.Strings(sorted)
	return strings.Join(sorted, ",")
}

// oneLine 把任意文本压成单行（表格单元格不允许换行，否则列对齐会散架）。
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(strings.Join(strings.Fields(s), " "))
}
