#!/usr/bin/env python3
"""一次性积分任务脚本的共享函数库.

与 probe_active.py 同风格：从 auths/ 读账号凭证，封装 growth 域 / report 域
请求，供 task_*.py 复用。全部默认 dry-run（写动作由调用脚本显式 --yes 放行）。

端点权威来源（Go 代码实测 + 本次实测确认）：
  - chat 域（copilot.tencent.com）：growth / tasks / buddy / streak / chat/completions
  - billing 域（www.codebuddy.cn）：/v2/report
  - accept : POST /v2/activity/growth/tasks/accept  {"task_codes":[code]}
  - claim  : POST /v2/activity/growth/tasks/reward/claim {"task_code":code}
"""
import json, os, time, glob, urllib.request, urllib.error


def _resolve_auths_dir() -> str:
    """解析 auths 凭证目录：WB2A_AUTHS > 本地 auths/ (有凭证) > release offline auths/ > 仓库根 auths/ > /root/workbuddy2api/auths 兜底。

    env 显式覆盖最优先；本地仓库 auths/ 若存在有效 workbuddy-*.json 则使用；
    若本地 auths/ 仅有 .gitkeep，自动回落同级 release/workbuddy2api-offline/auths/；
    兜底保持 Linux 服务器与容器挂载环境行为不变。
    """
    env = os.environ.get("WB2A_AUTHS")
    if env:
        return env
    repo_root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    local = os.path.join(repo_root, "auths")
    if glob.glob(os.path.join(local, "workbuddy-*.json")):
        return local
    offline = os.path.join(os.path.dirname(repo_root), "release", "workbuddy2api-offline", "auths")
    if os.path.isdir(offline) and glob.glob(os.path.join(offline, "workbuddy-*.json")):
        return offline
    if os.path.isdir(local):
        return local
    return "/root/workbuddy2api/auths"


AUTHS = _resolve_auths_dir()
CHAT_BASE = "https://copilot.tencent.com"   # growth / tasks / buddy / streak / chat
BILL_BASE = "https://www.codebuddy.cn"      # report / billing

# growth 域常量（travel.go / report.go 与本次实测对齐）
PATH_LIST_TASKS     = "/v2/activity/growth/tasks"
PATH_ACCEPT_TASKS   = "/v2/activity/growth/tasks/accept"
PATH_CLAIM_REWARD   = "/v2/activity/growth/tasks/reward/claim"
PATH_BUDDY_FIRST    = "/activity/growth/buddy/first"
PATH_BUDDY_AGREEMENT = "/activity/growth/buddy/agreement"
PATH_STREAK         = "/activity/growth/streak"
PATH_REPORT         = "/v2/report"
PATH_CHAT           = "/v2/chat/completions"

CLIENT_UA = "CLI/2.63.2 CodeBuddy/2.63.2"


def load_auth(uid_or_file: str) -> dict:
    """从 auths/ 加载账号凭证，uid_or_file 为 uid 前缀或 auths 文件名。

    返回 {token, uid, domain, nick, file, realm} 六元组。
    realm 读取兼容嵌套形（`auth.realm`，login.sh --realm=global 落盘形态）与
    扁平形（顶层 `realm`）；两种都缺省 → "cn"（老 CN 凭证零回归）。
    """
    if os.path.sep in uid_or_file or uid_or_file.endswith(".json"):
        p = uid_or_file
        if not os.path.isabs(p):
            p = os.path.join(AUTHS, p)
    else:
        pre = uid_or_file
        hits = glob.glob(os.path.join(AUTHS, f"workbuddy-{pre}*.json"))
        if not hits:
            raise SystemExit(f"no auth for {pre}")
        p = hits[0]
    d = json.load(open(p))
    a, acc = d["auth"], d["account"]
    realm = a.get("realm") or d.get("realm") or ""
    return {"token": a["accessToken"], "domain": a.get("domain") or "",
            "uid": acc["uid"], "nick": acc.get("nickname", ""),
            "file": os.path.basename(p), "realm": realm}


def auth_is_global(auth: dict) -> bool:
    """判定账号是否属于 global realm：realm==global 或 domain 后缀 .workbuddy.ai / .codebuddy.ai。

    与 Go auth.Realm() 的判定口径一致（显式 realm 优先于 domain 回落）。
    供 CN-only 任务脚本跳过 global 账号、明确提示，避免把 global token 打向
    copilot.tencent.com/codebuddy.cn（全球版无任务中心，打 CN 端点属错误行为）。
    """
    if (auth.get("realm") or "").strip().lower() == "global":
        return True
    d = (auth.get("domain") or "").strip().lower()
    return (
        d == "workbuddy.ai"
        or d.endswith(".workbuddy.ai")
        or d == "codebuddy.ai"
        or d.endswith(".codebuddy.ai")
    )


def chat_base(auth: dict) -> str:
    return auth.get("chat_base") or CHAT_BASE


def billing_base(auth: dict) -> str:
    return auth.get("billing_base") or BILL_BASE


def _headers(auth: dict) -> dict:
    hdr = {"Authorization": "Bearer " + auth["token"],
           "Accept": "application/json",
           "Content-Type": "application/json",
           "User-Agent": CLIENT_UA,
           "Origin": "https://www.codebuddy.cn",
           "Referer": "https://www.codebuddy.cn/"}
    if auth.get("uid"):
        hdr["X-User-Id"] = auth["uid"]
    if auth.get("domain"):
        hdr["X-Domain"] = auth["domain"]
    return hdr


def _request(auth, method, base, path, body=None, headers=None, timeout=30):
    url = path if path.startswith("http") else base + path
    hdr = _headers(auth)
    if headers:
        hdr.update(headers)
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, headers=hdr, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, json.loads(r.read().decode("utf-8", "replace"))
    except urllib.error.HTTPError as e:
        t = e.read().decode("utf-8", "replace")
        try:
            return e.code, json.loads(t)
        except Exception:
            return e.code, {"raw": t[:300]}
    except Exception as e:
        return -1, {"err": repr(e)}


def do_get(auth, base, path, headers=None) -> tuple:
    """GET 读请求，返回 (status, dict)。"""
    return _request(auth, "GET", base, path, None, headers)


def do_post(auth, base, path, body, headers=None) -> tuple:
    """POST 写请求，返回 (status, dict)。body 为 dict。"""
    return _request(auth, "POST", base, path, body, headers)


MP_PLATFORM = "miniprogram"
MP_TASK_CODES = {
    "Sequential_Tasks_1", "Sequential_Tasks_2", "Sequential_Tasks_3",
    "Sequential_Tasks_4", "Sequential_Tasks_5", "Sequential_Tasks_6",
    "Sequential_Tasks_7", "school_season", "wb_wechat_oa_subscribe_task",
}


def is_mp_task(code: str) -> bool:
    """判定是否为微信小程序专属任务口径。"""
    if not code:
        return False
    return code in MP_TASK_CODES or code.startswith("Sequential_Tasks_")


def list_tasks(auth, mp: bool = False) -> list:
    """GET /v2/activity/growth/tasks 全量任务列表（元素为原始 dict）。
    
    mp=True 时叠加 X-Client-Platform: miniprogram，用于拉取 Sequential 等小程序任务。
    """
    headers = {"X-Client-Platform": MP_PLATFORM} if mp else None
    st, d = do_get(auth, chat_base(auth), PATH_LIST_TASKS, headers=headers)
    if st != 200:
        raise RuntimeError(f"list_tasks http={st}")
    tasks = (d.get("data", {}) or {}).get("tasks") or []
    return tasks


def list_all_tasks(auth) -> list:
    """合并默认口径与小程序口径任务列表，按 task_code 去重（保留 MP 特有任务）。"""
    std_tasks = list_tasks(auth, mp=False)
    try:
        mp_tasks = list_tasks(auth, mp=True)
    except Exception:
        mp_tasks = []
    seen = set()
    merged = []
    for t in std_tasks:
        c = t.get("task_code")
        if c and c not in seen:
            seen.add(c)
            merged.append(t)
    for t in mp_tasks:
        c = t.get("task_code")
        if c and c not in seen:
            seen.add(c)
            merged.append(t)
    return merged


def task_status(auth, task_code) -> dict | None:
    """查单个任务当前状态；找不到返回 None。"""
    use_mp = is_mp_task(task_code)
    try:
        for t in list_tasks(auth, mp=use_mp):
            if t.get("task_code") == task_code:
                return t
    except Exception:
        pass
    # 兜底在另一口径查找
    try:
        for t in list_tasks(auth, mp=not use_mp):
            if t.get("task_code") == task_code:
                return t
    except Exception:
        pass
    return None


def accept_tasks(auth, task_codes, mp: bool = False) -> tuple:
    """POST accept 任务（not_accepted → accepted）。返回 (status, resp)。"""
    need_mp = mp or any(is_mp_task(c) for c in task_codes)
    headers = {"X-Client-Platform": MP_PLATFORM} if need_mp else None
    return do_post(auth, chat_base(auth), PATH_ACCEPT_TASKS,
                   {"task_codes": task_codes}, headers=headers)


def claim_reward(auth, task_code, mp: bool = False) -> tuple:
    """POST claim 领取奖励（M15 真实端点）。mp=True 或小程序专属任务自动带 X-Client-Platform 头。"""
    need_mp = mp or is_mp_task(task_code)
    headers = {"X-Client-Platform": MP_PLATFORM} if need_mp else None
    return do_post(auth, chat_base(auth), f"/activity/growth/tasks/{task_code}/claim",
                   None, headers=headers)


def get_streak(auth) -> int:
    """GET /activity/growth/streak 连登天数（只读 oracle）。失败返回 -1 记日志。"""
    st, d = do_get(auth, chat_base(auth), PATH_STREAK)
    if st != 200:
        return -1
    return (d.get("data", {}).get("streak", {}) or {}).get("days", 0)


def chat_event(auth, conversation_id=None, model_id="deepseek-v4-flash",
               model_name="DeepSeek V4 Flash", mode="craft"):
    """客户端 chat_request_send 事件完整形状（照抄 report.go / probe_active.py）。

    必须带 userId（=账号 uid），缺失则服务端 200 但静默丢弃。
    model_id/name 可换（如 GLM-5.2），供 model_chat 对齐实际模型。
    """
    now = int(time.time() * 1000)
    cid = conversation_id or f"task-{now}"
    return {"eventCode": "chat_request_send", "timestamp": now, "reportDelay": 0,
            "mode": mode, "conversationId": cid, "requestId": cid,
            "inputLength": 12, "requestModelId": model_id,
            "requestModelName": model_name, "isPlan": False,
            "isAutoExecuteTerminal": False, "isAutoModify": False,
            "codebaseEnable": False, "maxToken": 0, "maxSteps": 0, "temperature": 0,
            "maxRetries": 0, "mentionContexts": [], "knowledgeId": [],
            "knowledgeName": [], "codebaseId": "", "mentionContextCount": 0,
            "command": "", "expertId": "", "recommendId": "", "skillId": "",
            "skillCount": 0, "totalCount": 0, "fileUri": "", "presentAt": now,
            "traceId": "", "rootRequestId": cid, "parentConversationId": cid,
            "agentName": "default", "agentType": "conversation", "userId": auth["uid"]}


def report_activity(auth, count=1, gap=1.05, model_id="deepseek-v4-flash",
                    model_name="DeepSeek V4 Flash", mode="craft") -> list:
    """向 {billing}/v2/report 上报 count 条 chat_request_send。

    每次间隔 >= gap 秒（默认 1.05，匹配 probe_active.py 的同接口限速口径）。
    返回 [(status, code), ...] 汇总。
    """
    out = []
    for i in range(count):
        ev = chat_event(auth, model_id=model_id, model_name=model_name, mode=mode)
        st, r = do_post(auth, billing_base(auth), PATH_REPORT, [ev])
        out.append((st, r.get("code") if isinstance(r, dict) else None))
        if i < count - 1:
            time.sleep(gap)
    return out


def chat_completion(auth, model_id="glm-5.2", prompt="hi", max_tokens=32,
                    timeout=60, extra_var=None) -> tuple:
    """POST {chat}/v2/chat/completions 真实对话一次（stream:true）。

    服务端强制流式（payload.go 同款口径），这里逐行读 SSE 直到 done。
    返回 (status, first_content)。用于 Model_chat_GLM5.2 的“真实对话一次”。

    extra_var（可选）：顶层 extra_vars 覆盖/新增字段（如 growthEvent），
    模拟桌面端 requestOptions.providerData 的透传形状。
    """
    body = {"model": model_id, "messages": [{"role": "user", "content": prompt}],
            "stream": True, "max_tokens": max_tokens}
    if extra_var:
        body["extra_vars"] = {**(body.get("extra_vars") or {}), **extra_var}
    hdr = {"Accept": "text/event-stream"}  # SSE
    url = chat_base(auth) + PATH_CHAT
    req_headers = _headers(auth)
    req_headers.update(hdr)
    req = urllib.request.Request(url, data=json.dumps(body).encode(),
                                 headers=req_headers, method="POST")
    first = ""
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            status = r.status
            for raw in r:
                line = raw.decode("utf-8", "replace")
                if line.startswith("data: "):
                    payload = line[6:].strip()
                    if payload in ("[DONE]", ""):
                        continue
                    try:
                        obj = json.loads(payload)
                        delta = (obj.get("choices") or [{}])[0].get("delta") or {}
                        content = delta.get("content") or ""
                        if content and not first:
                            first = content
                    except Exception:
                        pass
            return status, first
    except urllib.error.HTTPError as e:
        t = e.read().decode("utf-8", "replace")
        return e.code, t[:200]
    except Exception as e:
        return -1, repr(e)[:200]