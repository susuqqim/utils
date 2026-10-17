# ==============================================================================
# 阶段 1：Go 多二进制编译环境
# ==============================================================================
FROM golang:1.23-alpine AS build

# 国内网络加速：国内 proxy.golang.org 不可达，必须配置 goproxy.cn
ENV GOPROXY=https://goproxy.cn,direct \
    GOSUMDB=sum.golang.google.cn \
    GOFLAGS=-mod=mod

WORKDIR /src

# 利用 Docker 缓存层：go.mod / go.sum 未变化时跳过依赖下载
COPY go.mod go.sum ./
RUN go mod download

# 复制源码并构建所有 Linux 二进制产物（-trimpath -s -w 精简体积）
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/wb2api ./cmd/server \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/signin_bin ./cmd/signin \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/login ./cmd/login \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/credit ./cmd/credit \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/trial_bin ./cmd/trial \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/activity_bin ./cmd/activity

# ==============================================================================
# 阶段 2：轻量生产运行时
# ==============================================================================
FROM alpine:3.20

# 安装运行所需的基础组件：
# - ca-certificates: HTTPS 上游证书通信
# - tzdata: 时区支持（确保调度器与临期时间精确匹配东八区）
# - wget: 存活探针工具
# - python3 + bash: 登录与日常运维/活动脚本支持
# - su-exec: 权限降级工具（用于在 entrypoint 中自愈宿主机挂载权限后平滑降权）
RUN apk add --no-cache wget ca-certificates tzdata python3 bash su-exec \
 && adduser -D -u 10001 app \
 && mkdir -p /app/auths /app/data /app/scripts \
 && chown -R app:app /app

ENV TZ=Asia/Shanghai

WORKDIR /app

# 从构建阶段复制所有编译产物
COPY --from=build /out/wb2api /app/wb2api
COPY --from=build /out/signin_bin /app/signin_bin
COPY --from=build /out/login /app/login
COPY --from=build /out/credit /app/credit
COPY --from=build /out/trial_bin /app/trial_bin
COPY --from=build /out/activity_bin /app/activity_bin

# 复制日常运维脚本（全部 shell 脚本与 python 辅助脚本）
COPY *.sh /app/
COPY credit.py /app/
COPY scripts/ /app/scripts/
COPY docker-entrypoint.sh /app/docker-entrypoint.sh

# 统一处理换行符（CRLF -> LF）并赋予可执行权限，杜绝 Windows 检出换行污染
RUN sed -i 's/\r$//' /app/*.sh /app/docker-entrypoint.sh /app/scripts/*.sh /app/scripts/*.py 2>/dev/null || true \
 && chmod +x /app/*.sh /app/docker-entrypoint.sh /app/scripts/*.sh /app/scripts/*.py 2>/dev/null || true

# 缺省配置文件（容器启动时通常由宿主机 ./config.json 卷挂载覆盖）
COPY config.example.json /app/config.json

# 暴露端口 17963（避开 Windows Hyper-V 预留端口段 7798-7897）
EXPOSE 17963

# 存活健康探针（存活语义）：
# 账号池未就绪时 /healthz 返回 503，busybox wget 退出码为 8。
# 退出码 0 与 8 均视为进程活着，只有连接失败(1)或超时判定为容器异常。
HEALTHCHECK --interval=10s --timeout=3s --start-period=3s --retries=3 \
  CMD wget -qO- http://127.0.0.1:17963/healthz >/dev/null 2>&1; e=$?; [ $e -eq 0 ] || [ $e -eq 8 ]

ENTRYPOINT ["/app/docker-entrypoint.sh"]
CMD ["/app/wb2api", "-config", "/app/config.json"]