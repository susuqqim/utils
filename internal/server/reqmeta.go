package server

import (
	"context"
	"net/http"
)

// reqMeta 承载「请求级」诊断信息：处理函数写入，采集中间件读出。
//
// 为什么不用全局/包级状态：并发请求会串台；为什么不用改函数签名：
// reqlog 采集在中间件里，处理函数没有返回值可传，context 是唯一自然的通道。
type reqMeta struct {
	// ModelEff 是实际出站模型（已剥 cn:/global: 前缀）。
	ModelEff string
	// EffortEff 是实际出站的思考档位（可能被上游能力表降级）。
	EffortEff string
}

type reqMetaKey struct{}

// withReqMeta 把空的 reqMeta 挂到请求上，返回新请求与写入句柄。
func withReqMeta(r *http.Request) (*http.Request, *reqMeta) {
	m := &reqMeta{}
	return r.WithContext(context.WithValue(r.Context(), reqMetaKey{}, m)), m
}

// reqMetaOf 取当前请求的 reqMeta（未挂载时返回 nil，调用方需判空）。
func reqMetaOf(ctx context.Context) *reqMeta {
	m, _ := ctx.Value(reqMetaKey{}).(*reqMeta)
	return m
}
