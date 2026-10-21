// Package discovery 实现局域网内的 Hub 自动发现。
//
// 机制（零第三方依赖，纯 UDP）：
//   - 服务端周期向广播地址（默认 255.255.255.255）发 Beacon（JSON），
//     携带 hubId / 实例 id / tcp+http 端口 / 协议版本 / 能力集；
//   - 客户端既可监听广播，也可主动发 Query（单播），服务端收到后立即回一个 Beacon，
//     无需等待下一个广播周期——这对「一上线就秒连」很关键；
//   - 客户端从 UDP 包源地址拿到 Hub 的 IP，结合 Beacon 的 tcpPort 完成连接。
//
// 依赖方向：本包不依赖任何内部包（叶子包），可被 adapter 装配使用，
// 但 core / transport / protocol 都不得反向依赖本包（由 scripts/check-deps.sh 强制）。
package discovery

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

const (
	// BeaconType / QueryType 是发现协议的信封 type，与 Hub 业务 Envelope 完全独立，
	// 避免与 TCP 长连接的 NDJSON 流混淆。
	BeaconType   = "_lanhub_beacon"
	QueryType    = "_lanhub_query"
	DefaultPort  = 8082
	broadcastIP  = "255.255.255.255"
	defaultIface = "0.0.0.0"
)

// Beacon 是服务端对外广播的「我是谁 / 在哪」信息。
type Beacon struct {
	Type       string   `json:"type"`       // 恒为 _lanhub_beacon
	HubID      string   `json:"hubId"`      // 集群/主机标识，稳定且人类可读
	InstanceID string   `json:"instanceId"` // 每次启动唯一，用于区分重启
	Name       string   `json:"name"`       // 展示名
	TCPPort    int      `json:"tcpPort"`    // Hub 长连接端口
	HTTPPort   int      `json:"httpPort"`   // REST 网关端口
	Proto      int      `json:"proto"`      // 服务端最高支持协议版本
	Caps       []string `json:"caps"`       // 服务端能力集
	TS         int64    `json:"ts"`         // 广播时间戳（毫秒）
}

// Options 构造 Service 的参数。
type Options struct {
	// BindAddr：监听与广播绑定的 UDP 地址，默认 ":8082"。
	BindAddr string
	// TCPPort / HTTPPort：要写入 Beacon 的 Hub 端口。
	TCPPort int
	HTTPPort int
	// HubID / Name / Proto / Caps：Beacon 内容，由 cmd/hub 从 protocol 包注入。
	HubID      string
	Name       string
	InstanceID string // 可选：每次启动唯一标识，不传则自动生成
	Proto      int
	Caps       []string
	// Interval：广播周期，默认 5s。
	Interval time.Duration
	// Targets：广播目标地址列表（"host:port"）。默认向 255.255.255.255:BindPort 发。
	// 单测中可注入 ["127.0.0.1:xxxx"] 避免真正打出去。
	Targets []string
}

// Service 是发现服务的句柄。
type Service struct {
	opts   Options
	beacon Beacon
	conn   *net.UDPConn
	stop   chan struct{}
	once   sync.Once
}

// New 构造 Service（不立即监听）。
func New(o Options) *Service {
	if o.BindAddr == "" {
		o.BindAddr = fmt.Sprintf("%s:%d", defaultIface, DefaultPort)
	}
	if o.Interval <= 0 {
		o.Interval = 5 * time.Second
	}
	if len(o.Targets) == 0 {
		o.Targets = []string{net.JoinHostPort(broadcastIP, portOf(o.BindAddr))}
	}
	if o.InstanceID == "" {
		o.InstanceID = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return &Service{
		opts: o,
		beacon: Beacon{
			Type:       BeaconType,
			HubID:      o.HubID,
			InstanceID: o.InstanceID,
			Name:       o.Name,
			TCPPort:    o.TCPPort,
			HTTPPort:   o.HTTPPort,
			Proto:      o.Proto,
			Caps:       o.Caps,
		},
		stop: make(chan struct{}),
	}
}

// Start 开始广播并监听 Query。
func (s *Service) Start() error {
	addr, err := net.ResolveUDPAddr("udp", s.opts.BindAddr)
	if err != nil {
		return err
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return err
	}
	// 注意：Go 的 *net.UDPConn 未暴露 SetBroadcast，向 255.255.255.255 写依赖
	// 底层 SO_BROADCAST。若系统禁止（常见于部分 sandbox），WriteToUDP 会返回
	// 错误并被 broadcastOnce 忽略——单播探测（Query 应答）不受影响，仍可发现。
	s.conn = conn
	go s.readLoop()
	go s.broadcastLoop()
	return nil
}

// Stop 停止广播与监听，释放 socket。
func (s *Service) Stop() {
	s.once.Do(func() { close(s.stop) })
	if s.conn != nil {
		_ = s.conn.Close()
	}
}

// Beacon 返回当前 Beacon（每次调用刷新 TS）。
func (s *Service) Beacon() Beacon {
	s.beacon.TS = time.Now().UnixMilli()
	return s.beacon
}

// BeaconJSON 返回可直发的 Beacon 字节。
func (s *Service) BeaconJSON() []byte {
	b, _ := json.Marshal(s.Beacon())
	return b
}

// HandleQuery 解析客户端查询，返回应回的 Beacon。非法的 query 返回 ok=false。
// 这是纯函数，便于单测：无需真正起 socket。
func (s *Service) HandleQuery(payload []byte) (Beacon, bool) {
	var q struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(payload, &q); err != nil {
		return Beacon{}, false
	}
	if q.Type != QueryType {
		return Beacon{}, false
	}
	return s.Beacon(), true
}

// broadcastLoop 周期广播。
func (s *Service) broadcastLoop() {
	ticker := time.NewTicker(s.opts.Interval)
	defer ticker.Stop()
	s.broadcastOnce()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			s.broadcastOnce()
		}
	}
}

func (s *Service) broadcastOnce() {
	b, err := json.Marshal(s.Beacon())
	if err != nil {
		return
	}
	for _, t := range s.opts.Targets {
		ua, err := net.ResolveUDPAddr("udp", t)
		if err != nil {
			continue
		}
		_, _ = s.conn.WriteToUDP(b, ua)
	}
}

// readLoop 监听单播 Query 并立即回应一个 Beacon（探测应答）。
func (s *Service) readLoop() {
	buf := make([]byte, 1024)
	for {
		select {
		case <-s.stop:
			return
		default:
		}
		_ = s.conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		n, from, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue // 周期超时只是为了让 stop 生效，不是错误
			}
			continue
		}
		if b, ok := s.HandleQuery(buf[:n]); ok {
			if resp, err := json.Marshal(b); err == nil {
				_, _ = s.conn.WriteToUDP(resp, from)
			}
		}
	}
}

// portOf 从 "host:port" 或 ":port" 中取出端口部分。
func portOf(addr string) string {
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[i+1:]
	}
	return fmt.Sprintf("%d", DefaultPort)
}
