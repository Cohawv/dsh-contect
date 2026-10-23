// Package protocol 定义 Hub 与所有客户端之间的对外契约。
//
// 这是整个系统里唯一允许长期冻结的包：它不依赖任何内部包（叶子包），
// 任何字段的新增都必须遵守「只增不改」规则，以保障老客户端无需同步升级。
package protocol

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"time"
)

// 消息类型（type 字段取值）
const (
	TypeRegister   = "register"    // 注册：{from, content:{name}}
	TypeHeartbeat  = "heartbeat"   // 心跳保活
	TypeChat       = "chat"        // 点对点：{from,to,content}
	TypeRoom       = "room"        // 房间消息：{from,roomId,content}
	TypeBroadcast  = "broadcast"   // 全员广播：{from,content}
	TypeCreateRoom = "create_room" // 建房间：{from,content:{name,members}}
	TypeJoin       = "join"        // 进房：{from,content:{roomId}}
	TypeLeave      = "leave"       // 离房：{from,content:{roomId}}
	TypeList       = "list"        // 拉取在线列表
	TypeSystem     = "system"      // 系统/控制回执
	TypePresence   = "presence"    // 上下线事件
	TypeAck        = "ack"         // 投递确认回执（V6 ②：接收方确认收到消息；仅声明 ack cap 的会话参与）
)

// Envelope 是统一的消息信封，所有接入形式（TCP / HTTP / MCP / DSH）共用同一结构。
//
// 字段演进铁律：
//  1. 只增不改——已存在的字段名、类型、语义不得变更；
//  2. 老客户端忽略未知字段（JSON 天然宽容），因此新增字段是安全的；
//  3. 会影响客户端行为的新字段，必须受协议版本或能力开关门控。
type Envelope struct {
	MsgID   string          `json:"msgId"`
	V       int             `json:"v,omitempty"` // 协议版本；缺省（0）按 ProtoLegacy 处理
	Type    string          `json:"type"`
	From    string          `json:"from"`
	To      string          `json:"to"`
	RoomID  string          `json:"roomId"`
	Content json.RawMessage `json:"content"`
	Seq     int64           `json:"seq"` // 服务端分配的单调递增序号（仅经 Route 的消息有值）
	TS      int64           `json:"ts"`
}

// UUID 生成信封唯一标识。
func UUID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return time.Now().Format("150405.000")
	}
	return hex.EncodeToString(b)
}

// NewEnvelope 构造信封，content 为 nil 时 content 字段省略。
func NewEnvelope(typ, from, to, roomID string, content any) Envelope {
	var raw json.RawMessage
	if content != nil {
		if b, err := json.Marshal(content); err == nil {
			raw = b
		}
	}
	return Envelope{
		MsgID:   UUID(),
		Type:    typ,
		From:    from,
		To:      to,
		RoomID:  roomID,
		Content: raw,
		TS:      time.Now().UnixMilli(),
	}
}

// NewSystem 构造一条系统回执。
// 注意：兼容期要求同时输出结构化 action 与旧 text，见 NewSystemAck。
func NewSystem(to, msg string) Envelope {
	return NewEnvelope(TypeSystem, "hub", to, "", map[string]string{"text": msg})
}

// UnmarshalContent 把信封负载解析到 v；负载为空时不做任何事（保持 v 的零值）。
func UnmarshalContent(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, v)
}
