# 用这个项目学习 Go

这份文档按“先能看懂，再能改动”的顺序写。你不需要先完整学完 Go 语法，可以一边跑测试一边读代码。

## 零基础先读这里

如果你现在连“函数”都不熟，先把代码当成一条流水线：

```text
命令行输入
  -> 读取配置
  -> 登录 Bifrost
  -> 拉取当前权重和最近日志
  -> 算出是否要调整
  -> 输出报告
  -> 如果开启 apply，才写回 Bifrost
```

代码里的每个“函数”，就是流水线里的一个小步骤。

### 函数是什么

函数就是“给它一些东西，它做一件事，然后可能还你一个结果”。

比如：

```go
func add(a int, b int) int {
	return a + b
}
```

可以按中文读：

```text
定义一个函数，名字叫 add。
它需要两个整数：a 和 b。
它会返回一个整数。
函数里面做的事情是：返回 a + b。
```

这个项目里真实的例子：

```go
func buildPlan(ctx context.Context, opts options, apply bool) (domain.Plan, error)
```

按中文读：

```text
定义一个函数，名字叫 buildPlan。
它需要三个输入：
  ctx：取消/超时信号
  opts：命令行和环境变量配置
  apply：是否允许执行写入
它返回两个东西：
  domain.Plan：调度计划
  error：错误；如果没有错误就是 nil
```

Go 经常返回两个值：`结果, 错误`。所以你会经常看到：

```go
plan, err := buildPlan(ctx, opts, apply)
if err != nil {
	return 1
}
```

意思是：

```text
调用 buildPlan。
如果 err 不是空，说明失败了，直接返回错误退出码。
如果 err 是空，说明成功，继续用 plan。
```

### 变量是什么

变量就是给一个值起名字。

```go
apply := false
```

意思是：创建一个叫 `apply` 的变量，值是 `false`。

Go 里 `:=` 常用于“第一次创建变量”。之后再改值通常用 `=`。

### 参数是什么

参数就是函数需要你交给它的输入。

```go
runPlan(ctx, logger, opts, apply)
```

这句话是在调用 `runPlan`，给它 4 个参数：

- `ctx`
- `logger`
- `opts`
- `apply`

### 返回值是什么

返回值就是函数做完后交还给调用者的东西。

```go
return 0
```

在这个项目的 CLI 里，`0` 通常表示成功，`1` 表示失败，`2` 表示命令用法错误。

### `if err != nil` 是什么

这是 Go 里最常见的错误处理。

```go
if err != nil {
	return domain.Plan{}, err
}
```

按中文读：

```text
如果 err 不是空，说明刚才那一步失败了。
返回一个空计划，并把错误继续往上交。
```

你刚开始学 Go，先记住：看到 `err != nil`，就是“出错了，停止当前流程”。

### 花括号 `{}` 是什么

花括号表示“一段代码”。

```go
if apply {
	// 只有 apply 为 true，才执行这里
}
```

函数、if、for、switch 都会用花括号包住自己的代码块。

### `for` 是什么

`for` 是循环，表示重复做某件事。

```go
for _, decision := range plan.Decisions {
	// 对每一个 decision 做一些事
}
```

按中文读：

```text
遍历 plan.Decisions 里的每一项。
每一项临时叫 decision。
```

这里的 `_` 表示“这个值我不用”。`range` 会给两个值：下标和值。我们不用下标，所以写 `_`。

### `switch` 是什么

`switch` 是根据不同情况走不同分支。

```go
switch os.Args[1] {
case "plan":
	// 用户输入 plan
case "daemon":
	// 用户输入 daemon
default:
	// 其他情况
}
```

这个项目里 `run()` 用它判断你执行的是哪个子命令。

## 最简 Go 词典

你刚开始看代码，先认这些词就够：

| 词 | 你先这样理解 |
| --- | --- |
| `package` | 这个文件属于哪个代码包 |
| `import` | 这个文件要用哪些外部包 |
| `func` | 定义一个函数，也就是一个命名步骤 |
| `type` | 定义一种新类型，比如一张表格/一组字段 |
| `struct` | 一组字段放在一起 |
| `interface` | 规定“你必须会哪些动作” |
| `return` | 函数结束，并把结果交回去 |
| `if` | 如果满足条件，就执行这一段 |
| `else` | 否则执行这一段 |
| `for` | 循环 |
| `switch` / `case` | 按不同值走不同分支 |
| `nil` | 空，没有值；常用于表示没有错误 |
| `err` | 约定俗成的错误变量名 |
| `:=` | 创建新变量 |
| `=` | 给已有变量赋值 |
| `[]Type` | 一组 Type，叫 slice |
| `map[K]V` | key 到 value 的映射 |
| `*Type` | 指针，表示“指向某个 Type 的地址”；先不用深究 |
| `defer` | 函数结束时再执行，常用于关闭文件/连接 |
| `go` | 启动一个 goroutine，也就是轻量后台任务 |
| `sync.Mutex` | 互斥锁，保护多处代码同时读写同一份数据 |

## 先看运行入口

从 [cmd/bifrost-scheduler/main.go](../cmd/bifrost-scheduler/main.go) 开始。

你会看到 Go 程序的基本形状：

```go
func main() {
	os.Exit(run())
}
```

Go 可执行程序从 `main` 包里的 `main()` 函数开始。这里 `main()` 很短，只负责把真正逻辑交给 `run()`，这样测试和阅读都更清楚。

建议先看这几个函数。这里的“函数”你可以先理解成“一个命名好的步骤”：

1. `run()`：解析子命令，决定执行 `plan`、`daemon` 还是 `version`。
2. `commonFlags()`：命令行参数和环境变量如何进入程序。
3. `buildPlan()`：把配置、Bifrost API 客户端、调度器组装起来。
4. `runDaemon()`：用 `time.Ticker` 每隔一段时间跑一次。
5. `startTelegramControl()`：如果开启 Telegram 交互，就启动后台 goroutine 监听命令。

## 再看项目分层

这个项目按轻量 DDD 分层。你可以把它理解成“每个目录只负责一类事情”。

| 目录 | 负责什么 | 适合学什么 |
| --- | --- | --- |
| `cmd/bifrost-scheduler` | CLI、daemon、日志 | `main`、`flag`、环境变量、进程退出码 |
| `internal/domain/scheduler` | 业务模型和调度规则 | `struct`、方法、切片、map、纯函数 |
| `internal/app/scheduler` | 应用用例编排 | `interface`、依赖反转、`context.Context` |
| `internal/bifrost` | Bifrost HTTP API | `net/http`、JSON、错误处理 |
| `internal/report` | 输出 JSON/Markdown | `io.Writer`、字符串拼接、格式化 |

## Telegram 交互里新增的 Go 概念

新增的 [cmd/bifrost-scheduler/telegram_control.go](../cmd/bifrost-scheduler/telegram_control.go) 适合学习“后台任务”和“共享状态”。

### goroutine 是什么

代码里有一句：

```go
go control.run(ctx)
```

按中文读：

```text
启动一个后台任务，让 control.run(ctx) 自己跑。
当前函数不用等它结束，可以继续往下执行。
```

daemon 主循环负责每 5 分钟调度一次；Telegram goroutine 负责等用户发 `/status`、`/last`、`/run`。它们同时运行，互不阻塞。

### Mutex 是什么

`daemonState` 里有：

```go
mu sync.Mutex
```

`Mutex` 是互斥锁。原因是：

- daemon 主循环会写最近计划。
- Telegram goroutine 会读最近计划。
- 如果两边刚好同时操作同一份数据，可能读到一半被改掉。

所以读写状态前会：

```go
s.mu.Lock()
defer s.mu.Unlock()
```

按中文读：

```text
先上锁。
函数结束时自动解锁。
上锁期间，其他地方要等这里处理完。
```

### 为什么 Telegram `/run` 不写线上

`telegram_control.go` 里手动运行计划时写的是：

```go
plan, err := buildPlan(ctx, c.opts, false)
```

最后一个参数 `false` 就是“不要 apply”。所以即使 daemon 自己启动时带了 `--apply`，Telegram `/run` 仍然只做 dry-run 预览。

这是故意的安全设计：Telegram 适合查询和预览，不适合直接做生产写入入口。

先读 `domain`，再读 `app`，最后读 `bifrost`。API 客户端代码最多，第一次看会比较吵。

## 主动首字测速怎么读

首字测速不是从 Bifrost 日志里猜出来的。代码路径是：

```text
config.go 的 ProbeConfig
  -> planner.go 调用 LoadProbeMetrics
  -> client.go 发 provider/model 流式请求
  -> 收到第一条 SSE data 记录 TTFT
  -> decision_service.go 在业务样本足够时按首字优先、成本其次算目标权重
```

重点看这几个函数：

- `NormalizeConfig()`：给 `probe` 补默认值，并限制 `samples <= 5`。
- `LoadProbeMetrics()`：只有 `probe.enabled=true` 才会发主动测速请求。
- `probeOnce()`：真正发 `/v1/chat/completions` 流式请求，记录首字时间。
- `MergeProbeMetrics()`：把主动测速结果合并到普通指标里。
- `probeDecision()`：用主动测速证据做明确异常处理；业务样本足够时才做首字重排。

这里要记住一个边界：`p95_latency_ms` 是 Bifrost 日志里的总耗时；`p95_ttft_ms` 是主动流式测速拿到的首字时间。

## Go 里的几个核心概念

### `package`

每个目录基本就是一个包。比如：

```go
package scheduler
```

表示这个文件属于当前目录的 `scheduler` 包。同一个包里的文件可以直接互相调用未导出的函数。

名字首字母大写表示导出，别的包可以用：

```go
func NewPlanner(...) Planner
```

名字首字母小写表示只在当前包内可用：

```go
func (p Planner) applyDecision(...)
```

### `struct`

`struct` 是一组字段。比如 `ProviderMetric` 表示一个 provider 最近窗口里的统计数据。

```go
type ProviderMetric struct {
	Provider string
	Total    int
	Errors   int
}
```

你可以把它当成 TypeScript interface + 运行时数据结构。

### 方法

Go 的方法接收者写在函数名前面：

```go
func (s DecisionService) Decide(...) []Decision
```

意思是 `DecisionService` 有一个方法叫 `Decide`。调用时写：

```go
decider.Decide(states, metrics, false)
```

### `interface`

`internal/app/scheduler/planner.go` 里有这个接口：

```go
type Store interface {
	LoadProviderStates(...)
	LoadMetrics(...)
	LoadProbeMetrics(...)
	SetProviderWeight(...)
	SetProviderKeysEnabled(...)
}
```

Go 的接口是隐式实现的。`internal/bifrost.BifrostClient` 只要有这些方法，就自动满足 `Store`，不需要写 `implements`。

这就是项目分层的关键：应用层只知道“我需要一个 Store”，不知道它背后是 HTTP、数据库还是测试 fake。

### `context.Context`

凡是可能阻塞、访问网络、等 IO 的函数，通常会带 `ctx context.Context`。

它用来传递取消信号和超时。比如 daemon 收到 SIGTERM 后，`ctx.Done()` 会被关闭，程序可以干净退出。

### `error`

Go 不用异常。函数失败时通常返回 `(value, error)`。

```go
cfg, err := appscheduler.LoadConfig(opts.ConfigPath)
if err != nil {
	return domain.Plan{}, err
}
```

阅读 Go 代码时，先习惯这种模式：每一步做完都检查 `err`。

### slice 和 map

slice 类似动态数组：

```go
var decisions []Decision
decisions = append(decisions, decision)
```

map 是 key-value：

```go
statesByPoolProvider := map[string]PoolProviderState{}
statesByPoolProvider[key(state.PoolID, state.Provider)] = state
```

这个项目经常把 `poolID + provider` 拼成 key，用来快速查某个 provider 的状态或指标。

## 推荐阅读路线

### 第一遍：理解程序怎么跑

1. [cmd/bifrost-scheduler/main.go](../cmd/bifrost-scheduler/main.go)
2. [internal/app/scheduler/planner.go](../internal/app/scheduler/planner.go)
3. [internal/domain/scheduler/models.go](../internal/domain/scheduler/models.go)

目标：知道一次 `plan` 命令从哪里开始，最后怎么生成 `Plan`。

### 第二遍：理解调度规则

1. [internal/domain/scheduler/config.go](../internal/domain/scheduler/config.go)
2. [internal/domain/scheduler/decision_service.go](../internal/domain/scheduler/decision_service.go)
3. [internal/domain/scheduler/decision_service_test.go](../internal/domain/scheduler/decision_service_test.go)

目标：看懂什么情况下恢复权重、降到最小探测权重、清零、禁 key。

### 第三遍：理解外部 API

1. [internal/bifrost/client.go](../internal/bifrost/client.go)
2. [internal/bifrost/client_test.go](../internal/bifrost/client_test.go)

目标：看懂登录、请求 Bifrost API、解析日志、更新 VK provider config。

## 最适合你练手的改动

从小到大：

1. 改 `README.md` 或 `docs/configuration.md` 的文字，然后跑 `go test ./...`。
2. 在 `internal/report/plan.go` 里调整 Markdown 输出文案。
3. 在 `internal/domain/scheduler/decision_service_test.go` 里新增一个测试场景。
4. 再去改 `internal/domain/scheduler/decision_service.go` 的规则。

不要一开始就改 `internal/bifrost/client.go`，那里主要是 HTTP 细节，学习收益低，出错成本高。

## 常用命令

```bash
go test ./...
```

跑所有测试。

```bash
go test ./internal/domain/scheduler -run TestCostWeightIsHealthyTargetWeight -v
```

只跑一个测试。

```bash
go run ./cmd/bifrost-scheduler version
```

直接运行 CLI。

```bash
go run ./cmd/bifrost-scheduler plan --config config.example.json --format markdown
```

用示例配置跑计划。需要真实 Bifrost API 时，要先配置环境变量。

## 看不懂时先问这三个问题

1. 这个文件属于哪一层？
2. 这个函数是在做业务判断，还是在做外部 IO？
3. 这个类型是输入、输出，还是中间状态？

能回答这三个问题，项目大部分代码就能慢慢读下去了。
