# 配置文件说明

配置文件是 JSON。公开模板是 `config.example.json`，真实线上配置放在 `config.json`。`config.json` 和 `.env` 已在 `.gitignore` 里，不提交到 GitHub。

这个调度器的配置原则是：

- 静态配置只表达“这个 provider 健康时我愿意给多少流量”，也就是 `cost_weight`。
- 质量不手填分数，由调度器从 Bifrost `/api/logs` 自动判断。
- 如果开启主动测速，首字速度由调度器自己发流式 `provider/model` 请求测量，不使用 Bifrost 日志里的总耗时冒充。
- 调度器不会因为 pool 叫 `gpt_low` 或 `gpt_stable` 就写死不同策略。
- 新渠道、低样本、单窗口异常不会直接清零或禁用。

Bifrost API 路径已经内置，不需要写进 JSON：

- `POST /api/session/login`
- `GET /api/governance/virtual-keys`
- `GET /api/logs`
- `PUT /api/governance/virtual-keys/{vk_id}`
- `PUT /api/providers/{provider}/keys/{key_id}`

## 最小配置

当前建议只管两个文本 VK。生图 VK 不写进配置，调度器就不会碰 image。

```json
{
  "mode": "read_only",
  "api": {
    "base_url": "https://example-bifrost.internal"
  },
  "probe": {
    "enabled": false,
    "model": "gpt-5.5",
    "samples": 2,
    "timeout": "20s"
  },
  "pools": [
    {
      "id": "text_low",
      "virtual_key": "text_low",
      "providers": [
        {
          "name": "provider_a",
          "cost_weight": 1
        },
        {
          "name": "provider_b",
          "cost_weight": 0.5
        },
        {
          "name": "new_provider_c",
          "role": "candidate",
          "cost_weight": 0.2,
          "min_weight": 0.05
        },
        {
          "name": "disabled_provider_d",
          "role": "quarantine"
        }
      ]
    },
    {
      "id": "text_stable",
      "virtual_key": "text_stable",
      "providers": [
        {
          "name": "provider_e",
          "cost_weight": 0.8
        },
        {
          "name": "provider_f",
          "cost_weight": 0.4
        }
      ]
    }
  ]
}
```

这份配置的含义：

- `mode: read_only`：只预览，不写线上。
- `virtual_key`：Bifrost 里真实的 Virtual Key 名称，必须完全一致。
- `cost_weight`：健康时恢复到的目标权重。数值越高，健康时越优先吃流量。
- `probe.enabled: false`：默认不发额外测速请求。开启后才会按首字速度参与调度。
- `role: candidate`：新渠道或待观察渠道。不会特殊惩罚；样本不足时只给最小探测权重。
- `role: quarantine`：已知但当前不允许承载流量。线上权重大于 0 时会建议清零。

## 质量如何自动判断

调度器读取 Bifrost 日志后，会按 provider 统计：

- 请求数 `total`
- 成功数 `success`
- 失败数 `errors`
- 错误率 `error_rate`
- 成功率 `success_rate`
- 成功请求 P95 延迟 `p95_latency_ms`
- timeout / stream idle 次数
- 凭证、额度、无可用 token 等关键错误
- 连续坏窗口数 `consecutive_bad_windows`
- 连续慢窗口数 `consecutive_slow_windows`

所以你不需要在配置里写 `quality_score`。质量是运行时事实，不是静态猜测。

## 主动首字测速

Bifrost 当前 `/api/logs` 里的 `latency` 是完整请求耗时，不是首字时间。
如果你要“首字快优先，其次成本”，需要开启 `probe`：

```json
{
  "probe": {
    "enabled": true,
    "model": "gpt-5.5",
    "prompt": "ping",
    "samples": 2,
    "timeout": "20s"
  }
}
```

开启后，每轮调度会对每个允许的 provider 发少量流式请求：

- 请求模型写成 `provider/model`，例如 `zz1cc_openai_lv4/gpt-5.5`，让 Bifrost 定向到这个 provider。
- 请求头使用对应 Virtual Key 的 `x-bf-vk`。
- 收到第一条 SSE `data:` 时记录 `p95_ttft_ms`。
- 完整响应结束时记录主动测速总耗时 `p95_probe_latency_ms`。
- `role: quarantine` 或 `allowed: false` 的 provider 不测速。

注意：

- 主动测速会产生真实上游请求，可能产生少量费用。
- 建议先保持 `mode: read_only` 观察报告里的 `首字 P95 ms`，确认合理后再考虑 `guarded_write + --apply`。
- 如果某个 provider 不支持全局 `probe.model`，可以在 provider 里写 `probe_model` 覆盖。
- 调度器不会把 Bifrost 日志的 `latency` 当成首字。

## 防误判机制

调度器默认用 `window=15m`、`quality_windows=3`，也就是总共看最近 45 分钟，并拆成 3 个 15 分钟子窗口。

默认保护规则：

- 请求数少于 `minimum_attempts=10`：不按错误率处罚。
- 普通错误少于 `min_errors=3`：不按错误率处罚。
- 只有单个窗口坏：最多降到 `min_weight`，不直接清零。
- 必须达到 `required_bad_windows=2` 个连续坏窗口，才允许清零或禁用。
- P95 延迟默认只展示，不自动调权。只有你显式设置 `max_p95_latency_ms` 后，才会要求至少 `min_latency_samples=5` 个成功样本，并且达到连续慢窗口要求才降权。
- 如果同一轮计划里的清零/禁用会让池子低于 `min_active_providers=1`，会保留最小权重。
- 新渠道无样本或样本太少时，只给最小探测权重。

这不能保证永远不误判，但可以避免最常见的误判来源：单次错误、低样本、瞬时抖动、池名猜测、人工静态质量分。

## 顶层字段

| 字段 | 类型 | 必填 | 默认值 | 可配值 | 作用 |
| --- | --- | --- | --- | --- | --- |
| `mode` | string | 否 | `read_only` | `read_only`, `guarded_write` | 写入安全开关。`read_only` 永远只预览；`guarded_write` 也必须配合命令行 `--apply` 才会写线上。 |
| `api` | object | 是 | 无 | 见下方 | Bifrost 实例连接信息。 |
| `window` | duration string | 否 | `15m` | `5m`, `15m`, `1h`, `1h30m` 等 Go duration | 单个质量判断子窗口长度。 |
| `quality_windows` | integer | 否 | `3` | 正整数 | 质量判断要回看几个子窗口。总统计长度等于 `window * quality_windows`。 |
| `minimum_attempts` | integer | 否 | `10` | 正整数 | 单个 provider 总请求数少于该值时，不按错误率降权或清零。 |
| `cooldown` | duration string | 否 | `30m` | `10m`, `30m`, `1h` 等 Go duration | 预留冷却时间参数。当前版本已解析但还未参与调度决策。 |
| `probe` | object | 否 | 关闭 | 见下方 | 主动流式测速配置。开启后用于首字优先调度。 |
| `pools` | array | 是 | 无 | 见下方 | 要治理的 Bifrost Virtual Key 列表。 |

`duration string` 使用 Go 的 `time.ParseDuration` 格式，例如 `30s`, `5m`, `1h30m`。不支持 `1d`。

## `api`

| 字段 | 类型 | 必填 | 默认值 | 可配值 | 作用 |
| --- | --- | --- | --- | --- | --- |
| `base_url` | string | 是 | 无 | URL，如 `https://your-bifrost.example.com` | Bifrost 实例地址。也可以用命令行 `--api-url` 或环境变量 `BIFROST_API_URL` 覆盖。 |

认证不写在 JSON 里，通过 `.env`、环境变量或命令行传入：

| 名称 | 来源 | 必填 | 默认值 | 作用 |
| --- | --- | --- | --- | --- |
| `BIFROST_API_USERNAME` / `--api-username` | 环境变量或命令行 | 是 | 无 | Bifrost Dashboard/admin 用户名。 |
| `BIFROST_API_PASSWORD` / `--api-password` | 环境变量或命令行 | 是 | 无 | Bifrost Dashboard/admin 密码。 |
| `BIFROST_API_URL` / `--api-url` | 环境变量或命令行 | 否 | `api.base_url` | 覆盖配置文件里的 Bifrost 地址。 |
| `BIFROST_API_TIMEOUT` / `--api-timeout` | 环境变量或命令行 | 否 | `30s` | API 请求超时。 |
| `BIFROST_SCHEDULER_CONFIG` / `--config` | 环境变量或命令行 | 否 | `config.example.json` | 配置文件路径。 |
| `BIFROST_SCHEDULER_FORMAT` / `--format` | 环境变量或命令行 | 否 | `markdown` | 输出格式，可配 `markdown` 或 `json`。 |

调度器启动后会先调用 `/api/session/login` 自动登录，后续请求自动使用返回的 token 或 session cookie。

## `probe`

| 字段 | 类型 | 必填 | 默认值 | 可配值 | 作用 |
| --- | --- | --- | --- | --- | --- |
| `enabled` | boolean | 否 | `false` | `true`, `false` | 是否启用主动首字测速。关闭时不发任何额外模型请求。 |
| `model` | string | 否 | `gpt-5.5` | Bifrost 模型名，不带 provider 前缀 | 默认测速模型。实际请求会自动变成 `provider/model`。 |
| `prompt` | string | 否 | `ping` | 任意短提示词 | 测速提示词。建议越短越好，减少费用。 |
| `samples` | integer | 否 | `2` | `1` 到 `5` | 每个 provider 每轮测速次数。上限 5 是为了防止误配置导致费用过高。 |
| `timeout` | duration string | 否 | `20s` | `5s`, `20s`, `1m` 等 Go duration | 单次主动测速最长等待时间。 |

## Telegram 通知

Telegram 通知不写进 JSON 配置，使用环境变量或命令行参数。这样 bot token 不会进入公开配置模板。

| 名称 | 来源 | 必填 | 默认值 | 作用 |
| --- | --- | --- | --- | --- |
| `BIFROST_SCHEDULER_TG_BOT_TOKEN` / `--telegram-bot-token` | 环境变量或命令行 | 否 | 空 | Telegram BotFather 给你的 bot token。 |
| `BIFROST_SCHEDULER_TG_CHAT_ID` / `--telegram-chat-id` | 环境变量或命令行 | 否 | 空 | 通知目标 chat id，可以是个人、群组或频道。 |
| `BIFROST_SCHEDULER_TG_THREAD_ID` / `--telegram-thread-id` | 环境变量或命令行 | 否 | 空 | Telegram 群组话题 ID，也就是 `message_thread_id`。不用话题就不填。 |
| `BIFROST_SCHEDULER_TG_INTERACTIVE` / `--telegram-interactive` | 环境变量或命令行 | 否 | `false` | 是否在 daemon 模式下开启 Telegram 交互控制台。 |

行为：

- token 和 chat id 都为空：通知关闭。
- 只填 token 或只填 chat id：认为配置错误，调度器会写错误日志，但不会停止调度。
- 本轮没有 `decisions`：不发送通知。
- `plan` 命令：只要本轮有 `decisions`，发送一次。
- `daemon` 命令：只有变更指纹变化时才发送；同一批变更不会每个 interval 重复刷屏。
- 动作较多或消息较长时：自动拆成多条 Telegram 发送，不会只显示前几条；如果开启交互按钮，按钮只放在最后一条。
- Telegram 发送失败：只写日志，不阻断 Bifrost 调度。
- `BIFROST_SCHEDULER_TG_INTERACTIVE=true` 时，daemon 会额外启动 Telegram 长轮询，读取命令和按钮点击。
- Telegram 交互只允许 `BIFROST_SCHEDULER_TG_CHAT_ID` 对应的 chat 操作；其他 chat 的消息会被拒绝。
- Telegram 交互不提供直接写线上命令。`/run` 永远是 dry-run。
- Telegram 交互回复使用 HTML 富文本；执行 `/run` 这类慢命令时会发送 `typing` 状态，让客户端显示“正在输入...”。

`.env` 示例：

```env
BIFROST_SCHEDULER_TG_BOT_TOKEN=123456:bot-token-from-botfather
BIFROST_SCHEDULER_TG_CHAT_ID=-1001234567890
# BIFROST_SCHEDULER_TG_THREAD_ID=123
BIFROST_SCHEDULER_TG_INTERACTIVE=true
```

交互命令：

| 命令 | 作用 | 是否写 Bifrost |
| --- | --- | --- |
| `/status` | 查看 daemon 是否运行、运行间隔、最近错误、最近决策数。 | 否 |
| `/last` | 查看最近一次调度计划摘要。 | 否 |
| `/run` | 立即执行一次手动 dry-run 预览。 | 否 |
| `/mute 1h` | 静音 Telegram 变更通知 1 小时。调度器仍继续运行。 | 否 |
| `/unmute` | 恢复 Telegram 变更通知。 | 否 |
| `/help` | 查看帮助。 | 否 |

为什么 `/run` 不写线上：

- Telegram 交互适合临时查看和手动预览，不适合作为高风险写入入口。
- 自动写入仍然只由 daemon 自己的运行参数控制：`config.json` 必须是 `guarded_write`，并且 daemon 命令必须带 `--apply`。
- 这样即使 Telegram bot token 泄露，攻击面也限制在查询和 dry-run，不会直接改 Bifrost 权重。

## `pools[]`

一个 pool 对应一个 Bifrost Virtual Key。

| 字段 | 类型 | 必填 | 默认值 | 可配值 | 作用 |
| --- | --- | --- | --- | --- | --- |
| `id` | string | 是 | 无 | 任意唯一名称 | 调度器内部展示和匹配用。不能重复。不会按名称触发特殊策略。 |
| `virtual_key` | string | 是 | 无 | Bifrost VK 名称 | 必须和 Bifrost 里的 Virtual Key `name` 完全一致。 |
| `kind` | string | 否 | `text` | 任意标签，常用 `text`, `image` | 仅用于输出展示。当前策略不根据 `kind` 分支。 |
| `min_active_providers` | integer | 否 | `1` | 正整数 | 达到禁用阈值时，如果清零会让活跃 provider 数低于该值，则保留最小权重。 |
| `rules` | object | 否 | 统一默认策略 | 见下方 | 覆盖该 pool 的调度阈值和默认权重。通常不用写。 |
| `providers` | array | 是 | 无 | 见下方 | 该 pool 允许、候选或隔离的 provider 列表。 |

如果 Bifrost VK 里存在某个 provider，但配置文件没有写它，调度器会认为它不该属于这个池，输出 `set_weight_zero` 计划。

## `pools[].providers[]`

每个 provider 必须对应 Bifrost 里真实的 provider 名称。

| 字段 | 类型 | 必填 | 默认值 | 可配值 | 行为 |
| --- | --- | --- | --- | --- | --- |
| `name` | string | 是 | 无 | Bifrost provider 名称 | 必须和 Bifrost 里的 provider 完全一致。 |
| `cost_weight` | number | 否 | `rules.default_cost_weight`，默认 `1` | `0` 或正数 | provider 健康时恢复到的目标权重。越高代表越便宜或越想优先使用。 |
| `role` | string | 否 | `fallback` | `primary`, `fallback`, `candidate`, `quarantine` 或自定义字符串 | 当前只有 `quarantine` 会禁止承载流量。`candidate` 主要用于标记新渠道，行为和普通 provider 一样，但人读更清楚。 |
| `allowed` | boolean | 否 | `true` | `true`, `false` | 是否允许该 provider 在这个 pool 里有权重。一般不用写；需要禁用时更推荐写 `role: quarantine`。 |
| `min_weight` | number | 否 | `rules.min_weight`，默认 `0.05` | `0` 或正数 | 探测、保护或恢复初期使用的最小权重。 |
| `probe_model` | string | 否 | `probe.model` | Bifrost 模型名，不带 provider 前缀 | 这个 provider 单独使用的测速模型。 |

常见写法：

```json
{
  "name": "cheap_provider_a",
  "cost_weight": 1
}
```

```json
{
  "name": "new_provider_b",
  "role": "candidate",
  "cost_weight": 0.2,
  "min_weight": 0.05
}
```

```json
{
  "name": "known_bad_provider_c",
  "role": "quarantine"
}
```

### `cost_weight` 到底是什么

`cost_weight` 不是质量分。它只是健康状态下的目标权重。

推荐理解：

- 便宜、你愿意多用：配高一点，比如 `1`。
- 中等成本：配 `0.4` 到 `0.8`。
- 贵、只想兜底或探测：配低一点，比如 `0.1` 到 `0.2`。

质量由运行日志自动调度：

- 健康：恢复到 `cost_weight`。
- 开启主动测速且样本足够：先按首字速度重新计算目标权重，再考虑恢复到 `cost_weight`。
- 样本不足：只给 `min_weight` 探测。
- 错误率高：降权。
- 连续坏窗口达到阈值：清零或禁用。
- 恢复后成功率达标：先以最小权重重新进入，再恢复到 `cost_weight`。

### `role: "candidate"` 是什么

`candidate` 表示“新渠道或待观察渠道”。

当前行为：

- 不会因为它是 candidate 就被隔离。
- 低样本时只给最小探测权重。
- 有足够成功样本后，按和其他 provider 一样的质量规则恢复到 `cost_weight`。
- 如果连续坏窗口达标，也会和其他 provider 一样被降权、清零或禁用。

它适合刚新加的 provider，例如你想让无人值守系统小流量试用，而不是一上来吃满流量。

### `role: "quarantine"` 是什么

`quarantine` 表示“这个 provider 是已知渠道，但当前不允许在这个 pool 里承载流量”。

调度器看到 `role: "quarantine"` 后会这样处理：

- 如果线上当前权重已经是 `0`：不动作。
- 如果线上当前权重大于 `0`：输出 `set_weight_zero`，在允许写入时把它在这个 VK 里的权重改成 `0`。
- 不删除 provider。
- 不删除 Virtual Key。
- 不修改 key 内容。
- 不因为它之后变健康就自动恢复权重。

它和“配置里不写这个 provider”的区别：

| 写法 | 含义 | 调度器行为 |
| --- | --- | --- |
| 配置里有 provider，并写 `role: "quarantine"` | 我知道这个 provider 存在，但现在故意隔离 | 如果线上权重大于 `0`，建议清零；如果已是 `0`，保持不动。 |
| Bifrost VK 里有 provider，但配置里没写 | 这个 provider 不在调度器允许名单里 | 输出 `set_weight_zero`，提醒配置和线上不一致。 |

`quarantine` 适合已知不该承载流量的渠道；新渠道不要标成 `quarantine`，应该用 `candidate`。

## `pools[].rules`

通常不用写。只有你想覆盖内置策略时才写 `rules`。

| 字段 | 类型 | 必填 | 默认值 | 可配值 | 行为 |
| --- | --- | --- | --- | --- | --- |
| `max_error_rate` | number | 否 | `0.5` | `0` 到 `1` | 错误率大于该值时，建议降权到 `cost_weight * (1 - error_rate)`，不低于 `min_weight`。 |
| `disable_error_rate` | number | 否 | `0.8` | `0` 到 `1` | 错误率大于等于该值，且连续坏窗口达标时，建议权重清零。 |
| `max_timeout_or_idle` | integer | 否 | `10` | 正整数 | 单窗口超时或 stream idle 次数大于该值时，该窗口算坏窗口。不写或写 `0` 都会使用默认值 `10`。 |
| `max_p95_latency_ms` | number | 否 | `0` | `0` 或正数 | 单窗口成功请求 P95 延迟大于该值时，该窗口算慢窗口。连续慢窗口达标才建议权重减半。默认 `0`，表示只展示延迟，不因延迟自动调权。 |
| `max_p95_ttft_ms` | number | 否 | `0` | `0` 或正数 | 主动测速首字 P95 大于该值时建议降权。默认 `0`，表示不按绝对首字阈值降权。 |
| `min_success_rate_for_recovery` | number | 否 | `0.95` | `0` 到 `1` | 成功率达到该值时，才允许从 0 恢复或恢复到目标权重。 |
| `min_errors` | integer | 否 | `3` | 正整数 | 错误数少于该值时，不按错误率处罚。 |
| `critical_error_threshold` | integer | 否 | `2` | 正整数 | 单窗口关键错误达到该值时，该窗口算坏窗口。 |
| `min_latency_samples` | integer | 否 | `5` | 正整数 | 至少有这么多成功样本时，P95 延迟才参与判断。 |
| `min_probe_samples` | integer | 否 | `2` | 正整数 | 至少有这么多主动测速成功样本时，首字结果才参与调权。 |
| `required_bad_windows` | integer | 否 | `2` | 正整数 | 允许清零或禁用前，需要达到的连续坏窗口数。 |
| `ttft_priority` | number | 否 | `0.75` | `0` 到 `1` | 首字速度在“首字 + 成本”综合目标权重中的占比。`0.75` 表示首字优先。 |
| `min_weight_change` | number | 否 | `0.02` | `0` 或正数 | 计算出的目标权重和当前权重差距小于该值时不生成动作，避免频繁抖动。 |
| `max_weight_step` | number | 否 | `0.2` | 正数 | 健康恢复和主动测速健康调权的单轮最大权重变化。异常降权不受这个限制，仍可快速降到探测权重。 |
| `default_cost_weight` | number | 否 | `1` | 正数 | provider 没写 `cost_weight` 时使用的健康目标权重。 |
| `min_weight` | number | 否 | `0.05` | `0` 或正数 | provider 没写 `min_weight` 时使用的最小探测/保护权重。 |

比例字段是 `0` 到 `1`，不是百分数字符串。比如 `0.8` 表示 80%。

## 输出动作

调度器输出里的 `action` 可能是：

| 动作 | 会做什么 | 触发原因 |
| --- | --- | --- |
| `set_weight` | 修改某个 VK provider config 的权重 | 健康恢复、错误率超阈值但未满足连续坏窗口、P95 延迟连续慢窗口达标、主动首字测速重新分配、样本太少但需要探测。 |
| `set_weight_zero` | 把某个 VK provider config 的权重设为 `0` | provider 不允许、被标记为 quarantine、错误率达到清零阈值且连续坏窗口达标、或 Bifrost 中存在但配置中缺失。 |
| `disable_provider` | 先权重设为 `0`，再尝试禁用该 provider 绑定 key | 凭证、额度或无可用 token 等关键错误达到阈值，并且连续坏窗口达标。 |
| `disable_provider_keys` | 不改权重，只继续尝试禁用该 provider 绑定 key | 上一轮已经把权重清零，但禁用 key 可能失败；后续仍看到关键错误时会重试 key 禁用。 |
| `review_missing_provider` | 不自动写，提示人工检查 | 配置里有 provider，但目标 Bifrost VK 中没有对应 provider config。 |

## 写入条件

调度器必须同时满足两个条件才会写线上：

1. 配置文件里 `mode` 是 `guarded_write`
2. 命令行显式带 `--apply`

只要其中任意一个不满足，输出就是 dry-run 预览，不会 PUT 到 Bifrost。
