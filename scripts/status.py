# -*- coding: utf-8 -*-
"""
WorkBuddy2API 状态与账号池概览工具
支持直接运行或通过 docker exec 在容器内运行
"""
import json
import os
import sys
import urllib.request as u

def fetch_status():
    config_candidates = [
        "/app/config.json",
        os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "config.json"),
        "config.json"
    ]
    api_key = ""
    listen = "127.0.0.1:17963"

    for cp in config_candidates:
        if os.path.exists(cp):
            try:
                with open(cp, "r", encoding="utf-8") as f:
                    cfg = json.load(f)
                    api_key = cfg.get("api_key", "")
                    l = cfg.get("listen", ":17963")
                    if l.startswith(":"):
                        listen = "127.0.0.1" + l
                    else:
                        listen = l
                break
            except Exception:
                pass

    url = f"http://{listen}/status"
    req = u.Request(url, headers={"Authorization": f"Bearer {api_key}"})
    try:
        opener = u.build_opener(u.ProxyHandler({}))
        with opener.open(req, timeout=5) as resp:
            return json.loads(resp.read().decode("utf-8"))
    except Exception as e:
        print(f"[错误] 无法连接到服务 ({url}): {e}")
        return None

def format_row(cols, widths):
    res = []
    for c, w in zip(cols, widths):
        # 简易中英文字符串对齐处理
        c_str = str(c)
        pad = w - sum(2 if ord(ch) > 127 else 1 for ch in c_str)
        if pad > 0:
            res.append(c_str + " " * pad)
        else:
            res.append(c_str[:w])
    return " ".join(res)

def main():
    data = fetch_status()
    if not data:
        sys.exit(1)

    total = data.get("total", 0)
    healthy = data.get("healthy", 0)
    cooling = data.get("cooling", 0)
    disabled = data.get("disabled", 0)
    sticky = data.get("sticky_sessions", 0)
    redis_mode = data.get("redis_mode", "noop")
    daily_credits = data.get("daily_credits_used", 0.0)
    total_credits = data.get("total_credits_used", 0.0)
    daily_tokens = data.get("daily_tokens_used", 0)
    total_tokens = data.get("total_tokens_used", 0)
    accounts = data.get("accounts", [])

    print("=" * 96)
    print("                    WorkBuddy2API 账号池实时概览")
    print("=" * 96)
    print(f"账号总数: {total:<3} | 可用健康: {healthy:<3} | 冷却中: {cooling:<3} | 禁用: {disabled:<3}")
    print(f"今日消耗: {daily_credits:.2f} 积分 ({daily_tokens} tokens) | 累计消耗: {total_credits:.2f} 积分 ({total_tokens} tokens)")
    print(f"粘性会话: {sticky:<3} | 存储模式: {redis_mode:<8}")
    print("-" * 96)

    headers = ["昵称", "UID", "在途", "剩余积分", "今日消耗", "累计消耗", "24h临期", "72h临期", "到期(天)", "状态"]
    widths = [12, 10, 6, 10, 10, 10, 9, 9, 9, 10]
    print(format_row(headers, widths))
    print("-" * 96)

    for a in accounts:
        nick = a.get("nickname") or "未知"
        uid = (a.get("uid") or "")[:8]
        inflight = a.get("in_flight", 0)
        credits = a.get("credits", 0)
        daily_used = a.get("daily_credits_used", 0.0)
        total_used = a.get("credits_used", 0.0)
        u24 = a.get("urgent_24h_remain", 0)
        u72 = a.get("urgent_72h_remain", 0)
        days = a.get("days_until_expire")
        days_str = f"{days:.1f}" if days is not None else "-"
        
        st = "正常"
        if a.get("disabled"):
            st = "已禁用"
        elif a.get("paused"):
            # 暂停选号 ≠ 禁用：保号任务（签到/活跃/保活/余额）照常，可随时恢复（见 paused 特性）
            st = "已暂停选号"
        elif a.get("cooling"):
            rem = a.get("cool_remaining_sec", a.get("cool_remaining", 0))
            st = f"冷却({rem}s)"

        cols = [nick, uid, inflight, credits, f"{daily_used:.2f}", f"{total_used:.2f}", u24, u72, days_str, st]
        print(format_row(cols, widths))

    print("=" * 96)

if __name__ == "__main__":
    main()
