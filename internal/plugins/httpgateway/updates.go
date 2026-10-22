// 更新通道的**服务端预留接口**（契约先行，实现后置）。
//
// 定位：这里只提供「清单」和「制品下载」两个稳定端点，不含任何推送、灰度、
// 签名校验或自动生效逻辑。客户端（dsh-plugin-lan-hub 的 lanhub-update）按本文件
// 的契约实现即可，将来把清单换成数据库/对象存储时，客户端无需改动。
//
// 契约纪律：
//   - 清单**永远返回 200 + 合法 JSON**。没有可用更新时是 latest="" + artifacts={}，
//     绝不返回 404——"接口不存在"和"没有新版本"对客户端是两种完全不同的语义，
//     混在一起会让排查变成猜谜。
//   - 清单文件损坏时返回 500 + 明确错误，而不是装作"没有更新"。静默降级会让人
//     以为已经是最新版，实际是分发坏了。
//   - artifact 的 url 允许相对路径，由客户端按清单 URL 解析；服务端不猜 scheme/host
//     （反代、TLS、端口映射下猜必错）。
//
// 数据来源：环境变量 LANHUB_UPDATE_DIR（默认 ./updates），期望结构：
//
//	./updates/lanhub-update.json
//	./updates/pkg/dsh-plugin-lan-hub-0.6.0.tgz
package httpgateway

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// 预留端点路径。客户端不得硬编码拼接，按清单里的 url 取。
const (
	// UpdateManifestPath 返回更新清单。
	UpdateManifestPath = "/api/updates"
	// updateArtifactPrefix 是制品下载前缀，其后只允许纯文件名。
	updateArtifactPrefix = "/api/updates/pkg/"
	// updateManifestFile 是清单文件名（位于 LANHUB_UPDATE_DIR 下）。
	updateManifestFile = "lanhub-update.json"
)

// UpdateArtifact 描述一个可下载的版本制品。
type UpdateArtifact struct {
	// URL 允许相对路径（如 "/api/updates/pkg/x.tgz"），客户端按清单来源解析。
	URL string `json:"url"`
	// SHA256 是制品的小写十六进制摘要，客户端**必须**校验后才允许暂存。
	SHA256 string `json:"sha256"`
	// Size 是字节数，用于下载后立即发现截断。
	Size int64 `json:"size"`
	// BuiltAt 为构建时间（RFC3339），仅用于展示排序。
	BuiltAt string `json:"builtAt"`
	// Notes 是给人看的一句话变更说明。
	Notes string `json:"notes"`
	// Proto 是该版本插件要求的线协议版本。客户端据此与 Hub 的 X-Proto-Version 比对，
	// 不匹配则拒绝安装——否则会出现"装完发现连不上"。
	Proto int `json:"proto"`
	// MinHost 是该版本要求的最低宿主 dsh 版本。
	MinHost string `json:"minHost"`
}

// UpdateManifest 是 GET /api/updates 的响应体。
type UpdateManifest struct {
	Name string `json:"name"`
	// Latest 为空表示"当前没有可用更新"（合法状态，不是错误）。
	Latest string `json:"latest"`
	// Proto / MinHost 是 latest 的冗余摘要，便于客户端一眼判断兼容性。
	Proto   int    `json:"proto"`
	MinHost string `json:"minHost"`
	// Source 标明清单位置，纯调试用。
	Source string                    `json:"source"`
	Items  map[string]UpdateArtifact `json:"artifacts"`
}

// updateDir 返回分发目录。允许用环境变量指向别处（容器 / 只读挂载都能用）。
func updateDir() string {
	if d := os.Getenv("LANHUB_UPDATE_DIR"); d != "" {
		return d
	}
	return "updates"
}

// loadUpdateManifest 读取并校验清单。
// 返回值 err == nil 且 latest 为空即"没有更新"，这是正常路径而非异常。
func loadUpdateManifest(dir string) (UpdateManifest, error) {
	empty := UpdateManifest{
		Name:   "dsh-plugin-lan-hub",
		Source: dir,
		Items:  map[string]UpdateArtifact{},
	}

	path := filepath.Join(dir, updateManifestFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// 目录里还没有清单：接口照样可用，只是没有更新。
			return empty, nil
		}
		return empty, err
	}

	var m UpdateManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return empty, err
	}
	if m.Items == nil {
		m.Items = map[string]UpdateArtifact{}
	}
	if m.Name == "" {
		m.Name = empty.Name
	}
	m.Source = path
	// latest 必须能在 artifacts 里找到，否则是分发事故，不能瞒着客户端。
	if m.Latest != "" {
		if _, ok := m.Items[m.Latest]; !ok {
			return empty, &manifestInconsistent{latest: m.Latest}
		}
	}
	return m, nil
}

type manifestInconsistent struct{ latest string }

func (e *manifestInconsistent) Error() string {
	return "manifest latest=" + e.latest + " 在 artifacts 中不存在"
}

// handleUpdateManifest 处理 GET /api/updates。
func handleUpdateManifest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	m, err := loadUpdateManifest(updateDir())
	if err != nil {
		http.Error(w, "update manifest invalid: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, m)
}

// handleUpdateArtifact 处理 GET /api/updates/pkg/<file>。
// 只接受纯文件名：反斜杠、斜杠、".." 一律拒绝，杜绝路径穿越。
func handleUpdateArtifact(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, updateArtifactPrefix)
	if name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") ||
		!strings.HasSuffix(name, ".tgz") {
		http.Error(w, "bad artifact name", http.StatusBadRequest)
		return
	}
	full := filepath.Join(updateDir(), "pkg", name)
	if _, err := os.Stat(full); err != nil {
		http.Error(w, "artifact not found", http.StatusNotFound)
		return
	}
	// 制品按内容协商为 octet-stream，避免被当成文本预览。
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeFile(w, r, full)
}

// registerUpdateRoutes 把两个预留端点挂到给定 registrar 上。
func registerUpdateRoutes(reg func(pattern string, h http.HandlerFunc)) {
	reg(UpdateManifestPath, handleUpdateManifest)
	reg(updateArtifactPrefix, handleUpdateArtifact)
}

// attachUpdateRoutes 把预留端点挂到共享 mux 上，**按 mux 幂等**。
//
// 为什么不用 ctx.RegisterHTTP：那会记进本插件的 routes/routeCount，从而改动
// /api/plugins 的 v2 事实契约基线——为两个预留端点改 golden 不值得。
//
// 为什么需要幂等：插件 reload/开关会再次调用 Init，而 ctx.ServeMux() 返回同一个
// mux；直接 mux.HandleFunc 重复注册会 panic（"multiple registrations"）。
// 按 mux 指针记账即可：同一个 mux 只挂一次，测试里新建的 mux 照常挂载。
var updateRoutesDone sync.Map // *http.ServeMux -> struct{}

func attachUpdateRoutes(mux *http.ServeMux) {
	if _, loaded := updateRoutesDone.LoadOrStore(mux, struct{}{}); loaded {
		return
	}
	registerUpdateRoutes(func(pattern string, h http.HandlerFunc) {
		mux.HandleFunc(pattern, h)
	})
}
