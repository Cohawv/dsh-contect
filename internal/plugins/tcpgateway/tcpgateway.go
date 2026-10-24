// Package tcpgateway 是 TCP/NDJSON 长连接接入层插件。
//
// 它把传输连接适配为业务内核能理解的 core.Session（session 实现），是所有客户端的
// 连接入口（Meta.Required=true）。行为必须与历史 internal/adapter/tcp.go 逐字节一致，
// 以保证长连接链路（注册 → 收发 → 下线事件）在插件化后仍然完整。
package tcpgateway

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"lan-agent-hub/internal/core"
	"lan-agent-hub/internal/plugin"
	"lan-agent-hub/internal/protocol"
	"lan-agent-hub/internal/transport"
)

const sendBufferSize = 256

// 超时默认值（V8 ⑤）。
//
// 为什么必须有：没有截止时间时，一个「连上但不读」的客户端能让写协程永远阻塞在
// Write 上——那条协程连同它的批次缓冲一起钉死，而且因为它卡住，后续消息全部堆在
// sendCh 里直到被丢弃。一个「连上但不发」的客户端则让读协程永远阻塞在 Read 上，
// 在线表里留着一条实际已死的会话。两者都是外部单点就能触发的资源占用。
//
// 10 秒是「写入一段 12KB 的批次」的宽松上限：局域网里正常是一毫秒级，
// 留到 10 秒只是不误杀瞬时拥塞的慢链路。90 秒是读空闲上限：客户端正常会有注册、
// 心跳类消息，完全静默一分半基本可以判定死连接。
const (
	defaultWriteTimeout  = 10 * time.Second
	defaultReadIdleLimit = 90 * time.Second
)

// maxWriteBatch 是一次写出的最大帧数。
//
// 太小省不下系统调用；太大则单次 Write 的字节数上去了，尾延迟也会被拉长
// （一批里最慢的那条决定整批什么时候出去）。64 条 × 约 200B ≈ 12KB，
// 一次 Write 的量级合适，也不会让队头等太久。
const maxWriteBatch = 64

// TCPGateway 是 tcpgateway 插件实体。
// deliverStats 是下发侧的丢弃统计。
//
// 为什么必须记账：队列满时的丢弃是**静默**的——发送方以为发成功了，
// 接收方收不到，唯一的迹象是「连接正常、延迟很低、消息却少了」。
// 这类问题在 1000 连接下曾表现为 9% 的业务消息凭空消失，
// 而压测报告上一切正常。数字必须能被看见。
type deliverStats struct {
	dropped atomic.Int64 // 发送队列满，丢弃
	closed  atomic.Int64 // 连接已关闭后仍尝试下发
	// V8 ⑤：超时断开的计数。与丢弃分开记——「消息丢了」和「连接被掐了」
	// 是两种完全不同的故障，混在一个数里就再也分不清。
	writeTimeout atomic.Int64
	readTimeout  atomic.Int64
}

type TCPGateway struct {
	addr   string
	stats  deliverStats
	hub    *core.Hub
	ln     transport.Listener
	done   chan struct{}
	cancel context.CancelFunc
	// V8 ⑤ 超时配置（Init 时解析，session 建立时下发给每条连接）。
	writeTimeout  time.Duration
	readIdleLimit time.Duration
}

// Meta 声明插件元信息。
func (p *TCPGateway) Meta() plugin.Meta {
	return plugin.Meta{
		Name:           "tcpgateway",
		Version:        "1.1.0",
		Description:    "TCP/NDJSON 长连接接入层，是所有客户端的连接入口",
		DefaultOn:      true,
		Required:       true,
		RequiredReason: "它是连接入口，必须启用；若只需纯内核/HTTP 形态，请改用 -plugin 白名单并保留 tcpgateway",
	}
}

// Options 声明本插件的可配置项（控制台设置视图据此自动生成表单）。
// addr 需要重新绑定监听端口，故本插件不实现 Applier（改后标注 pendingRestart）。
//
// 两个超时项（V8 ⑤）是可配的：不同的部署对「多慢算死」的判断不一样，
// 写死一个值必然有人在错误的一侧。填 0 表示关闭该项保护（例如客户端确实长时间静默）。
func (p *TCPGateway) Options() []plugin.OptionDef {
	return []plugin.OptionDef{
		{
			Key:         "addr",
			Type:        plugin.OptionString,
			Default:     ":8080",
			Description: "TCP 监听地址，形如 :8080 或 0.0.0.0:8080（改动需重启生效）",
		},
		{
			Key:         "writeTimeout",
			Type:        plugin.OptionDuration,
			Default:     "10s",
			Description: "单次写出的截止时间（如 10s）；卡住的连接会被断开而非无限阻塞写协程。填 0 关闭",
		},
		{
			Key:         "readIdleTimeout",
			Type:        plugin.OptionDuration,
			Default:     "90s",
			Description: "读空闲上限（如 90s）：超过该时长没有任何上行数据即断开。填 0 关闭",
		},
	}
}

// Init 从 Context 取出内核、监听地址与两个超时配置。
func (p *TCPGateway) Init(ctx *plugin.Context) error {
	hub, ok := ctx.Hub().(*core.Hub)
	if !ok {
		return errors.New("tcpgateway: missing *core.Hub in context")
	}
	p.hub = hub
	p.addr = ctx.String("addr", ":8080")
	p.writeTimeout = ctx.Duration("writeTimeout", defaultWriteTimeout)
	p.readIdleLimit = ctx.Duration("readIdleTimeout", defaultReadIdleLimit)
	if p.writeTimeout < 0 {
		p.writeTimeout = 0
	}
	if p.readIdleLimit < 0 {
		p.readIdleLimit = 0
	}
	if p.writeTimeout > 0 {
		log.Printf("tcpgateway: 写超时 %s", p.writeTimeout)
	}
	if p.readIdleLimit > 0 {
		log.Printf("tcpgateway: 读空闲上限 %s", p.readIdleLimit)
	}
	return nil
}

// Start 监听 TCP 并启动 Accept 循环（阻塞于独立 goroutine）。
func (p *TCPGateway) Start() error {
	if p.hub == nil {
		return errors.New("tcpgateway: hub not initialized")
	}
	codec := protocol.JSONLine{}
	ln, err := transport.ListenTCP(p.addr, codec)
	if err != nil {
		return fmt.Errorf("tcpgateway listen %s: %w", p.addr, err)
	}
	p.ln = ln
	p.done = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	// 把丢弃统计挂到内核：metrics 插件据此暴露 hub_deliver_dropped_total。
	p.hub.SetDropStats(p.DropStats)
	go p.serve(ctx)
	log.Printf("LAN Agent Hub TCP listening on %s (NDJSON)", p.addr)
	return nil
}

// DropStats 返回（队列满丢弃数, 关闭后尝试下发数），供内核/指标层读取。
func (p *TCPGateway) DropStats() (dropped, closed int64) {
	return p.stats.dropped.Load(), p.stats.closed.Load()
}

// TimeoutStats 返回（写超时断开数, 读空闲断开数），供观测与测试读取。
//
// 单独暴露而不并进 DropStats：那个方法被 core.Hub.SetDropStats 的既有签名钉住，
// 往里加返回值会波及内核接口。超时计数目前是插件级观测项。
func (p *TCPGateway) TimeoutStats() (write, read int64) {
	return p.stats.writeTimeout.Load(), p.stats.readTimeout.Load()
}

// serve 接受连接并驱动读写循环。Stop 时关闭 listener 使 Accept 返回错误退出循环。
func (p *TCPGateway) serve(ctx context.Context) {
	defer close(p.done)
	for {
		conn, err := p.ln.Accept()
		if err != nil {
			// 关闭 listener（Stop）或外部 cancel 都会使 Accept 返回错误；
			// ctx 已取消说明是正常停机，静默退出；否则是真实故障（如 fd 耗尽），
			// 必须打日志——历史上这里是两个 return，把错误静默丢弃了。
			if ctx.Err() == nil {
				log.Printf("tcpgateway: accept on %s failed: %v", p.addr, err)
			}
			return
		}
		s := newSession(conn, &p.stats, p.writeTimeout, p.readIdleLimit)
		go s.writeLoop()
		go s.readLoop(p.hub)
	}
}

// Stop 关闭 listener 让 Accept 循环退出，并等待 serve 真正结束（最多 5s）。
func (p *TCPGateway) Stop() error {
	if p.cancel != nil {
		p.cancel()
	}
	if p.ln != nil {
		_ = p.ln.Close()
	}
	if p.done != nil {
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
			// 超时：记录但不阻塞整体停机
			log.Printf("tcpgateway: serve loop did not exit within 5s")
		}
	}
	return nil
}

func init() {
	plugin.Register(&TCPGateway{})
}

// ---------------------------------------------------------------------------
// 以下为 session 实现（原 adapter/tcp.go 的 TCP 会话逻辑，行为逐字节不变）。
// ---------------------------------------------------------------------------

// session 是 core.Session 的 TCP 实现。
// 发送走独立写协程 + 有缓冲信道：业务层 Deliver 永不阻塞，缓冲满则丢弃（背压策略）。
type session struct {
	id     string
	conn   transport.Conn
	sendCh chan protocol.Envelope
	closed chan struct{}
	once   sync.Once
	stats  *deliverStats
	// V8 ⑤：从插件下发的超时配置（0 表示关闭该项保护）。
	writeTimeout time.Duration
	readIdle     time.Duration
	// dc 是底层连接的截止时间能力；连接不支持时为 nil（退化为不设超时）。
	dc transport.DeadlineConn
}

func newSession(conn transport.Conn, stats *deliverStats, writeTimeout, readIdle time.Duration) *session {
	dc, _ := conn.(transport.DeadlineConn)
	return &session{
		conn:         conn,
		sendCh:       make(chan protocol.Envelope, sendBufferSize),
		closed:       make(chan struct{}),
		stats:        stats,
		writeTimeout: writeTimeout,
		readIdle:     readIdle,
		dc:           dc,
	}
}

// isTimeout 判断错误是否为「截止时间已到」（而非对端关闭/连接重置）。
//
// 必须区分：EOF 是客户端正常离开，读超时是客户端还连着但不再说话。
// 两者都导致断开，但前者不该刷日志、不该计入超时统计——否则正常下线的噪声
// 会淹没真正需要排查的卡死连接。
func isTimeout(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) {
		return ne.Timeout()
	}
	return false
}

func (s *session) ID() string { return s.id }

// Deliver 非阻塞下发；缓冲满则丢弃，连接已关闭时静默返回（不 panic）。
func (s *session) Deliver(env protocol.Envelope) {
	defer func() { _ = recover() }()
	// 先单独判一次「已关闭」：若把 closed 与「队列满」放在同一个 select 里，
	// 两者同时就绪时 Go 会随机挑一个，导致 closed/dropped 两个计数互相串味，
	// 排查时无法判断到底是「连接死了」还是「消费不过来」。
	select {
	case <-s.closed:
		if s.stats != nil {
			s.stats.closed.Add(1)
		}
		return
	default:
	}
	select {
	case s.sendCh <- env:
	case <-s.closed:
		if s.stats != nil {
			s.stats.closed.Add(1)
		}
	default:
		if s.stats != nil {
			s.stats.dropped.Add(1)
		}
	}
}

func (s *session) Close() error {
	s.once.Do(func() { close(s.closed) })
	return s.conn.Close()
}

func (s *session) writeLoop() {
	batch := make([]protocol.Envelope, 0, maxWriteBatch)
	for {
		select {
		case env := <-s.sendCh:
			batch = append(batch[:0], env)
			// 把队列里已有的尽量多取一些凑成一批：扇出场景下队列通常不止一条，
			// 一次写出能省掉大量系统调用。只在「队列已空」时停手，不会为了凑数而等。
		drain:
			for len(batch) < maxWriteBatch {
				select {
				case more := <-s.sendCh:
					batch = append(batch, more)
				default:
					break drain
				}
			}
			// 写前设截止时间（V8 ⑤）：客户端不读时 Write 会一直阻塞，
			// 没有 deadline 这条协程就再也回不来了。
			if s.dc != nil && s.writeTimeout > 0 {
				_ = s.dc.SetWriteDeadline(time.Now().Add(s.writeTimeout))
			}
			if err := s.sendBatch(batch); err != nil {
				if isTimeout(err) && s.stats != nil {
					s.stats.writeTimeout.Add(1)
					log.Printf("tcpgateway: 写超时（%s）断开连接 %s：对端 %.0f 秒内未读走数据",
						s.writeTimeout, s.id, s.writeTimeout.Seconds())
				}
				return
			}
			// 写成功后清掉截止时间：留着会让下一次写继承一个已过期的绝对时间，
			// 表现为「第一条消息正常、后续全部 Write timeout」。
			if s.dc != nil && s.writeTimeout > 0 {
				_ = s.dc.SetWriteDeadline(time.Time{})
			}
		case <-s.closed:
			return
		}
	}
}

// sendBatch 优先走连接的批量发送能力；未实现时逐条退化（行为与历史一致）。
func (s *session) sendBatch(envs []protocol.Envelope) error {
	if bc, ok := s.conn.(transport.BatchConn); ok {
		return bc.SendBatch(envs)
	}
	for _, e := range envs {
		if err := s.conn.Send(e); err != nil {
			return err
		}
	}
	return nil
}

// readLoop 读帧 → 交给业务内核；退出时摘除在线记录。
func (s *session) readLoop(hub *core.Hub) {
	defer func() {
		// 按会话身份卸载：同 id 重连时本连接已被新连接顶替，
		// 无条件按 id 删除会把新会话从在线表里摘掉（连接活着却收不到消息）。
		hub.UnregisterSession(s.id, s)
		_ = s.Close()
	}()
	for {
		// 每次读前重设截止时间（V8 ⑤）。必须每轮重设而不是设一次：
		// net.Conn 的 deadline 是**绝对时间**，一旦过期，后续所有 IO 都直接失败，
		// 连接就再也用不了了。
		if s.dc != nil && s.readIdle > 0 {
			_ = s.dc.SetReadDeadline(time.Now().Add(s.readIdle))
		}
		env, err := s.conn.Recv()
		if err != nil {
			if isTimeout(err) {
				// 读空闲超时：连接还在，但对端不再上行任何数据。
				// 协议没有服务端 ping（NDJSON 只有信封），所以这里直接断开，
				// 而不是先探活——留一条死会话占着在线表比误断更麻烦。
				if s.stats != nil {
					s.stats.readTimeout.Add(1)
				}
				log.Printf("tcpgateway: 读空闲超过 %s，断开连接 %q", s.readIdle, s.id)
				return
			}
			// 帧内容坏了是可恢复的：回执并继续，与历史行为一致
			if errors.Is(err, transport.ErrBadFrame) {
				s.Deliver(protocol.NewEnvelope(protocol.TypeSystem, "hub", s.id, "", map[string]any{
					"text":   "bad json: " + err.Error(), // 旧契约，保留
					"action": protocol.ActionError,
					"code":   protocol.CodeBadJSON,
				}))
				continue
			}
			return
		}
		if env.Type == protocol.TypeRegister {
			s.id = env.From
		}
		hub.HandleEnvelope(s, &env)
	}
}
