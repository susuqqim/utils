// sanitize_dsh_prompt.go —— DSH 提示词里「DeepSeek 关联度最高」的身份句改写（不损伤语义）。
//
// 来源：app.asar 的 HARNESS_SOURCE_PROMPT / WEB_SURFACE_PROMPT 常量与 LEGACY 前缀，按原始字节提取。
// 回归夹具：testdata/dsh_prompt_paragraphs.txt（同一份原文）。
//
// 替换只做「删产品名」「同义换词」，指代不变：
//
//	the DeepSeek Harness implementation checkout -> the implementation checkout
//	through the DeepSeek Harness Web GUI        -> through the Web GUI
//	extend DSH itself                           -> extend this implementation itself
//	only dsh web injects window.__DSH_BOOT__    -> only the web launcher injects the boot payload
package upstream

// dshPromptRewrites DSH 提示词身份句改写（精确匹配，顺序执行）。
var dshPromptRewrites = [][2]string{
	{
		"the DeepSeek Harness implementation checkout",
		"the implementation checkout",
	},
	{
		"through the DeepSeek Harness Web GUI",
		"through the Web GUI",
	},
	{
		"extend DSH itself",
		"extend this implementation itself",
	},
	{
		"only dsh web injects window.__DSH_BOOT__",
		"only the web launcher injects the boot payload",
	},
	{
		"The DeepSeek Harness implementation checkout is at ",
		"The implementation checkout is at ",
	},
}

// dshPromptProbes 预检锚点：命中任意一条才跑上面的规则（普通请求零开销）。
var dshPromptProbes = []string{
	"DeepSeek Harness",
	"DSH itself",
	"window.__DSH_BOOT__",
}
