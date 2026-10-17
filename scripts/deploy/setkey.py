#!/usr/bin/env python3
"""setkey.py —— 把 api_key 写入 config.json（原子替换）。

为什么单独成文件（2026-10-03，第九轮审计 A2）：
  原先该逻辑内联在 wb2api.sh 的 `python3 -c` 里，密钥会出现在 shell/ps 的命令行参数中，
  Docker 分支还把密钥**插值进 python 源码**（含引号即注入）。现在密钥只经环境变量
  `WB2A_NEWKEY` 传入，且写入走 tmp + os.replace（中断不会留下半截 config.json）。
"""
import json
import os
import sys

path = sys.argv[1] if len(sys.argv) > 1 else "config.json"
key = os.environ.get("WB2A_NEWKEY", "")
if not key:
    print("ERR: 环境变量 WB2A_NEWKEY 为空", file=sys.stderr)
    sys.exit(2)
with open(path, encoding="utf-8") as f:
    cfg = json.load(f)
cfg["api_key"] = key
tmp = path + ".new"
with open(tmp, "w", encoding="utf-8") as f:
    json.dump(cfg, f, indent=2, ensure_ascii=False)
    f.flush()
    os.fsync(f.fileno())
os.replace(tmp, path)
print("api_key 已写入 %s" % path)
