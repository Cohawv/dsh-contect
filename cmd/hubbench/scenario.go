package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"lan-agent-hub/internal/protocol"
)

// pendingMap 记录「已发出、等待回程」的消息（nonce → 发出时刻）。
//
// 端到端延迟只能靠**往返**测（去程进 Hub、回程出 Hub）：每条去程发出时打一个
// nonce，回程带同一个 nonce 回来才能配对。定期清扫超时项，避免丢包时内存无界增长。
type pendingMap struct {
	mu sync.Mutex
	m  map[string]time.Time
}

func newPendingMap() *pendingMap { return &pendingMap{m: make(map[string]time.Time, 1024)} }

func (p *pendingMap) put(nonce string) {
	p.mu.Lock()
	p.m[nonce] = time.Now()
	p.mu.Unlock()
}

func (p *pendingMap) take(nonce string) (time.Time, bool) {
	p.mu.Lock()
	t, ok := p.m[nonce]
	if ok {
		delete(p.m, nonce)
	}
	p.mu.Unlock()
	return t, ok
}

// sweep 清扫超过 ttl 的等待项，返回数量（调用方计为超时错误）。
func (p *pendingMap) sweep(now time.Time, ttl time.Duration) int {
	p.mu.Lock()
	n := 0
	for k, t := range p.m {
		if now.Sub(t) > ttl {
			delete(p.m, k)
			n++
		}
	}
	p.mu.Unlock()
	return n
}

// endpoint 是一个压测端点（连接 + 角色 + 待配对表）。
type endpoint struct {
	c         *benchClient
	col       *collector
	pend      *pendingMap
	sender    bool // 是否主动发送
	echo      bool // 是否负责回程（回声方）
	peerCount int  // 可选的回声方数量（点对点目标轮转用）
}

// runChat 是点对点场景：发送方 → 回声方 → 回程，可测量往返延迟。
//
// 拓扑：一半连接当发送方，另一半当回声方。回声方收到一条压测去程就立刻原样
// 回一条（带同一 nonce），发送方据此算一次往返 —— 测的是「真实客户端收发一个
// 来回」的耗时，而不是进程内函数调用耗时。
func runChat(ctx context.Context, cfg *Config) (*Report, error) {
	senders, echoers := splitRoles(cfg.Connections)
	if echoers < 1 {
		return nil, usageErrorf("chat 场景至少需要 2 个连接（1 发送 + 1 回声），当前 -c=%d", cfg.Connections)
	}
	col := newCollector()
	var wg sync.WaitGroup
	clients := make([]*benchClient, 0, cfg.Connections)
	defer func() {
		for _, c := range clients {
			_ = c.Close()
		}
		wg.Wait()
	}()

	for i := 0; i < senders; i++ {
		c, err := cfg.connect(col, fmt.Sprintf("bench-s%03d", i))
		if err != nil {
			continue
		}
		clients = append(clients, c)
		e := &endpoint{c: c, col: col, pend: newPendingMap(), sender: true, peerCount: echoers}
		wg.Add(2)
		go func() { defer wg.Done(); e.readLoop(ctx) }()
		go func() { defer wg.Done(); e.sendLoop(ctx, cfg, "chat", "") }()
	}
	for i := 0; i < echoers; i++ {
		c, err := cfg.connect(col, fmt.Sprintf("bench-e%03d", i))
		if err != nil {
			continue
		}
		clients = append(clients, c)
		e := &endpoint{c: c, col: col, pend: newPendingMap(), echo: true}
		wg.Add(1)
		go func() { defer wg.Done(); e.readLoop(ctx) }()
	}

	if col.connEstablished.Load() == 0 {
		return nil, fmt.Errorf("没有任何连接建立成功，检查 -addr（%s）与服务端是否启动", cfg.Addr)
	}
	cfg.warmupAndRecord(col)
	<-ctx.Done()
	return cfg.report("chat", col, nil, nil), nil
}

// runRoom 是房间扇出场景：每条消息投递给房间内全部成员。
//
// 延迟测量：房间成员里第一个连接兼任回声方，收到别人的房间消息后向发送方回一条
// 点对点消息（携带同一 nonce），发送方据此算往返。这样「扇出」与「延迟」都能测，
// 且回程走普通点对点路由，不会把扇出放大成平方级流量。
func runRoom(ctx context.Context, cfg *Config) (*Report, error) {
	senders, members := splitRoles(cfg.Connections)
	if members < 1 {
		return nil, usageErrorf("room 场景至少需要 2 个连接（1 发送 + 1 回声成员），当前 -c=%d", cfg.Connections)
	}
	col := newCollector()
	ids := make([]string, 0, cfg.Connections)
	for i := 0; i < senders; i++ {
		ids = append(ids, fmt.Sprintf("bench-s%03d", i))
	}
	for i := 0; i < members; i++ {
		ids = append(ids, fmt.Sprintf("bench-e%03d", i))
	}

	var wg sync.WaitGroup
	clients := make([]*benchClient, 0, cfg.Connections)
	eps := make([]*endpoint, 0, cfg.Connections)
	defer func() {
		for _, c := range clients {
			_ = c.Close()
		}
		wg.Wait()
	}()
	for i, id := range ids {
		c, err := cfg.connect(col, id)
		if err != nil {
			continue
		}
		clients = append(clients, c)
		eps = append(eps, &endpoint{
			c:         c,
			col:       col,
			pend:      newPendingMap(),
			sender:    i < senders,
			echo:      i == senders, // 第一个房间成员兼任回声方
			peerCount: 1,
		})
	}
	if len(clients) == 0 {
		return nil, fmt.Errorf("没有任何连接建立成功，检查 -addr（%s）与服务端是否启动", cfg.Addr)
	}

	// 建房间放在起 reader 之前：此时只有本协程在读，是唯一能安全「请求-响应」的时刻。
	roomID, err := clients[0].createRoom(ctx, "bench-room", ids, cfg.Timeout)
	if err != nil {
		return nil, fmt.Errorf("建房间失败: %w", err)
	}

	for _, e := range eps {
		wg.Add(1)
		go func(ep *endpoint) { defer wg.Done(); ep.readLoop(ctx) }(e)
		if e.sender {
			wg.Add(1)
			go func(ep *endpoint) { defer wg.Done(); ep.sendLoop(ctx, cfg, "room", roomID) }(e)
		}
	}

	cfg.warmupAndRecord(col)
	<-ctx.Done()
	return cfg.report("room", col, nil, []string{
		fmt.Sprintf("房间 %s，成员 %d 个（扇出倍数 = 成员数）", roomID, len(clients)),
	}), nil
}

// runFlap 是连接抖动场景：持续上下线，验证 Hub 在注册/注销风暴下不漏 goroutine、
// 注册表最终一致。
//
// 判定：抖动结束后静置 500ms 再抓一次 Hub 指标，要求 agentsOnline 回落到 0
// （没有残留会话），且期间没有连接失败。
func runFlap(ctx context.Context, cfg *Config) (*Report, error) {
	col := newCollector()
	rate := cfg.FlapRate
	if rate <= 0 {
		rate = 20
	}
	interval := time.Second / time.Duration(rate)
	if interval <= 0 {
		interval = time.Millisecond
	}
	hold := 20 * time.Millisecond

	var wg sync.WaitGroup
	cfg.measureStart = time.Now() // flap 没有预热期：从第一秒就开始计时
	col.recording.Store(true)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	seq := 0
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			time.Sleep(500 * time.Millisecond) // 让服务端注销与清扫先跑完
			hub := cfg.scrape()
			notes := []string{fmt.Sprintf("抖动速率 %d 次/秒，单次保持 %s", rate, hold)}
			ok := true
			if hub != nil && hub.Scraped {
				notes = append(notes, fmt.Sprintf("抖动结束后 Hub 在线 Agent = %.0f（期望 0）", hub.AgentsOnline))
				if hub.AgentsOnline > 0 {
					ok = false
				}
			} else {
				notes = append(notes, "未采集 Hub 指标（缺 -http），注册表一致性未判定")
			}
			rep := cfg.report("flap", col, hub, notes)
			rep.OK = ok && col.connFailed.Load() == 0
			return rep, nil
		case <-ticker.C:
			seq++
			id := fmt.Sprintf("bench-f%06d", seq)
			wg.Add(1)
			go func() {
				defer wg.Done()
				c, err := cfg.connect(col, id)
				if err != nil {
					return
				}
				time.Sleep(hold)
				_ = c.Close()
			}()
		}
	}
}

// splitRoles 把连接数拆成「发送方 / 回声方」两半（至少各 1，除非总数不足）。
func splitRoles(total int) (senders, others int) {
	if total < 2 {
		return total, 0
	}
	senders = total / 2
	if senders < 1 {
		senders = 1
	}
	return senders, total - senders
}

// readLoop 读该连接上的所有帧，按角色处理：
//   - 回程（Echo=true）→ 配对发送时刻，记录一次往返延迟；
//   - 回声方收到别人的去程 → 立刻回一条；
//   - 其余非自己的消息 → 计入送达（房间扇出的每一份都算一次投递）。
func (e *endpoint) readLoop(ctx context.Context) {
	lastSweep := time.Now()
	for {
		if ctx.Err() != nil {
			return
		}
		env, err := e.c.recv()
		if err != nil {
			if ctx.Err() == nil {
				e.col.err(errRecv)
			}
			return
		}
		if time.Since(lastSweep) > 2*time.Second {
			lastSweep = time.Now()
			if n := e.pend.sweep(time.Now(), 5*time.Second); n > 0 {
				for i := 0; i < n; i++ {
					e.col.err(errTimeout)
				}
			}
		}
		p, ok := payloadOf(env)
		if !ok {
			continue
		}
		switch {
		case p.Echo:
			if t0, found := e.pend.take(p.Nonce); found {
				e.col.addLatency(time.Since(t0), e.c.id, p.Nonce)
				e.col.countDelivered()
			}
		case e.echo && p.From != e.c.id:
			if err := e.c.send(newBenchChat(e.c.id, p.From, p.Nonce, true)); err != nil {
				e.col.err(errSend)
			}
		case p.From != e.c.id:
			e.col.countDelivered()
		}
	}
}

// sendLoop 按配置速率持续发送；`-r 0` 表示不节流（跑极限吞吐）。
func (e *endpoint) sendLoop(ctx context.Context, cfg *Config, mode, roomID string) {
	var seq int64
	interval := time.Duration(0)
	if cfg.RatePerSec > 0 {
		interval = time.Duration(float64(time.Second) / cfg.RatePerSec)
	}
	if interval <= 0 {
		interval = time.Microsecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			seq++
			nonce := fmt.Sprintf("%s#%d", e.c.id, seq)
			e.pend.put(nonce)
			var env protocol.Envelope
			if mode == "room" {
				env = newBenchRoom(e.c.id, roomID, nonce)
			} else {
				env = newBenchChat(e.c.id, e.pickPeer(), nonce, false)
			}
			if err := e.c.send(env); err != nil {
				e.col.err(errSend)
				continue
			}
			e.col.countSent()
		}
	}
}

// pickPeer 按发送方 id 的数字后缀对回声方数量取模选目标。
// 用**确定性**轮转而非随机：同一组参数下两次压测打到完全相同的连接上，报告才可比。
func (e *endpoint) pickPeer() string {
	n := 0
	for i := 0; i < len(e.c.id); i++ {
		if e.c.id[i] >= '0' && e.c.id[i] <= '9' {
			n = n*10 + int(e.c.id[i]-'0')
		}
	}
	if e.peerCount <= 0 {
		return "bench-e000"
	}
	return fmt.Sprintf("bench-e%03d", n%e.peerCount)
}

// createRoom 发一条 create_room 并等系统回执拿到 roomId。
// 仅可在**没有其它协程读这条连接**时调用（握手期）。
func (c *benchClient) createRoom(ctx context.Context, name string, members []string, timeout time.Duration) (string, error) {
	env := protocol.NewEnvelope(protocol.TypeCreateRoom, c.id, "", "", map[string]any{
		"name": name, "members": members,
	})
	if err := c.send(env); err != nil {
		return "", err
	}
	deadline := time.Now().Add(3 * timeout)
	for {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("等待 create_room 回执超时")
		}
		got, err := c.recv()
		if err != nil {
			return "", err
		}
		if got.Type != protocol.TypeSystem {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(got.Content, &m); err != nil {
			continue
		}
		if act, _ := m["action"].(string); act == protocol.ActionCreated {
			if id, _ := m["roomId"].(string); id != "" {
				return id, nil
			}
		}
	}
}
