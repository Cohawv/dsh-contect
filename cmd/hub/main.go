package main

import (
	"flag"
	"log"
	"time"

	"lan-agent-hub/internal/adapter"
	"lan-agent-hub/internal/core"
	"lan-agent-hub/internal/protocol"
	"lan-agent-hub/internal/transport"
)

// main 只做装配：选传输实现 → 注入 codec → 构造业务内核 → 起接入层。
// 换传输（如改 WebSocket）只需替换这里的 transport 实现，其余代码不动。
func main() {
	addr := flag.String("addr", ":8080", "hub TCP listen address, e.g. :8080 or 0.0.0.0:8080")
	httpAddr := flag.String("http", ":8081", "hub HTTP gateway address (REST), e.g. :8081")
	flag.Parse()

	codec := protocol.JSONLine{}
	hub := core.NewHub()

	ln, err := transport.ListenTCP(*addr, codec)
	if err != nil {
		log.Fatalf("listen %s failed: %v", *addr, err)
	}
	log.Printf("LAN Agent Hub TCP listening on %s (NDJSON)", *addr)

	go hub.SweepLoop(30 * time.Second)

	go func() {
		log.Printf("LAN Agent Hub HTTP gateway on %s", *httpAddr)
		if err := adapter.StartHTTP(*httpAddr, hub); err != nil {
			log.Printf("http gateway error: %v", err)
		}
	}()

	if err := adapter.ServeTCP(ln, hub); err != nil {
		log.Printf("serve error: %v", err)
	}
}
