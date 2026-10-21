package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"time"
)

// 消息类型
const (
	TypeRegister   = "register"   // 注册：{from, content:{name}}
	TypeHeartbeat  = "heartbeat"  // 心跳保活
	TypeChat       = "chat"       // 点对点：{from,to,content}
	TypeRoom       = "room"       // 房间消息：{from,roomId,content}
	TypeBroadcast  = "broadcast"  // 全员广播：{from,content}
	TypeCreateRoom = "create_room"// 建房间：{from,content:{name,members}}
	TypeJoin       = "join"       // 进房：{from,content:{roomId}}
	TypeLeave      = "leave"      // 离房：{from,content:{roomId}}
	TypeList       = "list"       // 拉取在线列表
	TypeSystem     = "system"     // 系统/控制回执
	TypePresence   = "presence"   // 上下线事件
)

// Envelope 是统一的消息信封，所有接入形式共用同一结构
type Envelope struct {
	MsgID   string          `json:"msgId"`
	Type    string          `json:"type"`
	From    string          `json:"from"`
	To      string          `json:"to"`
	RoomID  string          `json:"roomId"`
	Content json.RawMessage `json:"content"`
	Seq     int64           `json:"seq"` // 服务端分配的单调递增序号
	TS      int64           `json:"ts"`
}

func uuid() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return time.Now().Format("150405.000")
	}
	return hex.EncodeToString(b)
}

func newEnvelope(typ, from, to, roomID string, content any) Envelope {
	var raw json.RawMessage
	if content != nil {
		if b, err := json.Marshal(content); err == nil {
			raw = b
		}
	}
	return Envelope{
		MsgID:   uuid(),
		Type:    typ,
		From:    from,
		To:      to,
		RoomID:  roomID,
		Content: raw,
		TS:      time.Now().UnixMilli(),
	}
}

func newSystem(to, msg string) Envelope {
	return newEnvelope(TypeSystem, "hub", to, "", map[string]string{"text": msg})
}
