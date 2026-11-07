// Package auth 解析 WorkBuddy auth 文件（嵌套形/扁平形双形态），
// 提供 refresh 后的原子写回。
package auth

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"workbuddy2api/internal/logfmt"
)

// Auth 是归一化后的账号凭证（来源可以是插件 OAuth 嵌套形或手写扁平形）。
type Auth struct {
	// sealed / rawDoc 记录「读入时是桌面端加密文件」的现场：
	// sealed = 被信封包裹的字段路径（回写时按同一批字段复原），
	// rawDoc = 解密后的文档树（回写时以它为底，保住桌面端写入的 uin 等字段）。
	sealed      map[string]bool
	rawDoc      map[string]any
	expiresAtMS bool // 读入时 expiresAt 是否毫秒（桌面端形态）
	// keepRaw 记录"解不开的非凭据信封"原值：回写时原样放回，绝不用空值覆盖。
	keepRaw map[string]any
	// sealedVals 记录从信封解出的明文字面量：回写时一个都不许出现（值级校验）。
	sealedVals map[string]bool
	// inputVals 是**原文件**里的字符串叶子：本来就在明文里的值不算泄漏（差集口径）。
	inputVals map[string]bool

	// mu 串行化 RefreshToken 写与 SaveAtomic 读，防止并发写回半更新 token。
	mu sync.Mutex

	AccessToken  string
	RefreshToken string
	ExpiresAt    int64 // Unix 秒
	Domain       string
	// realm 账号域（"cn" / "global"），落盘于 auth.realm（嵌套形）或顶层 realm（扁平形）。
	// 空 = 缺省：Realm() 按 domain 后缀回落，最终恒非空。
	//
	// 命名注记：Go 不允许字段与方法同名，持久化字段用未导出 realm，计算访问器用
	// 导出的 Realm()（跨包调用全部走方法）。Parse/SaveAtomic/login 在包内读写字段。
	realm        string
	UID          string
	EnterpriseID string
	Nickname     string
	FilePath     string // 来源文件；refresh 后原子写回此处

	// DeviceToken 设备风控 Token（X-Device-Token 头），来源 auth 文件的 device_token 键。
	// 缺省为空 = 不注入该头（容器内无桌面端 Turing SDK 的常见部署）。
	// 手写扁平形 auth 文件可直接写 "device_token": "..."；插件 OAuth 嵌套形
	// 顶层 device_token 也会被解析（与桌面端共用状态文件的部署方式）。
	DeviceToken string
}

// Lock 供同进程内其他包（upstream.RefreshToken）在改写 Auth 字段期间加锁。
func (a *Auth) Lock() { a.mu.Lock() }

// Unlock 释放 a.Lock 获取的锁。
func (a *Auth) Unlock() { a.mu.Unlock() }

// AccessTokenValue 加锁读取 AccessToken（出站请求头一律经此取值，勿直读字段）。
func (a *Auth) AccessTokenValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.AccessToken
}

// DomainValue 加锁读取 Domain（同 AccessTokenValue：RefreshToken 在锁内改写它）。
func (a *Auth) DomainValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Domain
}

// RefreshTokenValue 加锁读取 RefreshToken（同 AccessTokenValue：RefreshToken 在锁内改写它）。
func (a *Auth) RefreshTokenValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.RefreshToken
}

// globalEnabled 全局开关：global realm 是否路由（D5 双保险）。
// 默认开启（与 config global.enabled 缺省 true 一致）：Realm() 正常按显式 realm/
// domain 判定 global/cn。显式 SetGlobalEnabled(false)（config "enabled": false）关闭
// → 逃生门：纯 CN 部署，即便 auth 文件写了 realm=global 或 domain 为 .workbuddy.ai
// 也恒判 cn——「关了才锁死」的单一闸口集中收敛在 Realm()/IsGlobal() 里。
var globalEnabled atomic.Bool

func init() { globalEnabled.Store(true) }

// SetGlobalEnabled 注入 global realm 路由开关（false = 锁死纯 CN，逃生门）。
func SetGlobalEnabled(enabled bool) { globalEnabled.Store(enabled) }

// GlobalEnabled 报告 global realm 路由开关当前状态（测试/运维观测）。
func GlobalEnabled() bool { return globalEnabled.Load() }

// Realm 返回账号的归一化域：显式 Realm=="global" 或 domain 后缀 .workbuddy.ai → "global"，
// 否则 "cn"。显式 global 优先于 domain 回落（D1）。
// 全局开关 SetGlobalEnabled(false) 时恒 "cn"（逃生门：纯 CN 锁定，不影响默认行为）。
// 空 realm + 空 domain → "cn"（老 CN 凭证零回归）。
func (a *Auth) Realm() string {
	if a == nil || !globalEnabled.Load() {
		return "cn"
	}
	if strings.TrimSpace(a.realm) == "global" || isGlobalDomain(a.DomainValue()) {
		return "global"
	}
	return "cn"
}

// ResolveRealm 归一化 realm（cn/global）：显式非空优先，否则按原始 domain 推断
// （isGlobalDomain）。不受逃生门影响（逃生门是路由锁，不应影响标识判定）；
// domain 也为空 → "cn"（老 CN 凭证零回归）。
func ResolveRealm(explicit, domain string) string {
	if r := strings.TrimSpace(explicit); r != "" {
		return r
	}
	if isGlobalDomain(domain) {
		return "global"
	}
	return "cn"
}

// BackfillRealm 为缺省 realm 标识的账号持久化补标识：a.realm 为空时按「原始 domain 推断」
// 写回（cn/global），返回 (是否有变更, 归一化后的 realm)。已有标识不动（幂等）。
func (a *Auth) BackfillRealm() (bool, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if strings.TrimSpace(a.realm) != "" {
		return false, a.realm
	}
	r := ResolveRealm("", a.Domain)
	a.realm = r
	return true, r
}

// RealmStored 直读持久化的 realm 标识（可能为空 = 未 backfill 的旧文件，Realm() 会 fallback）。
func (a *Auth) RealmStored() string {
	if a == nil {
		return ""
	}
	return a.realm
}

// BackfillRealmFor 显式写入 realm 标识（包外登录路径使用：panel login 已知用户选了
// global，直接落盘 realm=global，不依赖 domain 后缀推断）。realm 需为 cn/global，
// 非法值报错（防写脏）。返回是否发生变更。
func BackfillRealmFor(a *Auth, realm string) (bool, error) {
	if a == nil {
		return false, fmt.Errorf("nil auth")
	}
	switch strings.TrimSpace(realm) {
	case "cn", "global":
	default:
		return false, fmt.Errorf("realm must be cn/global, got %q", realm)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.realm == realm {
		return false, nil
	}
	a.realm = realm
	return true, nil
}

// IsGlobal 报告账号是否属于 global realm（= Realm() == "global"）。
func (a *Auth) IsGlobal() bool { return a.Realm() == "global" }

// isGlobalDomain 判定 domain 是否指向国际版域名家族（workbuddy.ai / codebuddy.ai）。
// 同时接受裸域与任意子域（HasSuffix(".workbuddy.ai") 或 ".codebuddy.ai"）。
func isGlobalDomain(d string) bool {
	d = strings.ToLower(strings.TrimSpace(d))
	return d == "workbuddy.ai" || strings.HasSuffix(d, ".workbuddy.ai") ||
		d == "codebuddy.ai" || strings.HasSuffix(d, ".codebuddy.ai")
}

// NeedsRefresh 报告 token 是否将在 within 内过期（或已过期/无 expiry）。
func (a *Auth) NeedsRefresh(within time.Duration) bool {
	if a == nil {
		return true
	}
	a.mu.Lock()
	exp := a.ExpiresAt
	a.mu.Unlock()
	if exp <= 0 {
		return true
	}
	return time.Now().Add(within).Unix() >= exp
}

// Parse 兼容两种磁盘形态：
//
//	嵌套形 {"auth":{...},"account":{...}}  （插件 OAuth 输出）
//	扁平形 {"accessToken":...,"uid":...}   （手写/旧版）
func Parse(raw []byte) (*Auth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty auth storage")
	}
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	// 新版桌面端的 auth 文件用 $wbEncrypted 信封加密若干字段：先解出明文再走原解析。
	origLeaves := jsonStringLeaves(raw)
	plain, sealed, doc, meta, derr := decryptWBEncrypted(raw)
	if derr != nil {
		return nil, fmt.Errorf("storage_decrypt_error: %w", derr)
	}
	raw = plain
	var keepRaw map[string]any
	var sealedVals map[string]bool
	var rawDocForSave map[string]any
	if doc == nil {
		// 明文文件也要做一次类型收敛：桌面端/第三方工具可能把 uid 写成数字，
		// 而本仓结构是强类型（string），不收敛会直接解析失败。
		var m map[string]any
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if dec.Decode(&m) == nil && m != nil {
			rawDocForSave = cloneDoc(m).(map[string]any)
			normalizeDoc(m)
			if b, err := json.Marshal(m); err == nil {
				raw = b
			}
		}
	}
	if doc != nil {
		if meta != nil {
			keepRaw, sealedVals = meta.KeepRaw, meta.SealedVals
		}
		canonicalizeKeys(doc, sealed)
		// 保存用的文档保持**原类型**（不写回规范化后的形态），否则等于擅自改写桌面端文件。
		rawDocForSave = cloneDoc(doc).(map[string]any)
		normalizeDoc(doc)
		if normalized, merr := json.Marshal(doc); merr == nil {
			raw = normalized
		}
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	var a Auth
	if _, nested := probe["auth"]; nested {
		var n struct {
			Auth struct {
				AccessToken  string `json:"accessToken"`
				RefreshToken string `json:"refreshToken"`
				ExpiresAt    int64  `json:"expiresAt"`
				Domain       string `json:"domain"`
				Realm        string `json:"realm"`
			} `json:"auth"`
			Account struct {
				UID          string `json:"uid"`
				EnterpriseID string `json:"enterpriseId"`
				Nickname     string `json:"nickname"`
			} `json:"account"`
			// DeviceToken 顶层 device_token（嵌套形与扁平形共用）。
			// 放在 auth 段之外，手写时无需嵌进 auth 对象，降低配置门槛。
			DeviceToken string `json:"device_token"`
		}
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		a = Auth{
			AccessToken:  n.Auth.AccessToken,
			RefreshToken: n.Auth.RefreshToken,
			ExpiresAt:    n.Auth.ExpiresAt,
			Domain:       n.Auth.Domain,
			realm:        n.Auth.Realm,
			UID:          n.Account.UID,
			EnterpriseID: n.Account.EnterpriseID,
			Nickname:     n.Account.Nickname,
			DeviceToken:  n.DeviceToken,
		}
	} else {
		var f struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    int64  `json:"expiresAt"`
			Domain       string `json:"domain"`
			Realm        string `json:"realm"`
			UID          string `json:"uid"`
			EnterpriseID string `json:"enterpriseId"`
			Nickname     string `json:"nickname"`
			DeviceToken  string `json:"device_token"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		a = Auth{
			AccessToken:  f.AccessToken,
			RefreshToken: f.RefreshToken,
			ExpiresAt:    f.ExpiresAt,
			Domain:       f.Domain,
			realm:        f.Realm,
			UID:          f.UID,
			EnterpriseID: f.EnterpriseID,
			Nickname:     f.Nickname,
			DeviceToken:  f.DeviceToken,
		}
	}
	if strings.TrimSpace(a.AccessToken) == "" {
		return nil, fmt.Errorf("parse_error: missing accessToken")
	}
	// 桌面端 expiresAt 是毫秒；本仓内部一律 Unix 秒（否则有效期会算到几万年后）。
	if a.ExpiresAt > 1e12 {
		a.ExpiresAt /= 1000
		a.expiresAtMS = true // 记住原单位：回写要还原成毫秒，否则桌面端读成 1970 年过期
	}
	a.sealed, a.rawDoc, a.keepRaw, a.sealedVals, a.inputVals = sealed, rawDocForSave, keepRaw, sealedVals, origLeaves
	return &a, nil
}

// SaveAtomic 原子写回 FilePath（tmp + rename），保持读入时的形态（扁平仍扁平、嵌套仍嵌套）。
// 全程持 a.mu：防止与 RefreshToken 修改 token 字段并发，杜绝写回半更新。
// 防御：accessToken 为空时拒绝写回，避免误用空凭证覆盖有效文件。
func (a *Auth) SaveAtomic() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if strings.TrimSpace(a.AccessToken) == "" {
		return fmt.Errorf("save refused: empty accessToken (uid=%s)", a.UID)
	}
	if a.FilePath == "" {
		return fmt.Errorf("no FilePath set")
	}
	// 加密文件：以读入时的文档为底，保住桌面端自己写的字段（uin/phoneNumber 等），
	// 只覆盖本仓负责的字段，最后按同一批路径重新封成信封。
	doc := map[string]any{}
	if a.rawDoc != nil {
		doc = cloneDoc(a.rawDoc).(map[string]any)
	}
	expiresOut := a.ExpiresAt
	if a.expiresAtMS {
		expiresOut *= 1000
	}
	// 形态保持：扁平形态（顶层直接放 accessToken）的文件必须继续按扁平写回，
	// 否则会在旁边新建一个明文 auth 段（审计 R2-1 的 fail-open）。
	flat := a.rawDoc != nil && a.rawDoc["accessToken"] != nil && a.rawDoc["auth"] == nil
	if flat {
		setKeyCI(doc, "accessToken", a.AccessToken)
		setKeyCI(doc, "refreshToken", a.RefreshToken)
		setKeyCI(doc, "expiresAt", expiresOut)
		setKeyCI(doc, "domain", a.Domain)
		setKeyCI(doc, "realm", a.realm)
		if a.UID != "" {
			setKeyCI(doc, "uid", wbRestoreJSONType(doc["uid"], a.UID))
		}
	} else {
		// auth 段按 key 合并（不整体替换）：桌面端写的 userId/tokenType 等未知键必须留住。
		authDoc, _ := doc["auth"].(map[string]any)
		if authDoc == nil {
			authDoc = map[string]any{}
		}
		setKeyCI(authDoc, "accessToken", a.AccessToken)
		setKeyCI(authDoc, "refreshToken", a.RefreshToken)
		setKeyCI(authDoc, "expiresAt", expiresOut)
		setKeyCI(authDoc, "domain", a.Domain)
		setKeyCI(authDoc, "realm", a.realm)
		doc["auth"] = authDoc
	}
	acct, _ := doc["account"].(map[string]any)
	if acct == nil {
		acct = map[string]any{}
	}
	setKeyCI(acct, "uid", wbRestoreJSONType(acct["uid"], a.UID))
	setKeyCI(acct, "enterpriseId", wbRestoreJSONType(acct["enterpriseId"], a.EnterpriseID))
	setKeyCI(acct, "nickname", wbRestoreJSONType(acct["nickname"], a.Nickname))
	doc["account"] = acct
	// DeviceToken 非空才写回顶层 device_token：避免在无该字段的旧文件里引入空键
	// （保持与插件 OAuth 输出形状一致，插件读取忽略未知键）。
	if a.DeviceToken != "" {
		doc["device_token"] = a.DeviceToken
	}
	// 解不开的非凭据信封：原样放回（否则会被空值覆盖，原密文永久丢失）。
	for path, env := range a.keepRaw {
		parts := strings.Split(path, ".")
		switch len(parts) {
		case 1:
			doc[parts[0]] = env
		case 2:
			if parent, ok := doc[parts[0]].(map[string]any); ok {
				parent[parts[1]] = env
			} else {
				continue
			}
		default:
			continue
		}
		if a.sealed == nil {
			a.sealed = map[string]bool{}
		}
		a.sealed[path] = true
	}
	if err := sealDocFields(doc, a.sealed); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	// 正向校验（与信封形态无关）：加密文件回写后，序列化结果里绝不允许出现凭据明文。
	// 这一道兜住"记录在案的路径没被封上"的所有形态（容器级、根级、未来新字段）。
	outLeaves := jsonStringLeaves(raw)
	for val := range a.sealedVals {
		// 差集口径：出现在输出、且**原文件里本来没有**的密封值 = 泄漏。
		// 用集合相等而不是子串匹配：JSON 会把 < > & 转义成 \u003c 等，子串比对会漏判。
		if outLeaves[val] && !a.inputVals[val] {
			return fmt.Errorf("save refused: 加密文件重封失败（凭据会以明文落盘，uid=%s）", a.UID)
		}
	}
	tmp := a.FilePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, a.FilePath); err != nil {
		// 单文件挂载、跨设备或 Windows 句柄占用时 rename 失败，回退为直接覆盖写
		if werr := os.WriteFile(a.FilePath, raw, 0o600); werr != nil {
			return fmt.Errorf("rename %w, write fallback %w", err, werr)
		}
		_ = os.Remove(tmp)
	}
	return nil
}

// AuthFileGlob auth 文件的统一 glob 模式（宽侧：workbuddy*.json）。
const AuthFileGlob = "workbuddy*.json"

// LoadAuthFiles 返回 dir 下按 AuthFileGlob 匹配的 auth 文件清单（已排序）。
func LoadAuthFiles(dir string) ([]string, error) {
	files, err := filepath.Glob(filepath.Join(dir, AuthFileGlob))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}

// LoadDir 扫描并解析 dir 下 workbuddy*.json；解析失败的文件静默跳过（启动日志由调用方统计）。
// 顺带做 realm 标识存量迁移：对空 realm 的 auth 自动 backfill（原始 domain 推断）并 SaveAtomic
// 落盘，一次性把旧文件补上 realm 键。单个文件写失败不阻断启动（log WARN 继续），
// 避免历史 auth 目录个别文件不可写时整个服务起不来。
func LoadDir(dir string) ([]*Auth, error) {
	files, err := LoadAuthFiles(dir)
	if err != nil {
		return nil, err
	}
	// seenUID 重复 UID 检测：同 UID 出现在多个文件时（双 realm 同名 UID 概率近零）
	// 打 WARN 告警含两文件路径，由「后载入者胜出」保持现状行为（不改变加载结果）。
	seenUID := make(map[string]string, len(files))
	var out []*Auth
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := Parse(raw)
		if err != nil {
			continue
		}
		a.FilePath = f
		if prev, ok := seenUID[a.UID]; ok {
			log.Printf("WARN: uid %s duplicated across %s and %s — 后者覆盖（不同 realm 同名 UID？）",
				logfmt.UID8(a.UID), prev, f)
		}
		seenUID[a.UID] = f
		if a.RealmStored() == "" {
			if changed, r := a.BackfillRealm(); changed {
				if err := a.SaveAtomic(); err != nil {
					log.Printf("WARN: auth %s realm backfill save: %v", logfmt.UID8(a.UID), err)
				} else if r == "global" {
					log.Printf("auth %s 存量迁移: 补 realm=global（domain=%s）", logfmt.UID8(a.UID), a.Domain)
				}
			}
		}
		out = append(out, a)
	}
	return out, nil
}

// IsEnterprise 报告该账号是否为企业号（移植自上游 panel/main:internal/auth/auth.go:210）。
func (a *Auth) IsEnterprise() bool { return strings.TrimSpace(a.EnterpriseID) != "" }

// cloneDoc 深拷贝文档树：SaveAtomic 只改副本，绝不就地改 a.rawDoc
// （否则"第一次保存的副作用"会变成后续保存的输入，审计 A3）。
func cloneDoc(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, item := range t {
			out[k] = cloneDoc(item)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = cloneDoc(item)
		}
		return out
	}
	return v
}

// wbRestoreJSONType 保留原始 JSON 类型：桌面端把 uid 写成数字时，
// 回写不能擅自改成字符串（那是改写别人的文件）。
func wbRestoreJSONType(orig any, value string) any {
	switch orig.(type) {
	case json.Number, float64:
		if value != "" {
			if _, err := strconv.ParseInt(value, 10, 64); err == nil {
				return json.Number(value)
			}
		}
	}
	return value
}

// setKeyCI 大小写不敏感地写入：桌面端可能写成 AccessToken / ACCESSTOKEN，
// 固定写规范小写会留下两份键（其中一份还是明文），所以优先复用原键名。
func setKeyCI(m map[string]any, canonical string, value any) {
	for k := range m {
		if strings.EqualFold(k, canonical) {
			m[k] = value
			return
		}
	}
	m[canonical] = value
}
