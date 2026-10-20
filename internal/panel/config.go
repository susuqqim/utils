// config.go 面板配置页接口：读取当前配置、校验并保存（热生效 + 重启项标注）。
//
// 分工：cmd/server 持有 Config 类型与校验逻辑（Load/normalize），此处只做
// HTTP 编排——GET 回显、POST 透传给注入的 SaveConfig 闭包（由 main 完成
// "校验 → 落盘 → 热应用 → 返回需重启字段列表"）。
package panel

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
)

// getConfig 返回当前配置文件内容与路径（前端按 schema 渲染表单）。
func (p *Panel) getConfig(w http.ResponseWriter, r *http.Request) {
	if p.cfg.LoadConfig == nil {
		writeErr(w, http.StatusNotImplemented, "config api not available")
		return
	}
	cfg, err := p.cfg.LoadConfig()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load config: "+err.Error())
		return
	}
	// api_key_set：控制台据此判断是否需要弹出「首次初始化」提示。
	// 注意：只回布尔，绝不回密钥内容。
	// LoadConfig 可能返回 *Config、map[string]any 或任意结构体 ⇒ 统一走 JSON 归一化再判断，
	// 且**顺手从响应里剥掉 api_key**（此前会把明文密钥回给前端，注释与实现不一致）。
	keySet := false
	if raw, err := json.Marshal(cfg); err == nil {
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err == nil {
			if v, _ := m["api_key"].(string); v != "" && v != "CHANGE_ME_BEFORE_USE" {
				keySet = true
			}
			delete(m, "api_key") // 绝不回明文
			cfg = m
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"path":        p.cfg.ConfigPath,
		"api_key_set": keySet,
		"config":      cfg,
	})
}

// saveConfig 保存配置：body 直接是配置 JSON（前端按 schema 组装完整对象）。
// SaveConfig 闭包内部完成校验+落盘+热应用；校验失败返回 400 且不写盘。
func (p *Panel) saveConfig(w http.ResponseWriter, r *http.Request) {
	if p.cfg.SaveConfig == nil {
		writeErr(w, http.StatusNotImplemented, "config api not available")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	restartRequired, err := p.cfg.SaveConfig(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if restartRequired == nil {
		restartRequired = []string{}
	}
	log.Printf("panel: 配置已保存（热生效完成；需重启字段 %d 个）", len(restartRequired))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"restart_required": restartRequired,
	})
}
