// Package discovery 是对 internal/discovery.Service 的插件外壳。
//
// internal/discovery 是叶子包；本包只在外层加插件壳（Init 建 Service、
// 经共享 KV 交接给 httpgateway，Start/Stop 封装生命周期）。被禁用时共享 KV 无
// "discovery"，httpgateway 据此使 /api/discover 返回 404（保持现状行为）。
//
// V4：实现 plugin.Optioner（声明可配置项）与 plugin.Applier（interval 热更新），
// 让控制台的「设置」视图无需任何前端专属代码即可调整广播周期并立即生效。
package discovery

import (
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"lan-agent-hub/internal/core"
	"lan-agent-hub/internal/discovery"
	"lan-agent-hub/internal/plugin"
	"lan-agent-hub/internal/protocol"
)

// 默认配置值（与历史行为一致）。
const (
	defaultBindAddr = ":8082"
	defaultInterval = 5 * time.Second
)

// Discovery 是 discovery 插件实体。
type Discovery struct {
	hub      *core.Hub
	svc      *discovery.Service
	bindAddr string
	interval time.Duration
	name     string
}

// Meta 声明插件元信息。
func (p *Discovery) Meta() plugin.Meta {
	return plugin.Meta{
		Name:        "discovery",
		Version:     "1.1.0",
		Description: "局域网 UDP 自动发现（Beacon 广播 + 单播探测应答）",
		DefaultOn:   true,
	}
}

// Options 声明本插件的可配置项（控制台设置视图据此自动生成表单）。
func (p *Discovery) Options() []plugin.OptionDef {
	return []plugin.OptionDef{
		{
			Key:         "bindAddr",
			Type:        plugin.OptionString,
			Default:     defaultBindAddr,
			Description: "UDP 监听/广播地址，形如 :8082 或 0.0.0.0:8082；留空表示不启动发现服务",
		},
		{
			Key:         "hubName",
			Type:        plugin.OptionString,
			Default:     "",
			Description: "对外展示的 Hub 名称，留空则使用主机名",
		},
		{
			Key:         "noDiscovery",
			Type:        plugin.OptionBool,
			Default:     false,
			Description: "强制关闭局域网自动发现（等价于 bindAddr 留空）",
		},
		{
			Key:         "interval",
			Type:        plugin.OptionDuration,
			Default:     defaultInterval.String(),
			Description: "Beacon 广播周期，支持热更新（示例 5s、1m、500ms）",
		},
	}
}

// Init 从 Context 取出内核与配置；未禁用时创建 discovery.Service 并写入共享 KV。
func (p *Discovery) Init(ctx *plugin.Context) error {
	hub, ok := ctx.Hub().(*core.Hub)
	if !ok {
		return errors.New("discovery: missing *core.Hub in context")
	}
	p.hub = hub
	// 插件实例是进程级单例，先停掉上一次装配的残留（避免双 Init 泄漏 UDP socket，
	// 与 store/webhook 同款幂等修复——插件契约测试抓出）。
	if p.svc != nil {
		p.svc.Stop()
	}
	p.svc = nil
	p.bindAddr = ""

	bindAddr := ctx.String("bindAddr", defaultBindAddr)
	noDisc := ctx.Bool("noDiscovery", false)
	p.interval = ctx.Duration("interval", defaultInterval)
	if p.interval <= 0 {
		p.interval = defaultInterval
	}
	if noDisc || bindAddr == "" {
		// 显式关闭：不建 Service，共享 KV 无 "discovery" → /api/discover 返回 404。
		return nil
	}
	p.bindAddr = bindAddr

	name := ctx.String("hubName", "")
	if name == "" {
		name, _ = os.Hostname()
	}
	p.name = name

	svc := discovery.New(discovery.Options{
		BindAddr: bindAddr,
		TCPPort:  hub.TCPPort,
		HTTPPort: hub.HTTPPort,
		HubID:    name,
		Name:     name,
		Proto:    protocol.ProtoCurrent,
		Caps:     protocol.ServerCaps,
		Interval: p.interval,
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
	log.Printf("LAN auto-discovery on UDP %s (beacon every %s + query应答)", p.bindAddr, p.svc.Interval())
	return nil
}

// Stop 停止发现服务并释放 socket。
func (p *Discovery) Stop() error {
	if p.svc != nil {
		p.svc.Stop()
	}
	return nil
}

// Interval 返回当前**实际生效**的广播周期（读取底层 Service 的原子值）。
// 提供它是为了「热更新可观测」：运维/测试可从插件实例直接确认真实节奏，而不是只看配置文本。
func (p *Discovery) Interval() time.Duration {
	if p.svc != nil {
		return p.svc.Interval()
	}
	if p.interval > 0 {
		return p.interval
	}
	return defaultInterval
}

// Apply 实现 plugin.Applier：热更新配置项，无需重启。
// 只有 interval 能真正热更新（重建广播节奏）；bindAddr / hubName / noDiscovery 需要
// 重建 socket 与 Beacon，属于「重启生效」。因此当这些键与当前运行值不一致时返回 error，
// 由上层如实标注 pendingRestart（诚实优先于「看起来都生效了」）。
func (p *Discovery) Apply(options map[string]any) error {
	interval, hasInterval := options["interval"]
	if hasInterval {
		raw, ok := interval.(string)
		if !ok {
			return errors.New("discovery: interval 选项不是字符串")
		}
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			return errors.New("discovery: interval 选项不是合法正时长: " + raw)
		}
		if p.svc != nil {
			p.svc.SetInterval(d)
			log.Printf("discovery: interval 已热更新为 %s", d)
		}
		p.interval = d
	}

	// 非热更新项：与当前运行值不一致即视为「需要重启才生效」。
	var needRestart []string
	if v, ok := options["bindAddr"]; ok && fmt.Sprint(v) != p.bindAddr {
		needRestart = append(needRestart, "bindAddr")
	}
	if v, ok := options["hubName"]; ok {
		want := fmt.Sprint(v)
		if want == "" {
			want, _ = os.Hostname()
		}
		if want != p.name {
			needRestart = append(needRestart, "hubName")
		}
	}
	if v, ok := options["noDiscovery"]; ok {
		noDisc := p.svc == nil
		if b, isBool := v.(bool); isBool && b != noDisc {
			needRestart = append(needRestart, "noDiscovery")
		}
	}
	if len(needRestart) > 0 {
		return fmt.Errorf("discovery: %s 需要重启才生效", strings.Join(needRestart, "、"))
	}
	return nil
}

func init() {
	plugin.Register(&Discovery{})
}
