package upstream

import "strings"

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
