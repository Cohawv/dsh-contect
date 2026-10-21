package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestJSONLineRoundTrip(t *testing.T) {
	c := JSONLine{}
	src := Envelope{MsgID: "m1", Type: TypeChat, From: "A", To: "B"}
	b, err := c.Encode(src)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !strings.HasSuffix(string(b), "\n") {
		t.Fatalf("NDJSON frame must end with newline: %q", string(b))
	}
	got, err := c.Decode([]byte(strings.TrimSuffix(string(b), "\n")))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.MsgID != src.MsgID || got.Type != src.Type || got.From != src.From || got.To != src.To {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}

func TestNegotiate(t *testing.T) {
	cases := []struct {
		name    string
		clientV int
		want    int
	}{
		{"未声明版本按 v1", 0, ProtoLegacy},
		{"负数按 v1", -1, ProtoLegacy},
		{"v1 保持 v1", 1, ProtoLegacy},
		{"v2 协商为 v2", 2, ProtoV2},
		{"超出服务端能力则降级到当前", 99, ProtoCurrent},
		{"低于最低支持版本按 v1", ProtoMinSupported - 1, ProtoLegacy},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Negotiate(tc.clientV); got != tc.want {
				t.Fatalf("Negotiate(%d) = %d, want %d", tc.clientV, got, tc.want)
			}
		})
	}
}

// 版本门控：会改变客户端行为的字段只在协商版本 >= 2 时下发。
func TestRegisteredAckGating(t *testing.T) {
	var content map[string]any
	cfg := EndpointsCfg{HeartbeatMs: DefaultHeartbeatMs, TimeoutMs: DefaultTimeoutMs, TCPPort: 8080, HTTPPort: 8081}

	legacy := RegisteredAck("A", nil, ProtoLegacy, cfg)
	if err := json.Unmarshal(legacy.Content, &content); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"proto", "serverCaps", "heartbeatMs", "timeoutMs", "features", "clientCaps", "endpoints"} {
		if _, ok := content[k]; ok {
			t.Fatalf("v1 ack must not contain %q", k)
		}
	}
	if content["text"] != "registered as A" {
		t.Fatalf("legacy text must always be present, got %v", content["text"])
	}

	v2 := RegisteredAck("A", []string{CapStructuredAck, CapHeartbeatNeg}, ProtoV2, cfg)
	if err := json.Unmarshal(v2.Content, &content); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"proto", "serverCaps", "heartbeatMs", "timeoutMs"} {
		if _, ok := content[k]; !ok {
			t.Fatalf("v2 ack must contain %q", k)
		}
	}
	for _, k := range []string{"features", "clientCaps", "endpoints"} {
		if _, ok := content[k]; ok {
			t.Fatalf("v2 ack must not contain %q (v3+ only)", k)
		}
	}
	if content["text"] != "registered as A" {
		t.Fatalf("v2 ack must still carry legacy text, got %v", content["text"])
	}

	v3 := RegisteredAck("A", []string{CapStructuredAck, CapHeartbeatNeg, CapDiscovery, CapAgentCaps, CapEndpoints}, ProtoCurrent, cfg)
	if err := json.Unmarshal(v3.Content, &content); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"features", "clientCaps", "endpoints"} {
		if _, ok := content[k]; !ok {
			t.Fatalf("v3 ack must contain %q", k)
		}
	}
	feat, _ := content["features"].(map[string]any)
	if feat["structuredAck"] != true || feat["discovery"] != true || feat["endpoints"] != true {
		t.Fatalf("v3 features should enable discovery/endpoints, got %v", feat)
	}
	if ep, _ := content["endpoints"].(map[string]any); ep["tcpPort"] != float64(8080) {
		t.Fatalf("v3 endpoints must carry tcpPort, got %v", ep)
	}
}

// 能力门控：未声明对应 cap，即使版本够高也不会收到该 feature。
func TestComputeFeatures(t *testing.T) {
	cases := []struct {
		name  string
		proto int
		caps  []string
		want  NegotiatedFeatures
	}{
		{"v1 全 false", ProtoLegacy, nil, NegotiatedFeatures{}},
		{"v2 仅 structuredAck", ProtoV2, []string{CapStructuredAck}, NegotiatedFeatures{StructuredAck: true}},
		{"v3 无 cap 仅 structuredAck", ProtoCurrent, nil, NegotiatedFeatures{StructuredAck: true}},
		{"v3 全 cap", ProtoCurrent, []string{CapStructuredAck, CapHeartbeatNeg, CapDiscovery, CapAgentCaps, CapEndpoints},
			NegotiatedFeatures{StructuredAck: true, Heartbeat: true, Discovery: true, AgentCaps: true, Endpoints: true}},
		{"v3 discovery 隐含 endpoints", ProtoCurrent, []string{CapDiscovery},
			NegotiatedFeatures{StructuredAck: true, Discovery: true, Endpoints: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ComputeFeatures(tc.proto, tc.caps)
			if got != tc.want {
				t.Fatalf("ComputeFeatures(%d,%v) = %+v, want %+v", tc.proto, tc.caps, got, tc.want)
			}
		})
	}
}

func TestHasCap(t *testing.T) {
	if !HasCap([]string{"a", "b"}, "b") {
		t.Fatal("should find cap b")
	}
	if HasCap([]string{"a"}, "z") {
		t.Fatal("should not find cap z")
	}
	if HasCap(nil, "a") {
		t.Fatal("nil caps should not match")
	}
	if !CapSet([]string{"x"}).Has("x") {
		t.Fatal("CapSet.Has should work")
	}
}
