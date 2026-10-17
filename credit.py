# -*- coding: utf-8 -*-
"""WorkBuddy 账号池实时额度查询与排版展示"""
import subprocess
import json
import sys
import os

def get_credit_json():
    # 优先从运行中的 docker 容器查
    try:
        proc = subprocess.run(
            ["docker", "exec", "-w", "/app", "workbuddy2api", "/app/credit"],
            capture_output=True,
            text=True,
            encoding="utf-8"
        )
        if proc.returncode == 0 and proc.stdout.strip():
            return proc.stdout.strip()
    except Exception:
        pass

    # 兜底使用本地 credit.exe
    exe_path = os.path.join(os.path.dirname(__file__), "credit.exe")
    if os.path.exists(exe_path):
        try:
            proc = subprocess.run(
                [exe_path],
                capture_output=True,
                text=True,
                encoding="utf-8"
            )
            if proc.returncode == 0 and proc.stdout.strip():
                return proc.stdout.strip()
        except Exception:
            pass

    return None

def main():
    raw = get_credit_json()
    if not raw:
        print("[错误] 无法获取额度信息。请确认 Docker 容器正在运行，或目录下存在 credit.exe。")
        sys.exit(1)

    try:
        data = json.loads(raw)
    except Exception as e:
        print(f"[解析错误] 非有效 JSON: {e}\n{raw}")
        sys.exit(1)

    print("=" * 80)
    print("                    WorkBuddy 账号池实时额度查询")
    print("=" * 80)
    print(f"{'昵称':<12} {'UID':<10} {'剩余':<8} {'24h临期':<9} {'72h临期':<9} {'已消耗':<8} {'套餐':<8} {'状态'}")
    print("-" * 80)
    for a in data.get("accounts", []):
        uid = a.get("uid", "")
        uid_short = (uid[:8] + "..") if len(uid) > 8 else uid
        nick = (a.get("nickname") or "未知")[:10]
        remain = a.get("remain", 0)
        u24 = a.get("urgent_24h_remain", 0)
        u72 = a.get("urgent_72h_remain", 0)
        used = a.get("used", 0)
        size = a.get("size", 0)
        st = "正常" if a.get("ok") else ("异常: " + str(a.get("error", "")))
        print(f"{nick:<12} {uid_short:<10} {remain:<8} {u24:<9} {u72:<9} {used:<8} {size:<8} {st}")

    print("-" * 80)
    tot = data.get("total", {})
    rem = tot.get("remain", 0)
    size = tot.get("size", 0)
    pct = (rem / size * 100) if size > 0 else 0
    ok = tot.get("ok", 0)
    total_acc = tot.get("accounts", 0)
    print(f"总剩余: {rem} / {size} ({pct:.1f}%)   |   正常账号: {ok}/{total_acc}")
    print("=" * 80)

if __name__ == "__main__":
    main()
