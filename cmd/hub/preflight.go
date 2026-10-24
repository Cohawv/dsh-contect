package main

// 启动前端口预检（V7d）。
//
// 动机：此前启动遇到端口被占，症状是「必需插件 tcpgateway 启动失败」的
// 一串堆栈——用户要自己猜占端口的是谁。而最常见的占端口者恰恰是
// **上一个 Hub 实例**（双击两次 exe、忘关旧窗口）。预检在启动早期就把
// 这件事查清楚并用人话说出来；-replace 时甚至可以请旧实例体面退场。
//
// 安全边界（刻意的保守）：
//   - 只识别、只引导、只在用户显式 -replace 时才请求关停；
//   - -replace 仅当端口占用者被 /healthz 确认是 LAN Agent Hub 时才生效——
//     对无关程序（别的服务、别的软件）绝不代杀，直接报错让用户自己处理。

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// probeDial / probeHTTPClient 是探测函数的包级变量——测试注入用。
// main 包的小工具，函数变量比抽一个 dialer 接口更极简。
var (
	probeDial = net.DialTimeout
	probeGet  = func(url string) (statusLine string, body string, err error) {
		cl := &http.Client{Timeout: 800 * time.Millisecond}
		resp, err := cl.Get(url)
		if err != nil {
			return "", "", err
		}
		defer resp.Body.Close()
		buf := make([]byte, 256)
		n, _ := resp.Body.Read(buf)
		return resp.Status, string(buf[:n]), nil
	}
	// waitPortFree 等待端口释放（-replace 关停旧实例后用）；测试可注入立即成功。
	waitPortFree = defaultWaitPortFree
)

// loopbackHost 把监听地址里的 host 规范化为可探测的本机地址：
// 空 host（":8081"）与通配（"0.0.0.0"/"::"）都探 127.0.0.1——
// 我们关心的是「本进程能不能绑上」，不是「绑在哪块网卡」。
func loopbackHost(listenAddr string) (string, error) {
	host, _, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return "", fmt.Errorf("地址 %q 不是 host:port 形式: %w", listenAddr, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}
	return host, nil
}

// portNumOf 仅取端口号。
func portNumOf(listenAddr string) (int, error) {
	_, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(port)
	if err != nil || n <= 0 || n > 65535 {
		return 0, fmt.Errorf("地址 %q 端口不合法", listenAddr)
	}
	return n, nil
}

// occupied 报告 host:port 是否已有监听者。
func occupied(host string, port int) bool {
	conn, err := probeDial("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// isHub 探测 host:port 是否为 LAN Agent Hub（依据 /healthz 的响应特征）。
// 只有「确认是 Hub」才走接管/引导的特殊路径，其它一律按无关程序处理。
func isHub(host string, port int) bool {
	url := fmt.Sprintf("http://%s/healthz", net.JoinHostPort(host, strconv.Itoa(port)))
	_, body, err := probeGet(url)
	if err != nil {
		return false
	}
	// /healthz 返回 {"status":"ok"}（healthz 插件契约）。只认这个特征，
	// 不认 200 裸状态码——普通 web 服务返回 200 的太多了。
	return strings.Contains(body, `"status":"ok"`)
}

// defaultWaitPortFree 轮询等待端口释放。
func defaultWaitPortFree(host string, port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !occupied(host, port) {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("等待端口 %d 释放超时（%s）", port, timeout)
}

// probePost 与 probeGet 同 client/超时，但发 POST（/api/shutdown 只认 POST）。
var probePost = func(url string) (statusCode int, err error) {
	cl := &http.Client{Timeout: 3 * time.Second}
	resp, err := cl.Post(url, "application/json", strings.NewReader(`{}`))
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

// shutdownHub 请 host:port 上的旧 Hub 优雅退出（POST /api/shutdown）。
// 返回 error 表示接管失败，调用方应中止启动并如实报告。
func shutdownHub(host string, port int) error {
	url := fmt.Sprintf("http://%s/api/shutdown", net.JoinHostPort(host, strconv.Itoa(port)))
	code, err := probePost(url)
	if err != nil {
		return fmt.Errorf("请求旧实例关停失败: %w", err)
	}
	switch {
	case code == http.StatusNotFound:
		return errors.New("旧实例版本过旧，不支持远程关停（/api/shutdown 不存在），请手动关闭后重试")
	case code == http.StatusForbidden:
		return errors.New("旧实例拒绝了本机的关停请求（HTTP 403），请手动关闭后重试")
	case code != http.StatusOK:
		return fmt.Errorf("旧实例关停请求异常（HTTP %d）", code)
	}
	return nil
}

// preflightPorts 启动前检查 TCP 与 HTTP 端口。
// 返回 error 时 main 以 log.Fatalf 退出——错误文案自带出路（关停命令 / -replace / 换端口）。
func preflightPorts(tcpAddr, httpAddr string, replace bool) error {
	// 先查 HTTP 端口（-replace 的接管动作发生在它身上）。
	if err := preflightOne(httpAddr, replace, "HTTP"); err != nil {
		return err
	}
	// 再查 TCP 端口。若刚刚接管了旧 Hub，它的 TCP 监听会随进程退出而释放，
	// 这里继续走同样的探测逻辑即可，无需特判。
	return preflightOne(tcpAddr, replace, "TCP")
}

func preflightOne(listenAddr string, replace bool, label string) error {
	host, err := loopbackHost(listenAddr)
	if err != nil {
		return err
	}
	port, err := portNumOf(listenAddr)
	if err != nil {
		return err
	}
	if !occupied(host, port) {
		return nil
	}
	if !isHub(host, port) {
		// 绝不代杀无关程序——占用者身份不明，代杀等于拿用户的其它软件开刀。
		return fmt.Errorf("%s端口 %s 被其它程序占用（/healthz 探测不是 LAN Agent Hub，不敢代为关闭）。请先关闭该程序，或换一个端口",
			label, listenAddr)
	}
	if !replace {
		return fmt.Errorf("%s端口 %s 已被另一个 LAN Agent Hub 占用。三条出路：关闭旧实例后重试；加 -replace 自动接管（请求旧实例优雅退出）；换端口（如 -http :8082）",
			label, listenAddr)
	}
	// -replace 且确认是旧 Hub：请它走，等端口释放。
	if err := shutdownHub(host, port); err != nil {
		return fmt.Errorf("-replace 接管失败：%v", err)
	}
	if err := waitPortFree(host, port, 10*time.Second); err != nil {
		return fmt.Errorf("-replace 接管失败：%v", err)
	}
	return nil
}
