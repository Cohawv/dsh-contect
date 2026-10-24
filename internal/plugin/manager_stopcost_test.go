package plugin

import (
	"strings"
	"testing"
	"time"
)

// timedStopPlugin 的 Stop 固定耗时 d，用于验证停机耗时诊断。
//
// 不用现有的 slowStopPlugin：那个是「永远卡住」用来测硬超时的，
// 这里要的是「正常返回但慢」，两件事不能混。
type timedStopPlugin struct {
	meta Meta
	d    time.Duration
}

func (p *timedStopPlugin) Meta() Meta          { return p.meta }
func (p *timedStopPlugin) Init(*Context) error { return nil }
func (p *timedStopPlugin) Start() error        { return nil }
func (p *timedStopPlugin) Stop() error         { time.Sleep(p.d); return nil }

// TestStopCostsSortedSlowFirst 验证停机耗时被记录并按降序排列。
//
// 为什么要这个：停机慢通常只有一个元凶，但日志里原先只看到「停机花了 8 秒」，
// 不知道是谁。按耗时排序后一眼能定位，也让「某个插件的 Stop 悄悄变慢」
// 在版本之间变得可比较。
func TestStopCostsSortedSlowFirst(t *testing.T) {
	slow := &timedStopPlugin{meta: Meta{Name: "aslow", DefaultOn: true}, d: 60 * time.Millisecond}
	fast := &timedStopPlugin{meta: Meta{Name: "zfast", DefaultOn: true}, d: 1 * time.Millisecond}

	m := newManagerFrom([]Plugin{fast, slow})
	m.BuildContext(nil)
	m.InitAll()
	if _, err := m.StartAll(); err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	m.StopAll()

	costs := m.StopCosts()
	if len(costs) != 2 {
		t.Fatalf("记录到 %d 个插件耗时，期望 2", len(costs))
	}
	// 降序：慢的必须在前面，否则「一眼定位」就无从谈起。
	if costs[0].Name != "aslow" {
		t.Fatalf("首项 = %q，期望 aslow（应按耗时降序）", costs[0].Name)
	}
	if costs[0].Cost < 50*time.Millisecond {
		t.Fatalf("aslow 耗时 = %s，期望 >= 50ms（Stop 真睡了 60ms）", costs[0].Cost)
	}
	if costs[1].Cost >= costs[0].Cost {
		t.Fatalf("耗时未降序：%s >= %s", costs[1].Cost, costs[0].Cost)
	}
}

// TestSummaryIncludesStopCosts 验证停机摘要里能看到「谁最慢」。
func TestSummaryIncludesStopCosts(t *testing.T) {
	p := &timedStopPlugin{meta: Meta{Name: "zeta", DefaultOn: true}, d: 5 * time.Millisecond}
	m := newManagerFrom([]Plugin{p})
	m.BuildContext(nil)
	m.InitAll()
	if _, err := m.StartAll(); err != nil {
		t.Fatalf("StartAll: %v", err)
	}

	before := m.Summary()
	if strings.Contains(before, "停机耗时") {
		t.Fatal("尚未停机就不该出现耗时段")
	}

	m.StopAll()
	after := m.Summary()
	if !strings.Contains(after, "停机耗时") {
		t.Fatalf("停机摘要缺少耗时段：%s", after)
	}
	if !strings.Contains(after, "zeta=") {
		t.Fatalf("停机摘要未列出 zeta 的耗时：%s", after)
	}
}

// TestStopCostsEmptyBeforeStopAll 验证未停机时返回空（不 panic、不编造数字）。
func TestStopCostsEmptyBeforeStopAll(t *testing.T) {
	p := &timedStopPlugin{meta: Meta{Name: "n", DefaultOn: true}, d: time.Millisecond}
	m := newManagerFrom([]Plugin{p})
	if got := m.StopCosts(); len(got) != 0 {
		t.Fatalf("未停机时 StopCosts 应为空，实得 %d 项", len(got))
	}
}
