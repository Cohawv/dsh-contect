package transport

import (
	"bufio"
	"bytes"
	"net"
	"testing"
	"time"

	"lan-agent-hub/internal/protocol"
)

// pipeConn 造一对内存连接，服务端侧包成 ndjsonConn。
func pipeConn(t *testing.T) (*ndjsonConn, net.Conn) {
	t.Helper()
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
	return &ndjsonConn{conn: server, codec: protocol.JSONLine{}}, client
}

// 一批多帧写出后，对端必须能逐行解出全部信封且顺序不变。
func TestSendBatchWritesDecodableFramesInOrder(t *testing.T) {
	c, peer := pipeConn(t)
	envs := []protocol.Envelope{
		protocol.NewEnvelope(protocol.TypeChat, "a", "b", "", map[string]string{"i": "0"}),
		protocol.NewEnvelope(protocol.TypeChat, "a", "b", "", map[string]string{"i": "1"}),
		protocol.NewEnvelope(protocol.TypeChat, "a", "b", "", map[string]string{"i": "2"}),
	}

	done := make(chan error, 1)
	go func() { done <- c.SendBatch(envs) }()

	r := bufio.NewReader(peer)
	codec := protocol.JSONLine{}
	for i := 0; i < len(envs); i++ {
		line, err := r.ReadBytes('\n')
		if err != nil {
			t.Fatalf("读第 %d 帧失败: %v", i, err)
		}
		env, err := codec.Decode(bytes.TrimSpace(line))
		if err != nil {
			t.Fatalf("第 %d 帧解码失败: %v（内容 %q）", i, err, string(line))
		}
		if env.From != "a" || env.To != "b" {
			t.Fatalf("第 %d 帧字段不对: %+v", i, env)
		}
		var got map[string]string
		if err := protocol.UnmarshalContent(env.Content, &got); err != nil {
			t.Fatalf("第 %d 帧 content 解码失败: %v", i, err)
		}
		if want := string(rune('0' + i)); got["i"] != want {
			t.Fatalf("第 %d 帧顺序错了：i=%s，期望 %s（合批必须保序）", i, got["i"], want)
		}
	}
	if err := <-done; err != nil {
		t.Fatalf("SendBatch 返回错误: %v", err)
	}
}

// 空批不发任何字节；单条批等价于 Send。
func TestSendBatchEmptyAndSingle(t *testing.T) {
	c, peer := pipeConn(t)
	if err := c.SendBatch(nil); err != nil {
		t.Fatalf("空批应成功且不发字节，实际 %v", err)
	}

	one := protocol.NewEnvelope(protocol.TypeChat, "x", "y", "", map[string]string{"k": "v"})
	go func() { _ = c.SendBatch([]protocol.Envelope{one}) }()

	if err := peer.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("设读超时失败: %v", err)
	}
	line, err := bufio.NewReader(peer).ReadBytes('\n')
	if err != nil {
		t.Fatalf("单条批应写出 1 帧: %v", err)
	}
	if n := bytes.Count(line, []byte("\n")); n != 1 {
		t.Fatalf("单条批应只有 1 个换行，实际 %d", n)
	}
}

// 缓冲必须可复用：连续两批不应因为上次的残留而写出重复帧。
func TestSendBatchBufferReuseDoesNotLeak(t *testing.T) {
	c, peer := pipeConn(t)
	mk := func(tag string) protocol.Envelope {
		return protocol.NewEnvelope(protocol.TypeChat, "a", "b", "", map[string]string{"t": tag})
	}
	first := []protocol.Envelope{mk("1"), mk("2")}
	second := []protocol.Envelope{mk("3")}

	done := make(chan struct{})
	go func() {
		_ = c.SendBatch(first)
		_ = c.SendBatch(second)
		close(done)
	}()

	r := bufio.NewReader(peer)
	codec := protocol.JSONLine{}
	tags := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		if err := peer.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
			t.Fatalf("设读超时失败: %v", err)
		}
		line, err := r.ReadBytes('\n')
		if err != nil {
			t.Fatalf("读第 %d 帧失败: %v", i, err)
		}
		env, err := codec.Decode(bytes.TrimSpace(line))
		if err != nil {
			t.Fatalf("解码失败: %v", err)
		}
		var got map[string]string
		_ = protocol.UnmarshalContent(env.Content, &got)
		tags = append(tags, got["t"])
	}
	<-done
	want := []string{"1", "2", "3"}
	for i := range want {
		if tags[i] != want[i] {
			t.Fatalf("第 %d 帧 = %q，期望 %q（复用缓冲不能带出上一批的残留）", i, tags[i], want[i])
		}
	}
}

// ndjsonConn 必须满足可选接口，否则 writeLoop 永远走不到批量路径。
func TestNDJSONConnImplementsBatchConn(t *testing.T) {
	var c Conn = &ndjsonConn{}
	if _, ok := c.(BatchConn); !ok {
		t.Fatalf("ndjsonConn 应实现 BatchConn（否则写合并静默失效）")
	}
}
