# WorkBuddy2API 原生安装（无 Docker / 无 npm）

> 👶 **第一次使用？请先用浏览器打开同目录下的 `使用指南.html`** —— 图文步骤，照着做即可。

把 WorkBuddy / CodeBuddy 账号额度转成 OpenAI 兼容 API 的本地网关。
本原生发行物只包含一个静态单文件二进制，不需要 Docker Desktop，也不需要 Node/npm。
账号授权（OAuth）已内建在网关里，全程在浏览器完成。面向 Agent 的接入说明见 AGENTS.md。

## 一条命令安装

Linux / macOS：

    WB2API_BASE_URL=https://<你的托管目录>/native sh -c 'curl -fsSL "$WB2API_BASE_URL/install.sh" | sh'

Windows PowerShell：

    $env:WB2API_BASE_URL='https://<你的托管目录>/native'; irm "$env:WB2API_BASE_URL/install.ps1" | iex

把 WB2API_BASE_URL 换成实际托管 install.sh / install.ps1 / wb2api-<os>-<arch> / SHA256SUMS 的目录即可。

## 离线安装

下载并解压发行目录后，直接执行：

    Linux/macOS:  ./install.sh
    Windows:      powershell -ExecutionPolicy Bypass -File install.ps1

（离线模式下安装器优先使用同目录的 wb2api-<os>-<arch> 二进制，不联网。）

## Windows 图形化安装（MSI，推荐桌面用户）

发行目录里另有 `workbuddy2api-<版本>-x64.msi`（**每用户安装，免管理员**）：

    msiexec /i workbuddy2api-1.11.6-x64.msi

- 安装到 `%LOCALAPPDATA%\workbuddy2api`（与 `install.ps1` **同一路径**，老用户可无缝覆盖升级）。
- 自动创建**桌面**与**开始菜单**快捷方式（启动网关 / 停止网关 / 打开管理面板；面板为浏览器直达）。
- 默认注册开机自启（走二进制自带的 `autostart`）；`msiexec /i x.msi AUTOSTART=0` 可跳过。
- 卸载走「设置 → 应用 → 已安装的应用」，**默认保留账号数据**；
  仅当 `msiexec /x x.msi REMOVEDATA=1` 时才连 `%USERPROFILE%\.workbuddy2api` 一起删除。
- **升级前建议先 `wb2api stop`**（安装器现在会 best-effort 停掉本用户实例 —— 2026-10-03 起与 MSI 的
  `CaStopAppBeforeInstall` 对齐；但在"同版本重装"这类
  维护序列下不保证生效（会报「另一个程序正在使用此文件」）。`install.ps1` 同理。
- 与 `install.ps1` 二者选其一即可（同路径，混用会互相覆盖程序文件；账号数据不受影响）。

## 系统托盘（Windows）

Windows 原生模式**默认显示托盘图标**（与网关同进程，因此 `wb2api stop` 会连它一起结束，不留孤儿）：

- **左键单击** → 打开管理面板
- **右键菜单** → 状态（N/M 账号可用）/ 打开管理面板 / 重启服务 / 开机自启（勾选）/ 退出
- 启用托盘时运行期的控制台窗口会被隐藏（进程仍是 console 子系统，日志照常进面板与 `data/logs`）；
  需要看前台日志请用 `wb2api run`（该模式不启用托盘、保留控制台）
- 关闭托盘：`config.json` 里设 `"ui": { "tray": false }`，或启动参数加 `-no-tray`

## 安装后

1. 设置你自己的 API Key：

       wb2api setkey <你的密钥>

2. 启动：

       wb2api start

3. 浏览器打开面板，用 WorkBuddy / CodeBuddy 账号扫码或点授权链接登录：

       http://127.0.0.1:17963/panel/

4. 客户端（Cursor / Cline / Cherry Studio 等）配置：

       Base URL:  http://127.0.0.1:17963/v1
       API Key:   上一步设置的密钥
       Model:     glm-5.3 / deepseek-v4.1-flash / kimi-k2.7 等（面板可见完整列表）

## 命令

    wb2api start          后台启动（首次会自动等待健康检查）
    wb2api stop           停止
    wb2api status         查看健康状态
    wb2api panel          打印面板地址
    wb2api logs           跟踪日志（前台）
    wb2api setkey K       写入 API Key
    wb2api run            前台运行
    wb2api autostart on   开启开机自启
    wb2api autostart off  关闭开机自启
    wb2api autostart status 查看自启状态

## 开机自启

一条命令开关，按平台自动选择机制：

    Linux  : systemd --user（workbuddy2api.service；无 systemd 时回落 crontab @reboot）
    macOS  : launchd LaunchAgent（~/Library/LaunchAgents/com.workbuddy2api.plist）
    Windows: 内置实现，直接写 HKCU\...\Run（免管理员、免额外脚本、不经 PowerShell）

用法：

    wb2api autostart on
    wb2api autostart status
    wb2api autostart off

    Windows 可选：wb2api autostart on --startup-folder
      仅在写注册表被组策略/杀软拦截时，回退到启动文件夹 .cmd（该形态更易被杀软的
      "启动项防护"关注，故不默认使用）。

也可直接调用脚本： Linux/macOS 为 scripts 目录里的 autostart.sh（纯系统机制）；
Windows 的自启已内置在二进制中，不再需要 .ps1 脚本。

## 目录

    Unix:      程序 <prefix>/lib/workbuddy2api/wb2api，命令 <prefix>/bin/wb2api
               数据 ~/.workbuddy2api/（config.json / auths / data / logs）
    Windows:   程序 %LOCALAPPDATA%\workbuddy2api\wb2api-core.exe
               命令 %LOCALAPPDATA%\workbuddy2api\wb2api.cmd
               数据 %USERPROFILE%\.workbuddy2api\

可用 WB2API_HOME 覆盖数据目录，WB2API_PREFIX 覆盖 Unix 安装前缀，WB2API_PORT 覆盖端口。

## 说明

- 只监听 127.0.0.1，不暴露局域网。
- api_key 在 config.json 里，改动后需重启（wb2api stop && wb2api start）。
- 二进制未做代码签名：macOS 安装器会自动清除 quarantine 标记；Windows SmartScreen
  可能提示"未知发布者"，选"更多信息 -> 仍要运行"即可，或对二进制自行签名。
- Windows 二进制**刻意不剥符号表**（构建用 `-ldflags="-w"` 而非 `-s -w`）：实测剥掉符号后
  会被火绒等杀软启发式误判为 `HEUR:VirTool/Obfuscator.a` 并直接隔离，故以略增体积换可用性。
  若仍被拦截，把 `%LOCALAPPDATA%\workbuddy2api\` 加入杀软信任区即可。
- 支持平台：linux-amd64 / linux-arm64 / darwin-amd64 / darwin-arm64 / windows-amd64。
