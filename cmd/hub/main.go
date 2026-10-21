package main

import (
	"flag"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"lan-agent-hub/internal/adapter"
	"lan-agent-hub/internal/core"
	"lan-agent-hub/internal/discovery"
	"lan-agent-hub/internal/protocol"
	"lan-agent-hub/internal/transport"
)

// main 只做装配：选传输实现 → 注入 codec → 构造业务内核 → 起接入层与发现服务。
// 换传输（如改 WebSocket）只需替换这里的 transport 实现，其余代码不动。
func main() {
	addr := flag.String("addr", ":8080", "hub TCP listen address, e.g. :8080 or 0.0.0.0:8080")
	httpAddr := flag.String("http", ":8081", "hub HTTP gateway address (REST), e.g. :8081")
	discoverAddr := flag.String("discover", ":8082", "UDP discovery bind address; set empty to disable")
	noDiscovery := flag.Bool("no-discovery", false, "disable LAN auto-discovery beacon")
	hubName := flag.String("name", "", "hub display name (default: hostname)")
	flag.Parse()

	codec := protocol.JSONLine{}
	hub := core.NewHub()
	hub.TCPPort, hub.HTTPPort = portOf(*addr), portOf(*httpAddr)

	ln, err := transport.ListenTCP(*addr, codec)
	if err != nil {
		log.Fatalf("listen %s failed: %v", *addr, err)
	}
	log.Printf("LAN Agent Hub TCP listening on %s (NDJSON)", *addr)

	go hub.SweepLoop(30 * time.Second)

	// 局域网自动发现（可选）
	var disco *discovery.Service
	if !*noDiscovery && *discoverAddr != "" {
		name := *hubName
		if name == "" {
			name, _ = os.Hostname()
		}
		disco = discovery.New(discovery.Options{
			BindAddr: *discoverAddr,
			TCPPort:  hub.TCPPort,
			HTTPPort: hub.HTTPPort,
			HubID:    name,
			Name:     name,
			Proto:    protocol.ProtoCurrent,
			Caps:     protocol.ServerCaps,
		})
		if err := disco.Start(); err != nil {
			log.Printf("discovery start failed (ignored): %v", err)
			disco = nil
		} else {
			log.Printf("LAN auto-discovery on UDP %s (beacon every 5s + query应答)", *discoverAddr)
			defer disco.Stop()
		}
	}

	go func() {
		log.Printf("LAN Agent Hub HTTP gateway on %s", *httpAddr)
		if err := adapter.StartHTTP(*httpAddr, hub, disco); err != nil {
			log.Printf("http gateway error: %v", err)
		}
	}()

	if err := adapter.ServeTCP(ln, hub); err != nil {
		log.Printf("serve error: %v", err)
	}
}

// portOf 从 ":8080" / "0.0.0.0:8080" 中提取端口号。
func portOf(addr string) int {
	idx := strings.LastIndex(addr, ":")
	if idx < 0 {
		return 0
	}
	p, err := strconv.Atoi(addr[idx+1:])
	if err != nil {
		return 0
	}
	return p
}
