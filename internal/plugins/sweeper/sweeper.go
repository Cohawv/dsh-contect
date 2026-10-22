// Package sweeper 承载 core.SweepLoop 的周期清理（心跳超时连接摘除）。
//
// 依赖 ② 的 core 改造：SweepLoop 现接受 context，可被插件在 Stop 时取消并干净退出。
// interval 可配（默认 30s，与历史行为一致），且实现 plugin.Applier 支持运行期热更新。
package sweeper

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"lan-agent-hub/internal/core"
	"lan-agent-hub/internal/plugin"
)

// defaultInterval 是清理周期默认值（与历史行为一致）。
const defaultInterval = 30 * time.Second

// Sweeper 是 sweeper 插件实体。
type Sweeper struct {
	hub      *core.Hub
	interval time.Duration
	cancel   context.CancelFunc
	done     chan struct{}
}

// Meta 声明插件元信息。
func (p *Sweeper) Meta() plugin.Meta {
	return plugin.Meta{
		Name:        "sweeper",
		Version:     "1.1.0",
		Description: "周期清理心跳超时的连接（core.SweepLoop）",
		DefaultOn:   true,
	}
}

// Options 声明本插件的可配置项（控制台设置视图据此自动生成表单）。
func (p *Sweeper) Options() []plugin.OptionDef {
	return []plugin.OptionDef{
		{
			Key:         "interval",
			Type:        plugin.OptionDuration,
			Default:     defaultInterval.String(),
			Description: "心跳超时清理周期，支持热更新（示例 30s、1m）",
		},
	}
}

// Init 取出内核与清理间隔（默认 30s）。
func (p *Sweeper) Init(ctx *plugin.Context) error {
	hub, ok := ctx.Hub().(*core.Hub)
	if !ok {
		return errors.New("sweeper: missing *core.Hub in context")
	}
	p.hub = hub
	p.interval = ctx.Duration("interval", defaultInterval)
	if p.interval <= 0 {
		p.interval = defaultInterval
	}
	return nil
}

// Start 起 goroutine 跑 SweepLoop（可被 cancel 退出）。
func (p *Sweeper) Start() error {
	if p.hub == nil {
		return errors.New("sweeper: hub not initialized")
	}
	p.startLoop()
	log.Printf("sweeper started (interval=%s)", p.interval)
	return nil
}

// startLoop 启动清扫协程（调用方需保证没有已在运行的循环）。
func (p *Sweeper) startLoop() {
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.done = make(chan struct{})
	interval := p.interval
	go func(done chan struct{}) {
		defer close(done)
		p.hub.SweepLoop(ctx, interval)
	}(p.done)
}

// Stop 取消 SweepLoop 并等待退出（最多 5s）。
func (p *Sweeper) Stop() error {
	if p.cancel != nil {
		p.cancel()
	}
	if p.done != nil {
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
			log.Printf("sweeper: sweep loop did not exit within 5s")
		}
	}
	return nil
}

// Apply 实现 plugin.Applier：热更新 interval（重建清扫协程，无需重启进程）。
//
// 说明：SweepLoop 的周期是循环体内的固定值，无法在运行中修改，因此这里采取
// 「取消旧循环 → 用新周期起新循环」的方式，保证语义与重启一致且旧协程被干净回收。
func (p *Sweeper) Apply(options map[string]any) error {
	raw, ok := options["interval"]
	if !ok {
		return nil
	}
	s, ok := raw.(string)
	if !ok {
		return errors.New("sweeper: interval 选项不是字符串")
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return fmt.Errorf("sweeper: interval 选项不是合法正时长: %s", s)
	}
	if p.hub == nil {
		p.interval = d
		return nil
	}
	if p.cancel == nil {
		// 尚未启动：只记住新值，Start 时生效。
		p.interval = d
		return nil
	}
	if stopErr := p.Stop(); stopErr != nil {
		return stopErr
	}
	p.interval = d
	p.startLoop()
	log.Printf("sweeper: interval 已热更新为 %s", d)
	return nil
}

func init() {
	plugin.Register(&Sweeper{})
}
