package upstream

import (
	"strings"

	"workbuddy2api/internal/auth"
)

// identity.go 出站身份预设：WB（桌面端）/ VSCode（IDE 扩展）/ CLI。
//
// 背景（2026-10-21 上游收紧渠道校验）：上游开始用 `11128 Illegal API invocation from
// an unapproved channel` 拦截"看起来不像官方客户端"的请求。这套判定由**两部分**组成：
//
//  1. 请求头身份（UA / X-IDE-* / X-Product / X-Agent-Purpose）—— 由本文件的预设负责；
//  2. 提示词里的"身份句 + 品牌"组合 —— 由 sanitize.go 的 sanitizeBrandRe 负责。
//
// 两者是**独立的两层**，缺一层都可能被拦。社区（ardeyouxipianyi/workbuddy2api-hub
// issue #562）给出的临时解法是「用 vsc 或 CLI 出站，暂时不要用 WB」，即切换第 1 层。
//
// 设计取舍：
//   - **默认 identity 为空 = 完全保持历史行为**（UA 走三段式、头组走旧分支），零回归；
//   - 显式配置 `upstream.identity` 才启用预设，且预设内的 UA 与头组**自洽**（同属一套身份）；
//   - 预设只覆盖"身份类"头，其余头（鉴权、设备、业务）不受影响。

// Identity 出站身份枚举值。
const (
	IdentityDefault = ""       // 空 = 历史行为（不启用预设）
	IdentityWB      = "wb"     // WorkBuddy 桌面端
	IdentityVSCode  = "vscode" // VSCode 扩展（CodeBuddy IDE）
	IdentityCLI     = "cli"    // 桌面端内置 CLI
)

// identityPreset 一套自洽的出站身份：UA + 身份头。
type identityPreset struct {
	// ua 出站 User-Agent（空 = 沿用默认三段式）。
	ua string
	// headers 身份类请求头（键值即最终取值）。
	headers map[string]string
}

// vscodeIDEVersion / cliVersionPreset 预设使用的版本号。
// 与 codeBuddyIDEUA / codeBuddyCLIUA 常量保持一致（那两套已被 /v3/config 探测验证可用）。
const (
	vscodeIDEVersion = "4.12.0"
	cliPresetVersion = "2.63.2"
)

// identityPresets 身份预设表。键为 config `upstream.identity` 的取值（大小写不敏感）。
var identityPresets = map[string]identityPreset{
	IdentityWB: {
		ua: "", // 走默认三段式（含 realm 品牌段），避免与既有测试/归因冲突
		headers: map[string]string{
			"X-Agent-Purpose": "conversation",
			"X-IDE-Name":      "WorkBuddy",
			"X-IDE-Type":      "WorkBuddy",
			"X-Product":       "WorkBuddy",
		},
	},
	IdentityVSCode: {
		ua: "CodeBuddyIDE/" + vscodeIDEVersion + " CodeBuddy/" + vscodeIDEVersion,
		headers: map[string]string{
			"X-Agent-Purpose": "conversation",
			"X-IDE-Name":      "CodeBuddyIDE",
			"X-IDE-Type":      "CodeBuddyIDE",
			"X-IDE-Version":   vscodeIDEVersion,
			"X-Product":       "CodeBuddy",
		},
	},
	IdentityCLI: {
		ua: "CLI/" + cliPresetVersion + " CodeBuddy/" + cliPresetVersion,
		headers: map[string]string{
			"X-Agent-Purpose": "conversation",
			"X-Product":       "CodeBuddy",
		},
	},
}

// NormalizeIdentity 归一化身份取值（大小写/空白不敏感）。
//
// 返回 (规范值, 是否合法)：
//   - 空串 → (IdentityDefault, true)：不启用预设，保持历史行为；
//   - 已知别名 → (规范值, true)；
//   - 未知值 → ("", false)：由配置加载阶段报错，避免"配了但没生效"的静默失败。
func NormalizeIdentity(v string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "":
		return IdentityDefault, true
	case "wb", "workbuddy":
		return IdentityWB, true
	case "vscode", "vsc", "ide":
		return IdentityVSCode, true
	case "cli":
		return IdentityCLI, true
	default:
		return "", false
	}
}

// effectiveIdentity 返回本次请求实际生效的身份（空 identity 走默认 WB 形态）。
func (c *Client) effectiveIdentity() string {
	if c == nil {
		return IdentityWB
	}
	canonical, ok := NormalizeIdentity(c.Identity)
	if !ok || canonical == IdentityDefault {
		return IdentityWB // 默认 = 与默认 UA 自洽的 WB 桌面端
	}
	return canonical
}

// PrivateData 取值语义（官方 @tencent-ai/codebuddy-code 解包，见 hub wb_identity.py）：
//
//	X-Private-Data 是「**数据私有**」声明：
//	  true  = 数据私有，不参与模型优化（企业号默认）
//	  false = 允许参与模型优化（个人号默认）
//	注意不要按变量名直读 —— 官方内部字段是 enableModelOptimization，语义正相反。
//
// 各身份的官方默认值：
//   - WB 桌面端：**无条件 true**；
//   - VSCode 扩展：**仅非国内站**发 true（国内站不发此头）；
//   - CLI：企业号 true / 个人号 false。
const (
	PrivateDataAuto  = ""      // 空 / "auto"：按身份默认
	PrivateDataTrue  = "true"  // 数据私有（不参与模型优化）
	PrivateDataFalse = "false" // 允许参与模型优化
)

// NormalizePrivateData 归一化 upstream.private_data 取值。
// 返回 (规范值, 是否合法)："" 表示 auto（按身份默认）。
func NormalizePrivateData(v string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "auto":
		return PrivateDataAuto, true
	case "true", "on", "yes", "1":
		return PrivateDataTrue, true
	case "false", "off", "no", "0":
		return PrivateDataFalse, true
	default:
		return "", false
	}
}

// privateDataValue 计算本次请求的 X-Private-Data 取值（"" = 不发此头）。
//
// 优先级：运维显式声明（true/false 逐字发送）> 身份默认。
func (c *Client) privateDataValue(a *auth.Auth) string {
	if c != nil {
		if v, ok := NormalizePrivateData(c.PrivateData); ok && v != PrivateDataAuto {
			return v
		}
	}
	switch c.effectiveIdentity() {
	case IdentityVSCode:
		// 官方 CraftInvokableAgent 拦截器只在非国内站注入
		if a != nil && a.IsGlobal() {
			return PrivateDataTrue
		}
		return "" // 国内站不发
	case IdentityCLI:
		// 官方 getCustomHeaders：企业号 true、个人号 false（必定发送）
		if a != nil && strings.TrimSpace(a.EnterpriseID) != "" {
			return PrivateDataTrue
		}
		return PrivateDataFalse
	default: // WB 桌面端（含默认）
		return PrivateDataTrue
	}
}

// identityPresetFor 返回当前 Client 生效的身份预设（未配置/未知 → 零值，即不启用）。
func (c *Client) identityPresetFor() (identityPreset, bool) {
	if c == nil || c.Identity == "" {
		return identityPreset{}, false
	}
	canonical, valid := NormalizeIdentity(c.Identity)
	if !valid {
		return identityPreset{}, false
	}
	p, ok := identityPresets[canonical]
	return p, ok
}
