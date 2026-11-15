<div align="center"><img src="https://img.shields.io/badge/Contribution-cccccc" alt="Contribution"><img src="https://img.shields.io/badge/🤖%20Agent-100%25-94C9E7" alt="Agent"><img src="https://img.shields.io/badge/🧑‍💻%20Human-0%25-A86F4D" alt="Human"></div>

# SvcDeck · Windows 本地服务面板

SvcDeck 是一个**单二进制** Windows 本地服务面板，用来管理一批**不注册为 Windows 服务**的裸 exe：按依赖启动、用 HTTP / TCP / 进程探针判断存活、DOWN 后按服务粒度重启。

> **面板死，服务继续活；面板重启后接管已在运行的进程，不重复拉起，也不杀旧进程。**

## 核心约束

| 约束 | 实现 |
| --- | --- |
| 子进程脱离面板 | `DETACHED_PROCESS \| CREATE_NEW_PROCESS_GROUP`，spawn 后 `Process.Release()` |
| 不绑定生命周期 | 没有 Job Object / `KILL_ON_JOB_CLOSE`，不 `Wait()` 子进程 |
| 面板退出不杀子进程 | 退出时只关 HTTP、停探针、关 SQLite，不执行任何 `taskkill` |
| 启动前先接管 | `pid + 进程创建时间(FILETIME) + exe 路径` 三重校验，防 PID 复用 |
| 探针是最终真相 | 探针 UP 即认为在跑并 adopt；DOWN 才进入 spawn / 保活 |
| 只杀自己的树 | 手动 Stop 或保活重启只执行 `taskkill /PID <pid> /T`（必要时 `/F`） |

## 功能

- **探针看板**：总数 / UP / DOWN / 熔断统计，DOWN 置顶，4 秒轮询，日志抽屉。
- **服务管理**：CRUD、依赖拓扑、启用开关、HTTP/TCP/进程探针、保活退避、运行中配置变更提示「需 Restart」。
- **全局设置**：探针间隔、失败阈值、停止宽限、日志上限、Webhook、监听地址、Token、等依赖 UP、删除运行中服务。
- **运维**：命名 Mutex 单例、SQLite WAL、adopt、端口残留清理、熔断持久化、按服务粒度 taskkill。
- **AI 接入**：同一个 `SvcDeck.exe` 内置 `POST /mcp`（Streamable HTTP），无额外进程。

## 快速开始

要求 Go 1.26+（开发验证于 Go 1.27 / Windows amd64）。第三方依赖只有 `golang.org/x/sys/windows` 和纯 Go 的 `modernc.org/sqlite`。

```powershell
# 开发版：带控制台，日志直接打在终端
go build -trimpath -ldflags "-s -w" -o SvcDeck.exe .

# 交付版：GUI 子系统，双击无窗口；从终端启动会自动接管父控制台
go build -trimpath -ldflags "-s -w -H windowsgui" -o SvcDeck.exe .
```

```powershell
# 默认数据目录 = exe 所在目录，监听 127.0.0.1:8088
.\SvcDeck.exe

# 推荐把数据放到固定目录
.\SvcDeck.exe -data "D:\SvcDeckData"

# 临时覆盖监听地址 / Token（不写回数据库）
.\SvcDeck.exe -data "D:\SvcDeckData" -addr 127.0.0.1:18088 -token my-secret
```

| 参数 | 说明 |
| --- | --- |
| `-data` | 数据目录；`panel.db` 与 `logs/` 放这里，默认取 exe 所在目录 |
| `-db` | 直接指定 SQLite 文件路径，优先于 `-data` 推导 |
| `-addr` | 覆盖数据库中的 `listen_addr`，仅本次进程有效 |
| `-token` | 覆盖数据库中的 `token`，仅本次进程有效 |

浏览器打开 `http://127.0.0.1:8088/`。

**日志去向**：GUI 版双击启动写 `<data>/logs/panel.log`；从终端启动跟随终端；`> out.log 2>&1` 可重定向。业务日志始终写 `logs/<服务id>.log`，按 `log_max_mb` 轮转；`panel.log` 不轮转，长期运行需自行清理。

## 运行机制

**启动与 adopt**：读取 `enabled=1` 的服务，按 `depends_on` 拓扑排序；逐个先校验 `runtime_state` 中的 PID / 创建时间 / exe 路径，匹配则直接接管；不匹配再看探针，UP 则按唯一 exe 找回 PID，仍失败才 spawn。`manual_stop` 或 `circuit_open` 的服务会被跳过，避免面板重启破坏用户意图。

**探针**：全局间隔默认 5 秒，单服务超时默认 5 秒；支持 HTTP（状态码 / 关键词）、TCP、进程三种。连续失败达到 `fail_threshold`（默认 3）才进入保活；刚 spawn 后有 `max(15s, timeout+5s)` 启动宽限，避免慢启动被误杀。

**保活与熔断**：按 `restart_backoff_sec`（默认 `[1,2,5,30]`）重启；`restart_max_retries=0` 表示只显示 DOWN、不自动重启；达到上限置 `circuit_open` 并发一次可选 Webhook。探针 UP 后清零失败计数与熔断。熔断状态持久化，需手动 Start / Restart 复位。

**Stop 与退出**：Stop 只杀该服务自己的进程树；先 `taskkill /PID <pid> /T`，超过 `stop_grace_sec` 再 `/F`；`/T` 因个别子进程拒绝访问整体失败时，会兜底直接终止根进程。手动 Stop 会写 `manual_stop=1`，抑制保活直到手动 Start / Restart。面板退出路径没有任何子进程终止操作，强制结束面板也不影响业务进程。

运行中修改 exe / args / cwd / env 不会触发面板擅自重启；API 返回 `pending_restart`，前端提示需 Restart 生效。

## 数据

SQLite 默认 `panel.db`，开启 WAL，单连接，`busy_timeout=5000`，多步写入使用事务。

| 表 | 内容 |
| --- | --- |
| `services` | 服务配置（exe/args/cwd/env/依赖/探针/保活） |
| `runtime_state` | PID、进程创建时间、exe 路径、UP/失败/重试、熔断、`manual_stop`、退避时间、启动签名 |
| `settings` | 全局设置 |
| `probe_history` | 可选探测历史，按 `probe_history_limit` 保留 |

默认设置：`probe_interval_sec=5`、`fail_threshold=3`、`stop_grace_sec=8`、`log_max_mb=10`、`probe_history_limit=200`、`listen_addr=127.0.0.1:8088`、`token=""`、`wait_deps=false`、`allow_delete_running=false`、`webhook_url=""`。

备份：停面板后复制 `panel.db`（以及 `-wal` / `-shm`）；运行中请用 SQLite 在线备份，不要直接复制。

## REST API

默认监听 `127.0.0.1:8088`。设置 `token` 后，所有 `/api/*` 请求需带：

```http
X-Panel-Token: <token>
```

或：

```http
Authorization: Bearer <token>
```

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/health` | 健康检查 |
| GET | `/api/status` | 探针看板全量状态 |
| GET | `/api/services` | 配置 + 运行态列表 |
| GET | `/api/services/{id}` | 单个服务 |
| POST | `/api/services` | 新增；校验 ID、exe、依赖与环 |
| PUT | `/api/services/{id}` | 编辑并热加载；启动配置变化返回 `pending_restart` |
| DELETE | `/api/services/{id}` | 删除；被依赖时 409；运行中默认 409，`?stop=true` 先停 |
| POST | `/api/start/{id}` | 启动；清除 `manual_stop` 与熔断 |
| POST | `/api/stop/{id}` | 停止；仅杀该服务树，抑制保活 |
| POST | `/api/restart/{id}` | 先停后起 |
| POST | `/api/restart-all` | 所有 enabled 服务按依赖逆序停、正序起 |
| GET | `/api/logs/{id}?lines=100` | 日志尾部，上限 2000 行 |
| GET | `/api/history/{id}?lines=100` | 探测历史 |
| GET | `/api/config` | 全局设置 |
| PUT | `/api/config` | 保存并热加载；`listen_addr` 变更需重启 |

新增服务示例：

```powershell
$body = @{
  id='order-api'; name='订单服务'; group='核心业务'; enabled=$true
  exe='C:\Services\order-api\order-api.exe'
  args=@('--port=9001'); cwd='C:\Services\order-api'
  env=@{ ASPNETCORE_ENVIRONMENT='Production' }
  depends_on=@('redis')
  probe_type='http'; probe_url='http://127.0.0.1:9001/health'
  probe_expect=200; probe_keyword='ok'; probe_timeout_sec=5
  restart_max_retries=5; restart_backoff_sec=@(1,2,5,30)
} | ConvertTo-Json -Depth 6

Invoke-RestMethod -Method Post -Uri http://127.0.0.1:8088/api/services `
  -ContentType 'application/json' -Body $body
```

成功返回 `{"ok":true,...}`，失败返回 `{"error":"..."}` 与 400 / 401 / 404 / 409 / 500。exe 必须存在且不是目录；依赖必须存在且不能成环。

## MCP（AI 直接管理面板）

MCP 已编进同一个 `SvcDeck.exe`，不需要额外 exe 或额外进程。面板启动后直接提供：

```text
POST http://127.0.0.1:8088/mcp
```

传输为 **Streamable HTTP**：客户端 POST JSON-RPC；notification 返回 `202`；`GET` / `DELETE` 返回 `405`（当前不主动推送）。它只调用面板 REST API，不直接读写 SQLite。

| 项 | 值 |
| --- | --- |
| URL | `http://127.0.0.1:8088/mcp` |
| Transport | `streamable-http` |
| Header | 面板设置 token 时：`X-Panel-Token: <token>` 或 `Authorization: Bearer <token>` |

DeepSeek Harness 在 MCP 连接器里选 `streamable-http`，URL 填上面的地址；有 token 则加对应 header。

工具：

- **查询**：`list_services`、`get_service`、`get_status`、`get_logs`、`get_history`
- **变更**：`add_service`、`update_service`、`start_service`、`stop_service`、`restart_service`、`delete_service`、`restart_all`
- `update_service` 是局部合并（先 GET 再 PUT），不会清空未传字段；`add_service` 的参数与 `POST /api/services` 相同。
- `stop_service` / `restart_service` / `delete_service` / `restart_all` 标记为破坏性工具，AI 应先得到用户确认。

## 开机自启

**只让面板自启，业务服务仍由面板拉起，不要注册成 Windows 服务。**

推荐任务计划（管理员 PowerShell）：

```powershell
$tr = '"C:\SvcDeck\SvcDeck.exe" -data "D:\SvcDeckData"'
schtasks /Create /TN "SvcDeck Panel" /F /SC ONLOGON /RL HIGHEST /TR $tr
```

需要开机即启动可把 `/SC ONLOGON` 改为 `/SC ONSTART`。在任务计划程序中取消「运行超时停止」，设置失败重试，并且**不要**配置「停止所有子进程」类选项。

WinSW 也可以，但必须确认它不会在停止时 `taskkill /T` 或使用带 `KILL_ON_JOB_CLOSE` 的 Job Object；只让它终止 Panel 进程本身。

## 安全与边界

- 默认只监听 `127.0.0.1`；跨机访问请设置 token，并在前面套 HTTPS 反向代理（本项目不带 TLS）。
- Token 存在本机 SQLite，浏览器存在 localStorage；数据库目录建议限制 ACL。
- 仅支持 Windows；Linux/macOS 不会编译通过。
- 同一 exe 多实例：进程探针在 PID 缺失时无法区分归属，不会按 exe 猜测，也不会按 exe 清理；建议用 HTTP/TCP 探针或确保已记录各自 PID。
- 禁用服务仍参与探针展示，但不会自动启动/保活；禁用**不会**停止已在运行的进程。
- 修改 `listen_addr` 后需重启面板；`panel.log` 不参与 `log_max_mb` 轮转。
- 面板默认不需要管理员权限；管理更高权限进程时需以对应权限运行，否则 `OpenProcess` / `taskkill` 会失败。

## 常见问题

**Q：面板退出后服务为什么还活着？**

A：设计要求。子进程 `DETACHED_PROCESS` 且不被 Wait/Job 绑定；下次启动通过 adopt 接管。

**Q：怎么确认接管的是旧进程？**

A：看日志 `adopt 成功：pid N 仍是 <exe>`；`process_start_time` 与进程创建时间精确匹配。PID 被复用时日志会提示 adopt 失败，之后按探针结果处理。

**Q：DOWN 很久但不重启？**

A：检查 `restart_max_retries` 是否为 0、是否熔断、是否 `manual_stop`、是否还在启动宽限内。

**Q：Stop 报 `taskkill ... exit status 255 拒绝访问`？**

A：通常是面板权限低于目标进程，或目标属于其他用户/会话。用相同或更高权限重启面板；必要时先在任务管理器结束该进程，再 Start。

**Q：Token 忘了？**

A：停面板，把 SQLite `settings` 表中的 `token` 改为空；或用 `SvcDeck.exe -token newtoken` 临时覆盖，进设置页清空并保存。

## 代码贡献

统计锚定在提交 `0ed3218`（截至该提交：2 个提交 / 6524 行增删）：

| 指标 | Agent | Human |
| --- | --- | --- |
| 提交数 | 2 / 2（100%） | 0 / 2（0%） |
| 增删行数 | 6524 / 6524（100%） | 0 / 6524（0%） |

统计口径：`git log HEAD --no-merges --numstat`，按 **commit author** 归属，**不看** `Co-authored-by`；只统计已提交内容。顶部徽章反映当前 `HEAD`，本表是锚定快照。

```powershell
git log HEAD --no-merges --numstat --format="%H%x09%an%x09%ae%x09%s"
```
