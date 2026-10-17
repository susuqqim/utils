#!/bin/bash
set -e

# ==============================================================================
# WorkBuddy2API Docker 启动入口脚本
# 解决 Windows 宿主机挂载目录（NTFS / WSL2 drvfs）在非 root 权限下的权限拒绝问题
# ==============================================================================

# 确保持久化目录存在
mkdir -p /app/data /app/auths /app/scripts

# Windows Docker Desktop (WSL2 / Hyper-V) 挂载权限自愈：
# 宿主机 bind-mount 映射到容器后通常默认属主为 root:root，
# 尝试修复所有者并赋予宽容读写权限，避免 app 用户写 state.json 失败
chown -R app:app /app/data /app/auths 2>/dev/null || true
chmod -R 777 /app/data /app/auths 2>/dev/null || true
if [ -f /app/config.json ]; then
    chown app:app /app/config.json 2>/dev/null || true
    chmod 666 /app/config.json 2>/dev/null || true
fi

# 检查当前是否以 root 运行
if [ "$(id -u)" = '0' ]; then
    # 测试 app 用户是否具备 /app/data 的实际写入权限
    if su-exec app touch /app/data/.perm_check 2>/dev/null; then
        rm -f /app/data/.perm_check
        # 降权到 app (uid 10001) 运行服务（标准安全模式）
        exec su-exec app "$@"
    else
        # 挂载卷在特定 Windows 共享下拒绝非 root 写入时的自动逃生：
        echo "[WARN] 宿主机文件系统限制 app 用户写入，自动以 root 用户启动服务以确保正常运行"
        exec "$@"
    fi
else
    # 非 root 用户直接执行
    exec "$@"
fi
