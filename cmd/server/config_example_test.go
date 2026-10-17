package main

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

// T4（审查 E）：config.example.json 与配置结构体的覆盖关系此前只单向
// （TestKnownConfigKeysNoWarn 抓"模板有、结构体没有"）。反向漏写抓不到 ——
// 新增配置项时忘记写模板，用户照模板配就永远用不上新功能。
// 这里用反射遍历结构体的 json tag，断言**每个叶子键**都出现在模板里。
func TestConfigExampleCoversAllKeys(t *testing.T) {
	raw, err := os.ReadFile("../../config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	var example map[string]any
	if err := json.Unmarshal(raw, &example); err != nil {
		t.Fatalf("config.example.json 非法 JSON: %v", err)
	}
	missing := []string{}
	var walk func(rt reflect.Type, prefix string, out map[string]bool)
	walk = func(rt reflect.Type, prefix string, out map[string]bool) {
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			tag := strings.Split(f.Tag.Get("json"), ",")[0]
			if tag == "" || tag == "-" {
				continue
			}
			path := tag
			if prefix != "" {
				path = prefix + "." + tag
			}
			ft := f.Type
			if ft.Kind() == reflect.Ptr {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct && ft.Name() != "Duration" && ft.Name() != "Time" {
				walk(ft, path, out)
				continue
			}
			out[path] = true
		}
	}
	keys := map[string]bool{}
	root := reflect.TypeOf(Default())
	for root.Kind() == reflect.Ptr {
		root = root.Elem()
	}
	walk(root, "", keys)
	for path := range keys {
		segs := strings.Split(path, ".")
		cur := any(example)
		ok := true
		for _, seg := range segs {
			m, isMap := cur.(map[string]any)
			if !isMap {
				ok = false
				break
			}
			v, exists := m[seg]
			if !exists {
				ok = false
				break
			}
			cur = v
		}
		if !ok {
			missing = append(missing, path)
		}
	}
	if len(missing) > 0 {
		t.Errorf("config.example.json 缺少这些配置键（照模板配置就用不上新功能）：%v", missing)
	}
	// 光有键还不够：值必须能被**类型化解析**（否则用户照模板配置会启动即报错）。
	var cfg Config
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if err := dec.Decode(&cfg); err != nil {
		t.Fatalf("config.example.json 无法解析为 Config（值类型不对？）: %v", err)
	}
}
