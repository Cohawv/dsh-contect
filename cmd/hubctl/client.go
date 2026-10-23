package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// defaultAddr 是 Hub REST 网关的默认地址，与 cmd/hub 的 -http 默认值 ":8081" 对齐。
const defaultAddr = "http://localhost:8081"

// 单请求超时：REST 调用是短操作，10s 足够；SSE 流另用不设整体超时的客户端。
const rpcTimeout = 10 * time.Second

// usageError 表示命令行用法错误（缺参数、未知子命令、非法取值）→ 退出码 1。
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

// usageErr 构造用法错误。
func usageErr(format string, args ...any) error {
	return &usageError{fmt.Sprintf(format, args...)}
}

// connError 表示「没能和服务端说上话」：拨号失败、连接被拒、传输中断 → 退出码 2。
// 连接被拒是其中最常见的一种（服务端没启动），错误文案里会附上启动提示。
type connError struct{ err error }

func (e *connError) Error() string { return e.err.Error() }
func (e *connError) Unwrap() error { return e.err }

// statusError 表示服务端返回了非 2xx → 退出码 2。
type statusError struct {
	status int
	path   string
	body   string
}

func (e *statusError) Error() string {
	body := strings.TrimSpace(e.body)
	if body == "" {
		return fmt.Sprintf("服务端返回 HTTP %d（%s）", e.status, e.path)
	}
	runes := []rune(body)
	if len(runes) > 300 {
		body = string(runes[:300]) + "…"
	}
	return fmt.Sprintf("服务端返回 HTTP %d（%s）：%s", e.status, e.path, body)
}

// client 是 hubctl 与 Hub 通信的唯一入口：只有 REST + SSE，不碰任何内部包。
type client struct {
	baseURL string
	rpc     *http.Client
	stream  *http.Client
}

// newClient 以 baseURL 为前缀构造客户端（形如 http://localhost:8081）。
func newClient(baseURL string) *client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return &client{
		baseURL: normalizeAddr(baseURL),
		rpc:     &http.Client{Timeout: rpcTimeout},
		// SSE 不能设 Client.Timeout（它含响应体读取，会把长连接掐断），
		// 只在拨号阶段设超时，保证「连不上」能快速失败而不是永远挂着。
		stream: &http.Client{Transport: &http.Transport{
			DialContext:         dialer.DialContext,
			IdleConnTimeout:     60 * time.Second,
			TLSHandshakeTimeout: 5 * time.Second,
		}},
	}
}

// normalizeAddr 补 scheme、去掉结尾斜杠，让拼接路径时不会出现双斜杠。
func normalizeAddr(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return defaultAddr
	}
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return strings.TrimRight(s, "/")
	}
	return strings.TrimRight(u.String(), "/")
}

func (c *client) endpoint(path string, q url.Values) string {
	u := c.baseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u
}

// getJSON 发起 GET 并把响应体解析到 out（out 为 nil 时只丢弃响应体）。
func (c *client) getJSON(ctx context.Context, path string, q url.Values, out any) error {
	data, err := c.do(ctx, http.MethodGet, path, q, nil, c.rpc)
	if err != nil {
		return err
	}
	return decodeInto(data, out)
}

// postJSON 发起 POST（body 序列化为 JSON）并把响应体解析到 out。
func (c *client) postJSON(ctx context.Context, path string, body any, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	data, err := c.do(ctx, http.MethodPost, path, nil, strings.NewReader(string(raw)), c.rpc)
	if err != nil {
		return err
	}
	return decodeInto(data, out)
}

// decodeInto 把响应体解析到 out；解析失败按「服务端错误」处理（退出码 2）。
func decodeInto(data []byte, out any) error {
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return &statusError{status: 0, path: "响应解析失败", body: string(data)}
	}
	return nil
}

// do 执行单次请求并返回响应体；非 2xx 与传输层错误分别包装成 statusError / connError。
func (c *client) do(ctx context.Context, method, path string, q url.Values, body io.Reader, hc *http.Client) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint(path, q), body)
	if err != nil {
		return nil, &usageError{fmt.Sprintf("构造请求失败（%s %s）：%v", method, path, err)}
	}
	req.Header.Set("Accept", "application/json")
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, &connError{fmt.Errorf("%s %s 失败：%w", method, c.endpoint(path, q), err)}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, &connError{fmt.Errorf("读取响应失败（%s）：%w", path, err)}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &statusError{status: resp.StatusCode, path: path, body: string(data)}
	}
	return data, nil
}

// ---------------------------------------------------------------------------
// SSE
// ---------------------------------------------------------------------------

// sseEvent 是一条解析后的 SSE 事件（event:/data:/id: 三段）。
type sseEvent struct {
	ID    string
	Event string
	Data  string
}

// readSSE 从 r 读出一个完整事件；流干净结束时返回 io.EOF。
//
// 帧格式与服务端 realtime/logview 插件一致：字段行 + 空行结尾；
// 以 ":" 开头的是注释行（心跳 ":ping"），按规范忽略。
func readSSE(r *bufio.Reader) (*sseEvent, error) {
	var (
		ev      sseEvent
		data    []string
		pending bool
	)
	for {
		line, err := r.ReadString('\n')
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed != "" {
			if strings.HasPrefix(trimmed, ":") {
				// 心跳/注释：不计入事件内容，但也不能当作流结束。
			} else {
				pending = true
				field, value := trimmed, ""
				if i := strings.IndexByte(trimmed, ':'); i >= 0 {
					field, value = trimmed[:i], trimmed[i+1:]
					value = strings.TrimPrefix(value, " ")
				}
				switch field {
				case "event":
					ev.Event = value
				case "data":
					data = append(data, value)
				case "id":
					ev.ID = value
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				if pending {
					ev.Data = strings.Join(data, "\n")
					return &ev, nil
				}
				return nil, io.EOF
			}
			return nil, err
		}
		if trimmed == "" && pending {
			ev.Data = strings.Join(data, "\n")
			return &ev, nil
		}
	}
}

// openStream 建立一条 SSE 连接（GET + Accept: text/event-stream）。
func (c *client) openStream(ctx context.Context, path string, q url.Values) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint(path, q), nil)
	if err != nil {
		return nil, &usageError{fmt.Sprintf("构造请求失败（GET %s）：%v", path, err)}
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := c.stream.Do(req)
	if err != nil {
		return nil, &connError{fmt.Errorf("GET %s 失败：%w", c.endpoint(path, q), err)}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, &statusError{status: resp.StatusCode, path: path, body: string(data)}
	}
	return resp.Body, nil
}

// follow 持续消费一条 SSE 流，把每条事件交给 handle。
// 流正常结束（EOF）或 ctx 取消都返回 nil——跟随被中断不是错误。
func (c *client) follow(ctx context.Context, path string, q url.Values, handle func(sseEvent) error) error {
	body, err := c.openStream(ctx, path, q)
	if err != nil {
		return err
	}
	defer body.Close()
	br := bufio.NewReader(body)
	for {
		if ctx.Err() != nil {
			return nil
		}
		ev, rerr := readSSE(br)
		if rerr != nil {
			if errors.Is(rerr, io.EOF) || ctx.Err() != nil {
				return nil
			}
			return &connError{fmt.Errorf("读取事件流失败（%s）：%w", path, rerr)}
		}
		if err := handle(*ev); err != nil {
			return err
		}
	}
}
