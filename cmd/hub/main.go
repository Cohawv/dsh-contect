package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"lan-agent-hub/internal/adapter"
	"lan-agent-hub/internal/core"
	"lan-agent-hub/internal/discovery"
	"lan-agent-hub/internal/protocol"
	"lan-agent-hub/internal/transport"
)

// version 由构建时通过 -ldflags "-X main.version=..." 注入；默认值用于本地开发。
var version = "dev"

// main 只做装配：选传输实现 → 注入 codec → 构造业务内核 → 起接入层与发现服务。
// 换传输（如改 WebSocket）只需替换这里的 transport 实现，其余代码不动。
func main() {
	initLogging()
	addr := flag.String("addr", ":8080", "hub TCP listen address, e.g. :8080 or 0.0.0.0:8080")
	httpAddr := flag.String("http", ":8081", "hub HTTP gateway address (REST), e.g. :8081")
	discoverAddr := flag.String("discover", ":8082", "UDP discovery bind address; set empty to disable")
	noDiscovery := flag.Bool("no-discovery", false, "disable LAN auto-discovery beacon")
	hubName := flag.String("name", "", "hub display name (default: hostname)")
	showVersion := flag.Bool("version", false, "print version and exit")
	openBrowserFlag := flag.Bool("open", true, "auto-open the web dashboard in the default browser on start")
	flag.Parse()

	if *showVersion {
		log.Printf("lan-agent-hub %s", version)
		return
	}

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

	// 启动后自动在浏览器打开控制台，做到"双击 EXE 即见 UI"。
	if *openBrowserFlag {
		dashboardURL := fmt.Sprintf("http://localhost%s/", *httpAddr)
		go func() {
			time.Sleep(400 * time.Millisecond) // 等 HTTP 端口绑定完成
			openBrowser(dashboardURL)
		}()
	}

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

// initLogging 决定日志去向：若标准输出是终端（控制台编译/在终端运行）就照常打到控制台；
// 若不是（如用 -H=windowsgui 编译的无窗口版本，stdout 无效），则写入 exe 同目录的日志文件，
// 这样无窗口运行也能排障。零依赖、跨平台。
func initLogging() {
	if fi, err := os.Stdout.Stat(); err == nil && (fi.Mode()&os.ModeCharDevice) != 0 {
		return // 终端：保持默认（控制台）
	}
	if exe, err := os.Executable(); err == nil {
		logPath := filepath.Join(filepath.Dir(exe), "lan-agent-hub.log")
		if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644); err == nil {
			log.SetOutput(f)
		}
	}
}

// openBrowser 在系统默认浏览器中打开 URL，零依赖、跨平台。
// 失败仅记日志，不影响服务本身（用户仍可手动访问）。
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		log.Printf("自动打开浏览器失败（请手动访问 %s）：%v", url, err)
		return
	}
	log.Printf("已在默认浏览器打开控制台：%s", url)
}
