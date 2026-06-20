# bifrost-scheduler

`bifrost-scheduler` 是给 GGAPI Bifrost 用的自动调度器。它和旧的只读巡检项目分开维护，当前模板只治理两个文本 Virtual Key：

- `gpt_low`
- `gpt_stable`

生图池暂不纳入示例配置，避免调度器误动 image 权重。

调度器不按 `low` 或 `stable` 写死不同策略。配置里只需要给 provider 写健康时的 `cost_weight`；质量由 Bifrost 近期日志自动判断。

第一版默认只输出 dry-run 计划，不写生产。只有配置文件 `mode=guarded_write` 且命令显式带 `--apply` 时，才会执行受限写操作。

调度器直接对接 Bifrost 管理 API，不读取或写入 Bifrost 数据库。

Bifrost 管理 API 的固定路径已内置，不需要在配置文件里重复写：

- `/api/session/login`
- `/api/governance/virtual-keys`
- `/api/logs`
- `/api/providers/{provider}/keys/{key_id}`

## 能做什么

- 通过 `GET /api/governance/virtual-keys` 读取 VK provider 权重、key 绑定状态和 provider 是否存在。
- 通过 `GET /api/logs` 按 Virtual Key + provider 统计成功率、错误率、P95 延迟、关键错误和超时。
- 按 `config.example.json` 的规则输出调度计划：
  - `set_weight`：调低或恢复权重。
  - `set_weight_zero`：把某个 provider 在某个 VK 的权重置 0。
  - `disable_provider`：权重置 0，并禁用该 provider 的 key。
  - `review_missing_provider`：配置里应存在但 Bifrost VK 内没有，需要人工建 provider config。
- 用样本量、错误数和连续坏窗口做保护，避免单次失败或低样本误判；P95 延迟默认只展示，不自动调权。
- 同一轮计划会按“计划后的活跃 provider 数”保护 `min_active_providers`，避免多个坏渠道同时被清零导致池子无可用渠道。

## 安全边界

默认 `read_only` 不写 Bifrost。即使传 `--apply`，如果配置不是 `guarded_write`，也只会在输出里说明没有执行。

调度器只支持自动登录模式：

- 使用 `BIFROST_API_USERNAME` / `BIFROST_API_PASSWORD` 调 `/api/session/login`。
- 登录成功后调度器自动使用返回的 token 访问管理 API。
- 不使用 PostgreSQL DSN。
- 不使用普通 `sk-bf-*` Virtual Key。
- 不要求手工维护 dashboard/session token。

永远不自动做这些事：

- 删除 provider。
- 删除 Virtual Key。
- 修改 API key 值。
- 修改模型映射。
- 修改 Sub2API 分组、价格或用户余额。

## 本地运行

```bash
go build ./cmd/bifrost-scheduler

BIFROST_API_URL='https://your-bifrost.example.com' \
BIFROST_API_USERNAME='admin' \
BIFROST_API_PASSWORD='***admin-password***' \
./bifrost-scheduler plan --config config.example.json --format markdown
```

线上环境：

```bash
BIFROST_API_URL='https://your-bifrost.example.com' \
BIFROST_API_USERNAME='admin' \
BIFROST_API_PASSWORD='***admin-password***' \
./bifrost-scheduler plan --config config.json --format markdown
```

JSON 输出：

```bash
./bifrost-scheduler plan --format json
```

守护模式：

```bash
./bifrost-scheduler daemon --interval 5m
```

Docker Compose 生产示例：

```yaml
services:
  bifrost-scheduler:
    image: ghcr.io/akuma-real/bifrost-scheduler:latest
    restart: unless-stopped
    env_file:
      - .env
    volumes:
      - ./config.json:/app/config.json:ro
      - ./logs:/app/logs
    command:
      - daemon
      - --config
      - /app/config.json
      - --interval
      - 5m
    read_only: true
    tmpfs:
      - /tmp
    security_opt:
      - no-new-privileges:true
    logging:
      driver: json-file
      options:
        max-size: "2m"
        max-file: "3"
```

受限写入模式必须同时满足两个条件：

```bash
# 1. 配置文件 mode 改为 guarded_write
# 2. 命令显式加 --apply
./bifrost-scheduler plan --apply
```

生产日志建议写到轮转文件，避免容器 stdout 过大：

```env
BIFROST_SCHEDULER_LOG_FILE=/app/logs/scheduler.log
BIFROST_SCHEDULER_LOG_MAX_SIZE=10MB
BIFROST_SCHEDULER_LOG_MAX_BACKUPS=5
BIFROST_SCHEDULER_LOG_STDOUT=false
```

这样完整调度报告写入 `./logs/scheduler.log`，单文件到 10MB 自动轮转，最多保留 5 个备份；`docker logs` 只保留每轮短摘要和错误，Docker 自身日志由 compose 的 `max-size/max-file` 限制。

也可以用命令行覆盖 API 地址：

```bash
./bifrost-scheduler plan \
  --api-url https://your-bifrost.example.com \
  --api-username "$BIFROST_API_USERNAME" \
  --api-password "$BIFROST_API_PASSWORD"
```

## 配置思路

完整字段说明见 [docs/configuration.md](docs/configuration.md)。

当前建议先只接两个文本 VK：

```text
Sub2API gpt_low    -> Bifrost gpt_low
Sub2API gpt_stable -> Bifrost gpt_stable
```

不要再把 stable backup 做成独立 VK。备用 provider 放在 stable VK 里面，用低权重和自动调度治理。

生图单独一个 VK，不和文本池混用；当前示例不配置 image，后续确认策略后再单独加。
