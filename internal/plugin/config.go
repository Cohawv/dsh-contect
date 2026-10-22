package plugin

import (
	"encoding/json"
	"fmt"
	"os"
)

// PluginConfig 是 hub.json 中单个插件的配置段。
type PluginConfig struct {
	Enabled bool            `json:"enabled"`         // 是否启用（文件级覆盖 DefaultOn）
	Options map[string]any  `json:"options"`         // 传给插件 Context 的自定义配置项
}

// HubConfig 是 hub.json 中 plugins 段的结构。
type HubConfig struct {
	Plugins map[string]PluginConfig `json:"plugins"`
}

// LoadConfig 加载 hub.json 并合并每个插件的启用状态与配置项。
//
// 合并优先级（高 → 低）：CLI 显式开关 (cliEnabled) > 文件 (hub.json) > Meta.DefaultOn。
// 本版本只加载、不写回（写回留 V4）。
//
// 返回值：
//   - enabled：Name → 最终启用状态；
//   - options：Name → 合并后的配置项（无文件配置时为空 map，便于 Context 读取）；
//   - err：文件不存在视为「全部走默认」且不报错；文件损坏才返回错误。
func LoadConfig(path string, cliEnabled map[string]bool) (enabled map[string]bool, options map[string]map[string]any, err error) {
	enabled = make(map[string]bool)
	options = make(map[string]map[string]any)

	var data []byte
	if path != "" {
		data, err = os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				err = nil // 文件缺省：全部走默认
			} else {
				return enabled, options, fmt.Errorf("read config %q: %w", path, err)
			}
		}
	}

	fileCfg := HubConfig{}
	if len(data) > 0 {
		if err = json.Unmarshal(data, &fileCfg); err != nil {
			return enabled, options, fmt.Errorf("parse config %q: %w", path, err)
		}
	}

	// 遍历全局注册表（编译期已知），确定每个插件的启用状态与选项。
	for _, p := range All() {
		name := p.Meta().Name
		on := p.Meta().DefaultOn
		opts := map[string]any{}

		if pc, ok := fileCfg.Plugins[name]; ok {
			on = pc.Enabled
			if pc.Options != nil {
				opts = pc.Options
			}
		}
		if v, ok := cliEnabled[name]; ok {
			on = v
		}

		enabled[name] = on
		options[name] = opts
	}

	return enabled, options, nil
}
