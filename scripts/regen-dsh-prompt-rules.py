#!/usr/bin/env python3
# regen-dsh-prompt-rules.py —— 从 DSH 客户端 app.asar 重新生成 sanitize_dsh_prompt.go。
#
# 为什么需要脚本而不是手抄：这些句子是上游逐字指纹的原料，任何抄写误差都会让规则永不命中，
# 而规则「看起来一样」却失效是最难发现的回归（2026-10-27 已踩过一次）。
# 脚本按**字节**从 asar 提取常量 → 断言每条替换真的改变内容 → 生成 Go 规则 + 回归夹具。
#
# 用法： python regen-dsh-prompt-rules.py [asar路径]
# 产物： internal/upstream/sanitize_dsh_prompt.go
#        internal/upstream/testdata/dsh_prompt_paragraphs.txt
import sys
import os
p = sys.argv[1] if len(sys.argv) > 1 else r'D:\\Program Files (x86)\\dsh_Desktop\\resources\\app.asar'
data = open(p,'rb').read()

def const(name):
    i = data.find(('const %s = `' % name).encode())
    assert i >= 0, name
    st = data.find(b'`', i) + 1
    j = st
    while True:
        j = data.find(b'`', j)
        assert j > 0
        if data[j-1:j] != b'\\':
            break
        j += 1
    return data[st:j].decode('utf-8')

src = const('HARNESS_SOURCE_PROMPT')
web = const('WEB_SURFACE_PROMPT')
legacy = 'The DeepSeek Harness implementation checkout is at '
print('lens', len(src), len(web), 'web_has_boot', '__DSH_BOOT__' in web)

specs = [
    ('the DeepSeek Harness implementation checkout', 'the implementation checkout'),
    ('through the DeepSeek Harness Web GUI', 'through the Web GUI'),
    ('extend DSH itself', 'extend this implementation itself'),
    ('only dsh web injects window.__DSH_BOOT__', 'only the web launcher injects the boot payload'),
    (legacy, 'The implementation checkout is at '),
]
used = []
for a,b in specs:
    if (a in src) or (a in web) or (a in legacy):
        used.append((a,b))
print('used', len(used))
assert len(used) == 5, [a for a,b in specs if a not in src and a not in web and a not in legacy]

out_dir = os.path.join(os.path.dirname(os.path.abspath(__file__)), '..', 'internal', 'upstream')
os.makedirs(os.path.join(out_dir,'testdata'), exist_ok=True)
fixture = src + '\n\n' + web + '\n\n' + legacy + '<CHECKOUT>.'
open(os.path.join(out_dir,'testdata','dsh_prompt_paragraphs.txt'),'w',encoding='utf-8',newline='').write(fixture+'\n')

def gq(s):
    return '"' + s.replace('\\','\\\\').replace('"','\\"') + '"'

L=['// sanitize_dsh_prompt.go —— DSH 提示词里「DeepSeek 关联度最高」的身份句改写（不损伤语义）。',
'//',
'// 来源：app.asar 的 HARNESS_SOURCE_PROMPT / WEB_SURFACE_PROMPT 常量与 LEGACY 前缀，按原始字节提取。',
'// 回归夹具：testdata/dsh_prompt_paragraphs.txt（同一份原文）。',
'//',
'// 替换只做「删产品名」「同义换词」，指代不变：',
'//   the DeepSeek Harness implementation checkout -> the implementation checkout',
'//   through the DeepSeek Harness Web GUI        -> through the Web GUI',
'//   extend DSH itself                           -> extend this implementation itself',
'//   only dsh web injects window.__DSH_BOOT__    -> only the web launcher injects the boot payload',
'package upstream','',
'// dshPromptRewrites DSH 提示词身份句改写（精确匹配，顺序执行）。',
'var dshPromptRewrites = [][2]string{']
for a,b in used:
    L += ['\t{', '\t\t' + gq(a) + ',', '\t\t' + gq(b) + ',', '\t},']
L += ['}','',
'// dshPromptProbes 预检锚点：命中任意一条才跑上面的规则（普通请求零开销）。',
'var dshPromptProbes = []string{',
'\t"DeepSeek Harness",',
'\t"DSH itself",',
'\t"window.__DSH_BOOT__",',
'}','']
open(os.path.join(out_dir,'sanitize_dsh_prompt.go'),'w',encoding='utf-8',newline='').write('\n'.join(L))
print('wrote; fixture len', len(fixture))