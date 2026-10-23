package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"lan-agent-hub/internal/protocol"
)

// benchPayload 是压测消息的 content。
//
// 为什么不用普通 chat 文本：压测流量需要能自识别，否则回程（echo）会被当成
// 新消息再回一次，形成无限乒乓。bench 标记 + echo 标记让两侧都能一眼分清
// 「去程」与「回程」，且不影响 Hub —— Hub 只做路由，不解释 content。
type benchPayload struct {
	Text  string `json:"text"`
	Bench bool   `json:"bench"`
	Echo  bool   `json:"echo"`
	Nonce string `json:"nonce"` // 往返配对键（每条去程一个，回程原样带回）
	From  string `json:"from"`
}

// benchClient 是一个压测客户端：一条真实 TCP 长连接 + 真实协议信封。
//
// 刻意**不**复用 internal/transport 的 ndjsonConn：那个类型不导出、且是服务端
// 接受连接用的。这里按同样的分帧规则（NDJSON，每行一个信封）实现客户端侧，
// codec 仍复用 protocol.JSONLine —— 序列化契约只有一份。
type benchClient struct {
	id      string
	addr    string
	timeout time.Duration

	conn  net.Conn
	br    *bufio.Reader
	codec protocol.Codec

	wmu     sync.Mutex // 写串行化（Send 是「编帧 + 写」两步，不能交错）
	readMu  sync.Mutex // 读串行化（只有 reader goroutine 读，但 Close 期间要互斥）
	closed  atomic.Bool
	sentCnt atomic.Int64
	recvCnt atomic.Int64
}

func newBenchClient(addr, id string, timeout time.Duration) *benchClient {
	return &benchClient{id: id, addr: addr, timeout: timeout, codec: protocol.JSONLine{}}
}

// dialRegister 建连并完成注册握手（register → 等 action=registered 的系统回执）。
//
// 注册没成功就直接返回错误：一个没注册上的连接在压测里毫无意义，
// 把它算进「连接成功」只会让报告虚高。
func dialRegister(addr, id string, timeout time.Duration) (*benchClient, error) {
	c := newBenchClient(addr, id, timeout)
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	c.conn = conn
	c.br = bufio.NewReader(conn)

	reg := protocol.NewEnvelope(protocol.TypeRegister, id, "", "", map[string]string{"name": id})
	if err := c.send(reg); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("register send: %w", err)
	}
	// 等注册回执；期间可能先收到 presence 广播等其他帧，忽略即可。
	deadline := time.Now().Add(3 * timeout)
	for {
		if time.Now().After(deadline) {
			_ = c.Close()
			return nil, fmt.Errorf("register timeout（未收到 registered 回执）")
		}
		env, err := c.recv()
		if err != nil {
			_ = c.Close()
			return nil, fmt.Errorf("register recv: %w", err)
		}
		if isRegisteredAck(env) {
			return c, nil
		}
	}
}

// isRegisteredAck 判定注册回执。
//
// 判定同时接受结构化 action 与旧 text 前缀：协议过渡期两者并存，
// 压测工具若只认一种，未来某一侧调整就会让整个压测莫名其妙地失败。
func isRegisteredAck(env protocol.Envelope) bool {
	if env.Type != protocol.TypeSystem {
		return false
	}
	var m map[string]any
	if err := json.Unmarshal(env.Content, &m); err != nil {
		return false
	}
	if act, ok := m["action"].(string); ok && act == protocol.ActionRegistered {
		return true
	}
	if text, ok := m["text"].(string); ok {
		return len(text) >= 10 && text[:10] == "registered"
	}
	return false
}

// send 编帧并写出（带写超时）。写失败即视为连接不可用，交由调用方计数。
func (c *benchClient) send(env protocol.Envelope) error {
	b, err := c.codec.Encode(env)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closed.Load() {
		return fmt.Errorf("connection closed")
	}
	if err := c.conn.SetWriteDeadline(time.Now().Add(c.timeout)); err != nil {
		return err
	}
	if _, err := c.conn.Write(b); err != nil {
		return err
	}
	c.sentCnt.Add(1)
	return nil
}

// recv 读一帧（带读超时）。空行忽略，坏帧视为错误（压测不接受坏数据）。
func (c *benchClient) recv() (protocol.Envelope, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	if err := c.conn.SetReadDeadline(time.Now().Add(c.timeout)); err != nil {
		return protocol.Envelope{}, err
	}
	for {
		line, err := c.br.ReadBytes('\n')
		if err != nil {
			return protocol.Envelope{}, err
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		env, err := c.codec.Decode(line)
		if err != nil {
			return protocol.Envelope{}, fmt.Errorf("decode: %w", err)
		}
		c.recvCnt.Add(1)
		return env, nil
	}
}

// Close 幂等关闭连接。
func (c *benchClient) Close() error {
	if c.closed.Swap(true) {
		return nil
	}
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// payloadOf 从信封里取出压测负载；非压测消息返回 ok=false。
func payloadOf(env protocol.Envelope) (benchPayload, bool) {
	if len(env.Content) == 0 {
		return benchPayload{}, false
	}
	var p benchPayload
	if err := json.Unmarshal(env.Content, &p); err != nil {
		return benchPayload{}, false
	}
	if !p.Bench {
		return benchPayload{}, false
	}
	return p, true
}

// newBenchChat 构造一条压测去程/回程消息。
func newBenchChat(from, to, nonce string, echo bool) protocol.Envelope {
	return protocol.NewEnvelope(protocol.TypeChat, from, to, "", benchPayload{
		Text:  "bench",
		Bench: true,
		Echo:  echo,
		Nonce: nonce,
		From:  from,
	})
}

// newBenchRoom 构造一条压测房间消息（扇出场景）。
func newBenchRoom(from, roomID, nonce string) protocol.Envelope {
	return protocol.NewEnvelope(protocol.TypeRoom, from, "", roomID, benchPayload{
		Text:  "bench",
		Bench: true,
		Nonce: nonce,
		From:  from,
	})
}
