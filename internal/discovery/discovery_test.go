package discovery

import (
	"encoding/json"
	"fmt"
	"net"
	"testing"
	"time"
)

func TestBeaconJSON(t *testing.T) {
	svc := New(Options{HubID: "h1", TCPPort: 8080, HTTPPort: 8081, Proto: 3, Caps: []string{"discovery"}})
	b := svc.Beacon()
	if b.Type != BeaconType {
		t.Fatalf("beacon type = %q, want %q", b.Type, BeaconType)
	}
	if b.TCPPort != 8080 || b.HTTPPort != 8081 || b.Proto != 3 {
		t.Fatalf("beacon fields wrong: %+v", b)
	}
	raw := svc.BeaconJSON()
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("beacon json invalid: %v", err)
	}
	if decoded["type"] != BeaconType {
		t.Fatalf("beacon json missing type")
	}
}

func TestHandleQuery(t *testing.T) {
	svc := New(Options{HubID: "h1", TCPPort: 8080, HTTPPort: 8081, Proto: 3})
	// 合法 query
	b, ok := svc.HandleQuery([]byte(`{"type":"_lanhub_query"}`))
	if !ok {
		t.Fatal("valid query should be accepted")
	}
	if b.TCPPort != 8080 || b.Type != BeaconType {
		t.Fatalf("query response wrong: %+v", b)
	}
	// 非法 type
	if _, ok := svc.HandleQuery([]byte(`{"type":"foo"}`)); ok {
		t.Fatal("non-query type must be rejected")
	}
	// 非法 json
	if _, ok := svc.HandleQuery([]byte(`not json`)); ok {
		t.Fatal("malformed json must be rejected")
	}
}

func TestBroadcastReachesTarget(t *testing.T) {
	listener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	lp := listener.LocalAddr().(*net.UDPAddr).Port

	svc := New(Options{
		BindAddr:   "127.0.0.1:0",
		TCPPort:    8080,
		HTTPPort:   8081,
		HubID:      "broadcast-test",
		Name:       "test-hub",
		Proto:      3,
		Caps:       []string{"discovery", "structured-ack"},
		Interval:   500 * time.Millisecond,
		Targets:    []string{fmt.Sprintf("127.0.0.1:%d", lp)},
	})
	if err := svc.Start(); err != nil {
		t.Fatal(err)
	}
	defer svc.Stop()

	_ = listener.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 1024)
	n, _, err := listener.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("did not receive broadcast beacon: %v", err)
	}
	var b Beacon
	if err := json.Unmarshal(buf[:n], &b); err != nil {
		t.Fatal(err)
	}
	if b.HubID != "broadcast-test" || b.TCPPort != 8080 {
		t.Fatalf("received beacon wrong: %+v", b)
	}
}

func TestQueryResponseOverUDP(t *testing.T) {
	// 起一个真实 Service，客户端发 query，应在 UDP 上收到 beacon 回应
	svc := New(Options{
		BindAddr: "127.0.0.1:0",
		TCPPort:  9090,
		HTTPPort: 9091,
		HubID:    "query-test",
		Proto:    3,
		Caps:     []string{"discovery"},
	})
	if err := svc.Start(); err != nil {
		t.Fatal(err)
	}
	defer svc.Stop()
	port := svc.conn.LocalAddr().(*net.UDPAddr).Port

	client, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_, _ = client.Write([]byte(`{"type":"_lanhub_query"}`))

	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1024)
	n, err := client.Read(buf)
	if err != nil {
		t.Fatalf("did not receive query response: %v", err)
	}
	var b Beacon
	if err := json.Unmarshal(buf[:n], &b); err != nil {
		t.Fatal(err)
	}
	if b.HubID != "query-test" || b.TCPPort != 9090 {
		t.Fatalf("query response wrong: %+v", b)
	}
}
