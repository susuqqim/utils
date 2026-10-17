#!/usr/bin/env python3
"""两阶段账号登录助手（在容器内运行，跨平台一致）。

用法（由 wb2api.sh / wb2api.ps1 调用，不直接暴露给用户）:
    agent_login.py url  --realm=cn|global    # 第一步：打印授权 URL，state 落 auths/
    agent_login.py poll --realm=cn|global    # 第二步：登录完成后取 token，写账号文件

为什么放在容器里跑：这样一套代码同时适配 Windows / Linux / macOS，
不依赖宿主机是否有 Go 工具链，也不需要为每个平台单独准备 login 二进制
（上游的 ./login 是 Linux ELF，Windows 的 Git Bash 无法执行，会报 Exec format error）。

auth 文件格式与 internal/auth 的 Parse() 一致（嵌套形）:
    {"account":{"uid","enterpriseId","nickname"},
     "auth":{"accessToken","refreshToken","expiresAt","domain","realm"}}
"""
import json
import os
import shutil
import subprocess
import sys
import time

AUTH_DIR = os.environ.get("WB2A_AUTH_DIR", "/app/auths")
LOGIN_BIN = os.environ.get("WB2A_LOGIN_BIN", "/app/login")
STATE = os.path.join(AUTH_DIR, ".login-state.json")

REALMS = ("cn", "global")


def _fail(msg):
    print(msg, file=sys.stderr)
    return 1


def run_login(realm, sub):
    """执行 login 二进制的一个子命令；state 固定落在挂载的 auths/ 里，两阶段共享。"""
    env = dict(os.environ, WB2A_LOGIN_STATE=STATE)
    return subprocess.run(
        [LOGIN_BIN, "--realm=" + realm, sub],
        capture_output=True, text=True, env=env, timeout=180,
    )


def cmd_url(realm):
    p = run_login(realm, "url")
    if p.returncode != 0:
        return _fail("login url 失败: " + (p.stderr.strip() or p.stdout.strip()))
    url = p.stdout.strip().splitlines()[-1] if p.stdout.strip() else ""
    if not url.startswith("http"):
        return _fail("login url 未返回授权链接: " + p.stdout.strip())
    print(url)
    return 0


def cmd_poll(realm):
    try:
        if os.path.isfile(STATE):
            with open(STATE, "r", encoding="utf-8") as f:
                st = json.load(f)
                file_realm = (st.get("realm") or "").strip()
                if file_realm in REALMS:
                    realm = file_realm
    except Exception:
        pass

    p = run_login(realm, "poll")
    if p.returncode != 0:
        return _fail("login poll 失败（浏览器登录可能还没完成）: "
                     + (p.stderr.strip() or p.stdout.strip()))
    raw = p.stdout.strip()
    try:
        d = json.loads(raw.splitlines()[-1])
    except Exception as e:
        return _fail("poll 输出解析失败: %s\n原始输出: %s" % (e, raw[:300]))

    uid = (d.get("uid") or "").strip()
    if not uid and d.get("access_token"):
        import base64
        try:
            parts = d["access_token"].split(".")
            if len(parts) >= 2:
                pad = "=" * ((4 - len(parts[1]) % 4) % 4)
                payload = json.loads(base64.urlsafe_b64decode(parts[1] + pad))
                uid = str(payload.get("sub") or payload.get("uid") or "").strip()
                if not d.get("nickname"):
                    d["nickname"] = str(payload.get("name") or payload.get("preferred_username") or payload.get("email") or "").strip()
        except Exception:
            pass

    if not uid:
        return _fail("未取到 uid，请确认已在浏览器完成登录后重试 poll")
    if not d.get("access_token"):
        return _fail("未取到 access_token，登录可能未完成")

    expires_at = int(time.time()) + int(d.get("expires_in") or 0)
    auth = {
        "account": {
            "uid": uid,
            "enterpriseId": d.get("enterprise_id", ""),
            "nickname": d.get("nickname", ""),
        },
        "auth": {
            "accessToken": d.get("access_token", ""),
            "refreshToken": d.get("refresh_token", ""),
            "expiresAt": expires_at,
            "domain": d.get("domain", ""),
            "realm": d.get("realm") or realm,
        },
    }

    os.makedirs(AUTH_DIR, exist_ok=True)
    path = os.path.join(AUTH_DIR, "workbuddy-%s.json" % uid)
    existed = os.path.exists(path)
    # 原子写（2026-10-03 修，第九轮审计 A4）：先写同目录临时文件（创建即 0600，
    # 不存在"写与 chmod 之间世界可读"的窗口），fsync 后 os.replace 原子替换。
    # 旧实现是 open(path,"w") 直接**覆盖写** —— 进程被杀/磁盘满会留下截断的凭证文件，
    # 直接毁掉该账号原有可用凭证（与 Go 侧 auth.SaveAtomic 的做法对齐）。
    tmp = path + ".new"
    with open(tmp, "w", encoding="utf-8") as f:
        json.dump(auth, f, indent=1)
        f.flush()
        os.fsync(f.fileno())
    os.chmod(tmp, 0o600)
    if existed:
        try:
            shutil.copy2(path, path + ".bak")
        except OSError:
            pass
    os.replace(tmp, path)

    print(json.dumps({
        "ok": True,
        "action": "覆盖更新" if existed else "新增",
        "file": os.path.basename(path),
        "uid": uid,
        "nickname": auth["account"]["nickname"],
        "realm": auth["auth"]["realm"],
        "expiresAt": expires_at,
    }, ensure_ascii=False))

    if auth["auth"]["realm"] == "global":
        try:
            scripts_dir = os.path.dirname(os.path.abspath(__file__))
            if scripts_dir not in sys.path:
                sys.path.insert(0, scripts_dir)
            import global_region as gr
            okr, needs, msg = gr.activate_region(auth["auth"]["accessToken"], uid)
            if needs:
                _, ios2, _, _ = gr.detect_region(auth["auth"]["accessToken"])
                _, countries, _ = gr.fetch_countries(intl_only=True)
                pick = next((c for c in countries if c.get("IOS2") == ios2), None)
                if not pick and countries:
                    pick = next((c for c in countries if c.get("IOS2") == "SG"), countries[0])
                if pick:
                    gr.complete_flow(auth["auth"]["accessToken"], uid, pick=pick, call_register=True)
            gr.trial(auth["auth"]["accessToken"])
        except Exception as e:
            print("[提示] 国际版区域完善/Trial 自动处理异常: %s" % e, file=sys.stderr)

    return 0


def main(argv):
    args = [a for a in argv if not a.startswith("--realm")]
    realm = "cn"
    for a in argv:
        if a.startswith("--realm="):
            realm = a.split("=", 1)[1].strip().lower()
    if realm not in REALMS:
        return _fail("非法 realm %r（可选 cn|global）" % realm)
    if not args:
        return _fail("用法: agent_login.py <url|poll> [--realm=cn|global]")

    sub = args[0]
    if sub == "url":
        return cmd_url(realm)
    if sub == "poll":
        return cmd_poll(realm)
    return _fail("未知子命令 %r（可选 url|poll）" % sub)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
