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
- 可选 Telegram bot 通知：本轮有变更时发送摘要；daemon 模式会按变更指纹去重，避免同一批变更每 5 分钟重复刷屏。
- 可选 Telegram 交互控制台：在 daemon 模式下查看状态、查看最近计划、手动 dry-run、静音/恢复通知。

## 代码结构

项目按轻量 DDD 分层：

- `internal/domain/scheduler`：领域模型、配置默认值、调度决策规则。这里不依赖 HTTP、CLI、文件系统或 Bifrost API。
- `internal/app/scheduler`：应用用例，负责编排读取状态、统计指标、生成计划和执行受限写入。
- `internal/bifrost`：Bifrost 管理 API 适配器，负责登录、读取 VK/logs、更新权重和禁用 key。
- `internal/report`：输出层，把调度计划渲染成 JSON 或中文 Markdown。
- `cmd/bifrost-scheduler`：CLI、daemon、日志轮转和环境变量入口。

新增调度规则时优先放在 `internal/domain/scheduler`，新增 API 字段或接口兼容逻辑放在 `internal/bifrost`，不要把 HTTP 细节塞进领域层。

如果你想借这个项目学习 Go，先读 [docs/learning-go.md](docs/learning-go.md)。那份文档按阅读顺序解释了入口、包、结构体、接口、错误处理和测试。

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

Telegram 通知是可选的。只要配置 bot token 和 chat id，调度器发现本轮有 `decisions` 时会发通知：

```env
BIFROST_SCHEDULER_TG_BOT_TOKEN=123456:bot-token-from-botfather
BIFROST_SCHEDULER_TG_CHAT_ID=-1001234567890
# 如果发到 Telegram 群组话题，再填这个：
# BIFROST_SCHEDULER_TG_THREAD_ID=123
# 开启 Telegram 交互控制台：
BIFROST_SCHEDULER_TG_INTERACTIVE=true
```

通知失败不会阻断调度，只会写一条错误日志。没有建议动作时不发通知。

Telegram 交互控制台只在 `daemon` 模式下生效，使用 Telegram `getUpdates` 长轮询，不需要 webhook，也不需要开放公网端口。为了安全，只有 `BIFROST_SCHEDULER_TG_CHAT_ID` 配置的 chat 可以操作。

交互回复会使用 Telegram HTML 富文本显示粗体和等宽代码；执行 `/run` 等需要等待 Bifrost API 的命令时，bot 会先显示“正在输入...”，避免看起来像没反应。

可用命令：

```text
/status     查看 daemon 状态、运行间隔、最近错误
/last       查看最近一次调度计划摘要
/run        立即执行一次 dry-run 预览，不写线上
/mute 1h    静音变更通知，调度器仍继续运行
/unmute     恢复变更通知
/help       查看帮助
```

注意：Telegram `/run` 永远是 dry-run。即使生产容器的 daemon 带了 `--apply`，手动 `/run` 也不会写 Bifrost。真正写线上仍然只由 `config.json` 的 `mode=guarded_write` 和 daemon 命令里的 `--apply` 控制。

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
