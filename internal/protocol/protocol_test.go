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
		{"v2 启用 v2", 2, ProtoCurrent},
		{"超出服务端能力则降级", 99, ProtoCurrent},
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

	legacy := RegisteredAck("A", ProtoLegacy, DefaultHeartbeatMs, DefaultTimeoutMs)
	if err := json.Unmarshal(legacy.Content, &content); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"proto", "serverCaps", "heartbeatMs", "timeoutMs"} {
		if _, ok := content[k]; ok {
			t.Fatalf("v1 ack must not contain %q", k)
		}
	}
	if content["text"] != "registered as A" {
		t.Fatalf("legacy text must always be present, got %v", content["text"])
	}

	modern := RegisteredAck("A", ProtoCurrent, DefaultHeartbeatMs, DefaultTimeoutMs)
	if err := json.Unmarshal(modern.Content, &content); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"proto", "serverCaps", "heartbeatMs", "timeoutMs"} {
		if _, ok := content[k]; !ok {
			t.Fatalf("v2 ack must contain %q", k)
		}
	}
	if content["text"] != "registered as A" {
		t.Fatalf("v2 ack must still carry legacy text, got %v", content["text"])
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
}
