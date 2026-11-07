package auth

// wbencrypted.go 解密/重封新版桌面端 auth 文件里的 $wbEncrypted 字段。
//
// 信封形态（对 WorkBuddy 桌面端逆向所得，与 misakano7545/workbuddy2api-panel 的
// 实现一致）：
//
//	{"$wbEncrypted":1,"envelope":"base64(JSON{suite,keyId,nonce,authTag,ciphertext})"}
//
// key = sha256(utf8(at-rest secret))；AES-256-GCM；AAD 为固定二进制拼接；
// keyId = sha256(key) 的 hex 前 16 位，用来确认信封出自同一把密钥（对不上就报错，
// 不猜）。没有 $wbEncrypted 的文件原样返回，零成本。

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// wbAtRestSecretKey 桌面端 at-rest 静态密钥（canonical base64）。
// 桌面端换密钥时 keyId 校验会失败并显式报错。
const wbAtRestSecretKey = "Sik9U5aXhCdwTVEwsEySDOmDoB9r9ntFxHF1fst9LQI="

func wbKey() [32]byte { return sha256.Sum256([]byte(wbAtRestSecretKey)) }

func wbKeyID(key [32]byte) string {
	h := sha256.Sum256(key[:])
	return hex.EncodeToString(h[:])[:16]
}

// wbFieldAAD 字段级 AAD（framing=2）。任何一个字节不同都会解密失败。
func wbFieldAAD(suite uint32, keyID string) []byte {
	var b bytes.Buffer
	b.WriteString("WB-AAD" + string(rune(0)))
	b.WriteByte(1)
	wbWriteLP(&b, "WBEV1")
	wbWriteLP(&b, "sym-v1")
	var u [4]byte
	binary.BigEndian.PutUint32(u[:], suite)
	b.Write(u[:])
	wbWriteLP(&b, keyID)
	b.WriteByte(2) // framing: file=1 field=2 record=3 stream=4
	b.WriteByte(0) // 无 sequence
	b.WriteByte(0) // 无 final
	return b.Bytes()
}

func wbWriteLP(b *bytes.Buffer, s string) {
	var u [4]byte
	binary.BigEndian.PutUint32(u[:], uint32(len(s)))
	b.Write(u[:])
	b.WriteString(s)
}

// decryptWBEncrypted 解开 raw 里所有信封字段。
// 返回明文 JSON、被封装字段的路径集合（供回写复原）、以及解出的文档树。
func decryptWBEncrypted(raw []byte) (plain []byte, sealed map[string]bool, doc map[string]any, meta *wbDocMeta, err error) {
	if !bytes.Contains(raw, []byte("\"$wbEncrypted\"")) {
		return raw, nil, nil, nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // 超长数字形态的凭据不能经 float64 中转（会丢精度且不可逆）
	if err := dec.Decode(&doc); err != nil {
		return raw, nil, nil, nil, nil // 交给调用方按原路径报解析错误
	}
	sealed = map[string]bool{}
	key := wbKey()
	keepRaw := map[string]any{}
	sealedVals := map[string]bool{}
	v, changed, err := wbDecryptValue(doc, key, "", sealed, keepRaw, sealedVals)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if !changed {
		// 没有任何字段被解开（例如信封只在数组里）：仍把文档交回去，
		// 否则保存时会以空文档为底，把 accounts 数组整段丢掉。
		return raw, nil, doc, nil, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return raw, nil, nil, nil, nil
	}

	plain, err = json.Marshal(m)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("at_rest_marshal_failed: %w", err)
	}
	meta = &wbDocMeta{KeepRaw: keepRaw, SealedVals: sealedVals}
	return plain, sealed, m, meta, nil
}

func wbDecryptValue(v any, key [32]byte, prefix string, sealed map[string]bool, keepRaw map[string]any, sealedVals map[string]bool) (any, bool, error) {
	switch t := v.(type) {
	case map[string]any:
		if wbIsOne(t["$wbEncrypted"]) {
			if env, ok := t["envelope"].(string); ok && env != "" {
				plainVal, err := wbDecryptEnvelope(env, key)
				if err != nil {
					if !wbCredentialPath(prefix) {
						// 非凭据字段（nickname/phoneNumber/头像…）坏掉：不能拖垮整个账号，
						// 也不能用"空值信封"覆盖原密文（那样原值再也拿不回来）。
						// 记下原信封，回写时原样放回去。
						if keepRaw != nil {
							keepRaw[prefix] = t
						}
						return nil, true, nil
					}
					return nil, false, err
				}
				// 根级信封（整文件就是一个信封）也要记录：调用方据此拒绝写盘，
				// 否则重封无从下手，只能把明文写回去。
				sealed[prefix] = true
				// 值级校验依据：这段明文必须**不出现在**序列化结果里（键名拼写无关）。
				if sealedVals != nil {
					for _, txt := range wbTextForms(plainVal) {
						sealedVals[txt] = true
					}
				}
				return plainVal, true, nil
			}
		}
		changed := false
		for k, item := range t {
			childPath := wbJoinPath(prefix, k)
			// 只处理到两层（"auth.accessToken" / "device_token"）：更深的路径没有可寻址的
			// 父容器可回写，就地解密会把明文留进 rawDoc 而封装阶段又跳不过去 —— 等于明文落盘。
			if wbPathDepth(childPath) > 2 {
				continue
			}
			nv, c, err := wbDecryptValue(item, key, childPath, sealed, keepRaw, sealedVals)
			if err != nil {
				return nil, false, err
			}
			if c {
				t[k] = nv
				changed = true
			}
		}
		return t, changed, nil
	case []any:
		// 数组元素（accounts[0].token 这类）本仓不回写、也没有可寻址的父容器：
		// 就地解密会把明文留在数组里、回写时又封不回去，反而制造泄漏面。原样保留。
		return t, false, nil
	}
	return v, false, nil
}

// wbCredentialPath 报告该路径是否属于「必须解开，解不开就算故障」的凭据字段。
// 根（整文件信封）与 auth 容器按凭据对待：其中可能含 access/refresh token。
func wbCredentialPath(prefix string) bool {
	switch prefix {
	case "", "auth", "auth.accessToken", "auth.refreshToken", "auth.expiresAt", "device_token":
		return true
	}
	return false
}

// normalizeDoc 把解密后文档里的已知字段收敛成本仓 Go 类型。
//
// 为什么需要：信封里的明文是"桌面端怎么写就怎么存"（字符串可能是裸文本，也可能是
// JSON 数字/布尔），而本仓 Auth 结构是强类型。在解析前统一收敛，既避免
// "13800000000 变成 number 导致 string 字段解析失败"，也避免反过来。
func normalizeDoc(doc map[string]any) {
	asString := func(v any) (string, bool) {
		switch t := v.(type) {
		case string:
			return t, true
		case json.Number:
			return t.String(), true // 原样数字串，不经 float64（保精度）
		case float64:
			return strconv.FormatFloat(t, 'f', -1, 64), true
		case bool:
			return strconv.FormatBool(t), true
		}
		return "", false
	}
	if authObj, ok := doc["auth"].(map[string]any); ok {
		for _, k := range []string{"accessToken", "refreshToken", "domain", "realm"} {
			if v, ok := authObj[k]; ok {
				if s, ok := asString(v); ok {
					authObj[k] = s
				}
			}
		}
		if v, ok := authObj["expiresAt"]; ok {
			switch t := v.(type) {
			case string:
				if n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64); err == nil {
					authObj["expiresAt"] = json.Number(strconv.FormatInt(n, 10))
				}
			case float64:
				authObj["expiresAt"] = json.Number(strconv.FormatFloat(t, 'f', -1, 64))
			}
		}
	}
	if acct, ok := doc["account"].(map[string]any); ok {
		for _, k := range []string{"uid", "enterpriseId", "nickname"} {
			if v, ok := acct[k]; ok {
				if s, ok := asString(v); ok {
					acct[k] = s
				}
			}
		}
	}
	for _, k := range []string{"uid", "enterpriseId", "nickname", "device_token"} {
		if v, ok := doc[k]; ok {
			if sv, ok := asString(v); ok {
				doc[k] = sv
			}
		}
	}
	if v, ok := doc["expiresAt"]; ok {
		switch t := v.(type) {
		case string:
			if n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64); err == nil {
				doc["expiresAt"] = json.Number(strconv.FormatInt(n, 10))
			}
		case float64:
			doc["expiresAt"] = json.Number(strconv.FormatFloat(t, 'f', -1, 64))
		}
	}
}

// wbIsOne 判断信封标记是否为 1（UseNumber 后是 json.Number，旧路径可能是 float64）。
func wbIsOne(v any) bool {
	switch t := v.(type) {
	case float64:
		return t == 1
	case json.Number:
		if f, err := t.Float64(); err == nil {
			return f == 1
		}
		return false
	case bool:
		return t
	case string:
		t = strings.TrimSpace(t)
		return t == "1" || strings.EqualFold(t, "true")
	}
	return false
}

// wbDocMeta 解密附带的现场信息（不走 doc，避免被文件内容注入）。
type wbDocMeta struct {
	// KeepRaw：解不开的非凭据信封原值（路径 → 原信封），回写时原样放回。
	KeepRaw map[string]any
	// SealedVals：从信封解出的明文字面量，回写后一个都不许出现。
	SealedVals map[string]bool
}

// canonicalFields 本仓会读写的标准键名。
var canonicalFields = []string{
	"accessToken", "refreshToken", "expiresAt", "domain", "realm",
	"uid", "uin", "nickname", "enterpriseId", "device_token",
}

// canonicalizeKeys 把大小写变体归一到标准键名，并去重。
//
// 为什么必须做：桌面端/第三方工具可能写成 AccessToken，而本仓写回固定用规范小写，
// 于是同一对象里出现两份键、值还不一致 —— 明文会从另一份键漏出去。
// 去重优先保留：精确规范键 > 信封值 > 第一个命中；sealed 路径同步改写。
func canonicalizeKeys(doc map[string]any, sealed map[string]bool) {
	canon := func(m map[string]any, parent string) {
		for _, name := range canonicalFields {
			var hits []string
			for k := range m {
				if strings.EqualFold(k, name) {
					hits = append(hits, k)
				}
			}
			if len(hits) == 0 {
				continue
			}
			// 优先级：精确规范键 > 读入时确实是信封的键（sealed 路径为证）> 第一个命中。
			// 注意此刻信封已被解开，所以判据必须用 sealed 路径，而不是"值是不是信封"。
			target := hits[0]
			for _, h := range hits {
				if h == name {
					target = h
				}
			}
			for _, h := range hits {
				if sealed != nil && sealed[wbChildPath(parent, h)] {
					target = h
					break
				}
			}
			for _, h := range hits {
				if h == target {
					continue
				}
				// 删除重复键前先把它身上的 sealed 标记**转移**到保留键：
				// 否则"信封在变体键、明文在规范键"的畸形文件会丢掉加密标记，
				// 保存时把轮换后的新 token 明文写回。
				if sealed != nil && sealed[wbChildPath(parent, h)] {
					sealed[wbChildPath(parent, target)] = true
					delete(sealed, wbChildPath(parent, h))
				}
				delete(m, h)
			}
			if target != name {
				m[name] = m[target]
				delete(m, target)
				if sealed != nil {
					if sealed[wbChildPath(parent, target)] {
						delete(sealed, wbChildPath(parent, target))
						sealed[wbChildPath(parent, name)] = true
					}
				}
			}
		}
	}
	canon(doc, "")
	if authObj, ok := doc["auth"].(map[string]any); ok {
		canon(authObj, "auth")
	}
	if acct, ok := doc["account"].(map[string]any); ok {
		canon(acct, "account")
	}
}

// wbTextForms 把解出的明文转成"可能的文本形态"集合：字符串原样，
// 数字/布尔按文本（数字密文回写时同样要能比对到）。
func wbTextForms(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case json.Number:
		return []string{t.String()}
	case float64:
		return []string{strconv.FormatFloat(t, 'f', -1, 64)}
	case bool:
		return []string{strconv.FormatBool(t)}
	}
	return nil
}

// jsonStringLeaves 收集文档里所有字符串叶子（精确值集合）。
// 用于值级校验的"差集"口径：密封值只要出现在输出、且输入里本来没有，就算泄漏。
// 用集合相等而不是子串匹配 —— 后者会被 JSON 转义（< > & → \u003c）弄成 false negative。
func jsonStringLeaves(raw []byte) map[string]bool {
	out := map[string]bool{}
	if len(raw) == 0 {
		return out
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if dec.Decode(&v) != nil {
		return out
	}
	var walk func(any)
	walk = func(x any) {
		switch t := x.(type) {
		case string:
			out[t] = true
		case map[string]any:
			for _, e := range t {
				walk(e)
			}
		case []any:
			for _, e := range t {
				walk(e)
			}
		}
	}
	walk(v)
	return out
}

func wbChildPath(parent, key string) string {
	if parent == "" {
		return key
	}
	return parent + "." + key
}

// wbPathDepth 返回点分路径的段数（"auth"=1，"auth.accessToken"=2）。
func wbPathDepth(path string) int {
	if path == "" {
		return 0
	}
	return strings.Count(path, ".") + 1
}

func wbJoinPath(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

func wbDecryptEnvelope(envelopeB64 string, key [32]byte) (any, error) {
	rawEnv, err := base64.StdEncoding.DecodeString(envelopeB64)
	if err != nil {
		return nil, fmt.Errorf("at_rest_bad_envelope: %w", err)
	}
	var env struct {
		Suite      uint32 `json:"suite"`
		KeyID      string `json:"keyId"`
		Nonce      string `json:"nonce"`
		AuthTag    string `json:"authTag"`
		Ciphertext string `json:"ciphertext"`
	}
	if err := json.Unmarshal(rawEnv, &env); err != nil {
		return nil, fmt.Errorf("at_rest_bad_envelope: %w", err)
	}
	if env.Suite != 1 {
		// 未来格式：显式拒绝，不按 suite=1 硬解、也不在回写时静默降级。
		return nil, fmt.Errorf("at_rest_unsupported_suite: suite=%d（仅支持 1）", env.Suite)
	}
	if want := wbKeyID(key); env.KeyID != want {
		return nil, fmt.Errorf("at_rest_key_mismatch: envelope keyId=%s want %s（桌面端换了 at-rest 密钥）", env.KeyID, want)
	}
	nonce, tag, ct, err := wbB64Triple(env.Nonce, env.AuthTag, env.Ciphertext)
	if err != nil {
		return nil, err
	}
	// 长度必须是 GCM 要求的形状：否则 crypto/cipher 会 **panic**，而解密发生在
	// 启动的 LoadDir 里 —— 一个坏文件就能把网关变成启动崩溃循环。
	if len(nonce) != 12 {
		return nil, fmt.Errorf("at_rest_bad_envelope: nonce 长度 %d（需 12）", len(nonce))
	}
	if len(tag) != 16 {
		return nil, fmt.Errorf("at_rest_bad_envelope: authTag 长度 %d（需 16）", len(tag))
	}
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, fmt.Errorf("at_rest_cipher_failed: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("at_rest_gcm_failed: %w", err)
	}
	plain, err := gcm.Open(nil, nonce, append(ct, tag...), wbFieldAAD(env.Suite, env.KeyID))
	if err != nil {
		return nil, fmt.Errorf("at_rest_decrypt_failed: %w", err)
	}
	return wbDecodePlain(plain), nil
}

func wbB64Triple(nonce, tag, ct string) (n, tg, c []byte, err error) {
	for _, f := range []struct {
		name string
		in   string
		out  *[]byte
	}{{"nonce", nonce, &n}, {"authTag", tag, &tg}, {"ciphertext", ct, &c}} {
		v, e := base64.StdEncoding.DecodeString(f.in)
		if e != nil {
			return nil, nil, nil, fmt.Errorf("at_rest_bad_envelope: %s: %w", f.name, e)
		}
		*f.out = v
	}
	return n, tg, c, nil
}

// wbDecodePlain 明文可能是纯文本、JSON、或 Buffer 形态的 JSON（{"type":"Buffer","data":[…]}）。
func wbDecodePlain(plain []byte) any {
	var v any
	dec := json.NewDecoder(bytes.NewReader(plain))
	dec.UseNumber() // 纯数字明文（超长 token）不能经 float64 中转
	if dec.Decode(&v) != nil {
		return string(plain)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err == nil || err != io.EOF {
		// 尾部还有残留：不能截断成前缀（"7abc123"→7 会被封进信封永久化）。
		return string(plain)
	}
	v = wbUnwrapBuffer(v)
	if s, ok := v.(string); ok {
		var again any
		if json.Unmarshal([]byte(s), &again) == nil {
			return wbUnwrapBuffer(again)
		}
	}
	return v
}

func wbUnwrapBuffer(v any) any {
	switch t := v.(type) {
	case map[string]any:
		if t["type"] == "Buffer" {
			if arr, ok := t["data"].([]any); ok {
				buf := make([]byte, 0, len(arr))
				for _, n := range arr {
					switch v := n.(type) {
					case float64:
						buf = append(buf, byte(v))
					case json.Number:
						i, err := v.Int64()
						if err != nil {
							return t
						}
						buf = append(buf, byte(i))
					default:
						return t
					}
				}
				return string(buf)
			}
		}
		for k, item := range t {
			t[k] = wbUnwrapBuffer(item)
		}
		return t
	case []any:
		for i, item := range t {
			t[i] = wbUnwrapBuffer(item)
		}
		return t
	}
	return v
}

// sealWBValue 把一个值重新封成信复（SaveAtomic 回写时用）。
func sealWBValue(v any) (map[string]any, error) {
	text, ok := v.(string)
	if !ok {
		b, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("at_rest_seal_failed: %w", err)
		}
		text = string(b)
	}
	key := wbKey()
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, fmt.Errorf("at_rest_cipher_failed: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("at_rest_gcm_failed: %w", err)
	}
	keyID := wbKeyID(key)
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("at_rest_seal_failed: %w", err)
	}
	ct := gcm.Seal(nil, nonce, []byte(text), wbFieldAAD(1, keyID))
	env, err := json.Marshal(map[string]any{
		"suite":      1,
		"keyId":      keyID,
		"nonce":      base64.StdEncoding.EncodeToString(nonce),
		"authTag":    base64.StdEncoding.EncodeToString(ct[len(ct)-16:]),
		"ciphertext": base64.StdEncoding.EncodeToString(ct[:len(ct)-16]),
	})
	if err != nil {
		return nil, fmt.Errorf("at_rest_seal_failed: %w", err)
	}
	return map[string]any{
		"$wbEncrypted": 1,
		"envelope":     base64.StdEncoding.EncodeToString(env),
	}, nil
}

// sealDocFields 按读入时记录的路径把 doc 里对应字段重新封装。
//
// 路径形态：""（整文件信封）、"device_token"（顶层标量）、"auth"（整个对象）、
// "auth.accessToken"（字段级）。上一级已经封成信封时，下级路径自动跳过。
// **任何记录在案却没能重新封上的凭据路径都报错**（fail-closed），绝不静默跳过 ——
// 静默跳过等于把 at-rest 加密静默降级成明文。
func sealDocFields(doc map[string]any, sealed map[string]bool) error {
	if len(sealed) == 0 {
		return nil
	}
	if sealed[""] {
		return fmt.Errorf("at_rest_seal_failed: 整文件信封无法安全重封，拒绝写盘")
	}
	paths := make([]string, 0, len(sealed))
	for p := range sealed {
		paths = append(paths, p)
	}
	// 浅路径先处理：容器级封完后，其下级路径自然跳过。
	sort.Slice(paths, func(i, j int) bool {
		return strings.Count(paths[i], ".") < strings.Count(paths[j], ".")
	})
	done := map[string]bool{}
	for _, path := range paths {
		skip := false
		for d := range done {
			if path == d || strings.HasPrefix(path, d+".") {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		parts := strings.Split(path, ".")
		switch len(parts) {
		case 1:
			v, ok := doc[parts[0]]
			if !ok || v == nil {
				continue
			}
			if isWBEnvelope(v) {
				done[path] = true
				continue
			}
			env, err := sealWBValue(v)
			if err != nil {
				return err
			}
			doc[parts[0]] = env
			done[path] = true
		case 2:
			parent, ok := doc[parts[0]].(map[string]any)
			if !ok {
				// 父容器不存在 = 这次回写根本没写这块内容，无需封装。
				continue
			}
			v, ok := parent[parts[1]]
			if !ok || v == nil {
				continue
			}
			if isWBEnvelope(v) {
				done[path] = true
				continue
			}
			env, err := sealWBValue(v)
			if err != nil {
				return err
			}
			parent[parts[1]] = env
			done[path] = true
		default:
			// 三段以上/数组内路径：本仓不回写这些容器，跳过即可。
			// 报错会让"刷新后的 token 永远落不了盘"，比不封更糟；
			// 明文泄漏由 SaveAtomic 的正向校验兜底。
			continue
		}
	}
	return nil
}

func isWBEnvelope(v any) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	_, ok = m["$wbEncrypted"]
	return ok
}
