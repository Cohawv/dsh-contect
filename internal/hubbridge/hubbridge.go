// Package hubbridge 把 *core.Hub 适配成插件框架定义的两种能力：
// Publisher（发消息）与 HubView（只读快照）。
//
// 存在意义：框架（internal/plugin）按「不得 import core」的边界设计，
// 只以 any 持有 hub；Publisher/HubView 两个窄接口从 V1 起就写在框架里，
// 但生产装配（cmd/hub）从未调用 Manager.SetHub —— 两个能力一直是空实现。
// V6 ② group 插件是第一个真实消费方（设计文档要求分组路由「通过 Publisher 能力
// 发消息，不改 core」），本包即为该能力补齐生产接线，同时让后续插件
// （scheduler/webhook 等）开箱即用。
//
// 依赖方向：hubbridge → core + plugin（两者都是被依赖方，无环）。
// 本包不引入任何外部依赖，也不修改 core 的任何行为——只是读取与转发。
package hubbridge

import (
	"fmt"

	"lan-agent-hub/internal/core"
	"lan-agent-hub/internal/plugin"
	"lan-agent-hub/internal/protocol"
)

// Publisher 实现 plugin.Publisher。
//
// Send 直接走 core.Router.Route：seq 分配、消息落盘钩子（store）、
// msg.routed/msg.content 事件发布全部由内核热路径完成——插件发出的消息
// 与客户端发出的消息走完全相同的路由机器，不搞第二套投递逻辑。
// 仅当 env.To 非空且目标离线时返回 error（供调用方对账/重试）。
type Publisher struct {
	hub *core.Hub
}

// NewPublisher 构造 Publisher（hub 通常为 cmd/hub 装配期传入的 *core.Hub）。
func NewPublisher(hub *core.Hub) *Publisher {
	return &Publisher{hub: hub}
}

// Send 投递一条信封：To 非空走点对点，RoomID 非空走房间，都空走全广播。
func (p *Publisher) Send(env protocol.Envelope) error {
	if missed := p.hub.Router.Route(env); missed != "" {
		return fmt.Errorf("hubbridge: target %q offline", missed)
	}
	return nil
}

// Broadcast 以 from 身份全广播一条文本消息（便捷方法，等价于 Send 一条
// 无 To/RoomID 的 chat 信封）。
func (p *Publisher) Broadcast(from, text string) error {
	p.hub.Router.Route(protocol.NewEnvelope(protocol.TypeChat, from, "", "",
		map[string]any{"text": text}))
	return nil
}

// View 实现 plugin.HubView：把内核在线表/房间表/计数映射成纯值快照。
// 快照不含 Session 句柄——从类型上杜绝插件拿到会话后绕过路由私自下发。
type View struct {
	hub *core.Hub
}

// NewView 构造 View。
func NewView(hub *core.Hub) *View {
	return &View{hub: hub}
}

// Agents 返回在线 Agent 快照（含 caps/proto/lastSeen），按 ID 升序（Registry.All 已排序）。
func (v *View) Agents() []plugin.AgentInfo {
	all := v.hub.Registry.All()
	out := make([]plugin.AgentInfo, 0, len(all))
	for _, a := range all {
		out = append(out, plugin.AgentInfo{
			ID:       a.ID,
			Name:     a.Name,
			Caps:     a.Caps,
			Proto:    a.Proto,
			LastSeen: a.LastSeen,
		})
	}
	return out
}

// Rooms 返回房间快照。数据源是 RoomService.List（/api/rooms 的同款契约形状），
// 在此做一次类型化映射；键缺失时退化为零值，不 panic。
func (v *View) Rooms() []plugin.RoomInfo {
	list := v.hub.Rooms.List()
	out := make([]plugin.RoomInfo, 0, len(list))
	for _, m := range list {
		ri := plugin.RoomInfo{}
		if id, ok := m["id"].(string); ok {
			ri.ID = id
		}
		if name, ok := m["name"].(string); ok {
			ri.Name = name
		}
		if n, ok := m["memberCount"].(int); ok {
			ri.MemberCount = n
		}
		if members, ok := m["members"].([]string); ok {
			ri.Members = members
		}
		out = append(out, ri)
	}
	return out
}

// Stats 返回内核计数类统计。
func (v *View) Stats() plugin.HubStats {
	return plugin.HubStats{
		AgentCount:   len(v.hub.Registry.All()),
		RoomCount:    len(v.hub.Rooms.List()),
		ProtoVersion: protocol.ProtoCurrent,
	}
}
