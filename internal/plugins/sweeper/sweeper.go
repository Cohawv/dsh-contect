// Package sweeper 承载 core.SweepLoop 的周期清理（心跳超时连接摘除）。
//
// 依赖 ② 的 core 改造：SweepLoop 现接受 context，可被插件在 Stop 时取消并干净退出。
// interval 可配（默认 30s，与历史行为一致）。
package sweeper

import (
	"context"
	"errors"
	"log"
	"time"

	"lan-agent-hub/internal/core"
	"lan-agent-hub/internal/plugin"
)

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
		Version:     "1.0.0",
		Description: "周期清理心跳超时的连接（core.SweepLoop）",
		DefaultOn:   true,
	}
}

// Init 取出内核与清理间隔（默认 30s）。
func (p *Sweeper) Init(ctx *plugin.Context) error {
	hub, ok := ctx.Hub().(*core.Hub)
	if !ok {
		return errors.New("sweeper: missing *core.Hub in context")
	}
	p.hub = hub
	p.interval = ctx.Duration("interval", 30*time.Second)
	return nil
}

// Start 起 goroutine 跑 SweepLoop（可被 cancel 退出）。
func (p *Sweeper) Start() error {
	if p.hub == nil {
		return errors.New("sweeper: hub not initialized")
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.done = make(chan struct{})
	go func() {
		defer close(p.done)
		p.hub.SweepLoop(ctx, p.interval)
	}()
	log.Printf("sweeper started (interval=%s)", p.interval)
	return nil
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

func init() {
	plugin.Register(&Sweeper{})
}
