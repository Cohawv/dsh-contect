// Package discovery 是对 internal/discovery.Service 的插件外壳。
//
// internal/discovery 是叶子包，**不改动**；本包只在外层加插件壳（Init 建 Service、
// 经共享 KV 交接给 httpgateway，Start/Stop 封装生命周期）。被禁用时共享 KV 无
// "discovery"，httpgateway 据此使 /api/discover 返回 404（保持现状行为）。
package discovery

import (
	"errors"
	"log"
	"os"

	"lan-agent-hub/internal/core"
	"lan-agent-hub/internal/discovery"
	"lan-agent-hub/internal/plugin"
	"lan-agent-hub/internal/protocol"
)

// Discovery 是 discovery 插件实体。
type Discovery struct {
	hub      *core.Hub
	svc      *discovery.Service
	bindAddr string
}

// Meta 声明插件元信息。
func (p *Discovery) Meta() plugin.Meta {
	return plugin.Meta{
		Name:        "discovery",
		Version:     "1.0.0",
		Description: "局域网 UDP 自动发现（Beacon 广播 + 单播探测应答）",
		DefaultOn:   true,
	}
}

// Init 从 Context 取出内核与配置；未禁用时创建 discovery.Service 并写入共享 KV。
func (p *Discovery) Init(ctx *plugin.Context) error {
	hub, ok := ctx.Hub().(*core.Hub)
	if !ok {
		return errors.New("discovery: missing *core.Hub in context")
	}
	p.hub = hub
	// 插件实例是进程级单例，先清掉上一次装配的残留（避免读到旧 Service）。
	p.svc = nil
	p.bindAddr = ""

	bindAddr := ctx.String("bindAddr", ":8082")
	noDisc := ctx.Bool("noDiscovery", false)
	if noDisc || bindAddr == "" {
		// 显式关闭：不建 Service，共享 KV 无 "discovery" → /api/discover 返回 404。
		return nil
	}
	p.bindAddr = bindAddr

	name := ctx.String("hubName", "")
	if name == "" {
		name, _ = os.Hostname()
	}

	svc := discovery.New(discovery.Options{
		BindAddr: bindAddr,
		TCPPort:  hub.TCPPort,
		HTTPPort: hub.HTTPPort,
		HubID:    name,
		Name:     name,
		Proto:    protocol.ProtoCurrent,
		Caps:     protocol.ServerCaps,
	})
	p.svc = svc
	ctx.SetShared("discovery", svc)
	return nil
}

// Start 启动发现服务；失败仅记日志、不阻断整体启动（与历史行为一致）。
func (p *Discovery) Start() error {
	if p.svc == nil {
		return nil
	}
	if err := p.svc.Start(); err != nil {
		log.Printf("discovery start failed (ignored): %v", err)
		p.svc = nil
		return nil
	}
	log.Printf("LAN auto-discovery on UDP %s (beacon every 5s + query应答)", p.bindAddr)
	return nil
}

// Stop 停止发现服务并释放 socket。
func (p *Discovery) Stop() error {
	if p.svc != nil {
		p.svc.Stop()
	}
	return nil
}

func init() {
	plugin.Register(&Discovery{})
}
