package main

import (
	"flag"
	"log"
	"net"
)

func main() {
	addr := flag.String("addr", ":8080", "hub TCP listen address, e.g. :8080 or 0.0.0.0:8080")
	httpAddr := flag.String("http", ":8081", "hub HTTP gateway address (REST), e.g. :8081")
	flag.Parse()

	hub := NewHub()
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen %s failed: %v", *addr, err)
	}
	log.Printf("LAN Agent Hub TCP listening on %s (NDJSON)", *addr)
	go hub.cleanupLoop()

	go func() {
		log.Printf("LAN Agent Hub HTTP gateway on %s", *httpAddr)
		if err := hub.StartHTTP(*httpAddr); err != nil {
			log.Printf("http gateway error: %v", err)
		}
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("accept error: %v", err)
			continue
		}
		go hub.handleConn(conn)
	}
}
