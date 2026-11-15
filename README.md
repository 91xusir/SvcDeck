# SvcDeck · Windows 本地服务面板

SvcDeck 是一个**单二进制**的 Windows 本地服务面板，用来管理一批**不注册为 Windows 服务**的裸 exe 业务程序。

它只做三件事：

1. **拉起者**：按依赖顺序启动 exe；
2. **探活者**：用 HTTP / TCP / 进程探针判断是否存活；
3. **按需重启者**：只有探针持续 DOWN 或用户手动操作时，才按服务粒度 `taskkill` 并重启。

面板本身**不是**业务进程的生命周期绑定者：

> **面板死，服务继续活；面板重启后接管已在跑的服务，而不是重复拉起或杀掉旧的。**

---

## 0. 核心模型与硬约束（不可改）

| 约束 | 实现 |
| --- | --- |
| 子进程脱离面板 | `DETACHED_PROCESS \| CREATE_NEW_PROCESS_GROUP`（`0x00000008 \| 0x00000200`） |
| 禁止 Job Object + `KILL_ON_JOB_CLOSE` | 代码中完全没有 Job Object，也没有 `CreateJobObject` / `SetInformationJobObject` |
| 禁止面板退出时 `taskkill /T` 全部子进程 | 退出路径只关 HTTP、取消探针循环、关 SQLite，不发送任何 taskkill |
| 启动前先接管 | 读取 `runtime_state.pid + process_start_time + exe_path`，用 `OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION)` + `QueryFullProcessImageName` + `GetProcessTimes` 校验 |
| 探针是最终真相 | 探针 UP 即认为在跑并 adopt；探针 DOWN 才进入 spawn / 保活 |
| 只杀自己的树 | 手动 Stop 或保活重启时，仅执行 `taskkill /PID <pid> /T`（必要时再加 `/F`） |
| 面板退出不杀子进程 | 即使面板被强制结束，业务进程仍持有独立控制台/进程组 |

> `DETACHED_PROCESS` 让子进程没有控制台，从根上避免面板被 Ctrl+C / 关闭控制台时连带子进程。
> 面板崩溃或被 `Stop-Process` / `taskkill /F` 强制结束，子进程也继续运行（已实测）。

---

## 1. 功能概览

- **探针看板**
  - 顶部统计：总数 / UP / DOWN / 异常熔断 / 平均响应
  - 卡片网格：绿 UP、红 DOWN、黄熔断、灰已停止/已禁用/待检查
  - DOWN 卡片高亮并置顶
  - 卡片操作：Start / Stop / Restart / 日志
  - 全部重启（按依赖正序启动、逆序停止）
  - 4 秒轮询 `/api/status`
  - 日志右侧抽屉（深色终端风，2 秒自动刷新，可关闭）
- **服务管理**
  - 表格：名称 / ID / 探针 / 启动 exe / 依赖 / 启用开关 / 操作
  - 新增 / 编辑弹窗：基本信息、启动配置、依赖多选、探针、保活
  - 删除二次确认，运行中必须显式选择「先停止」
  - 前端校验：ID 唯一、exe 绝对路径、依赖不成环、端口/退避合法性
  - 运行中的启动配置变更会提示「需 Restart 生效」
- **全局设置**
  - 探针间隔、失败阈值、停止宽限、日志上限、探测历史保留数
  - Webhook、监听地址、访问令牌、等依赖探针 UP、删除运行中服务
  - 保存后立即热加载（监听地址除外，需重启面板进程）
- **运维特性**
  - 命名 Mutex 单例
  - SQLite WAL + 事务化写
  - adopt / PID 复用校验
  - 按服务粒度 taskkill
  - 端口残留清理
  - 熔断（持久化）与可选 Webhook
  - 手动 Stop 会抑制保活，直到手动 Start / Restart

---

## 2. 目录结构

```text
SvcDeck/
├── main.go                 # 启动、单例、HTTP、优雅退出、embed 静态页
├── api.go                  # REST API + 输入校验
├── supervisor.go           # 拓扑序、adopt/spawn、探针循环、保活、熔断
├── probe.go                # HTTP / TCP / 进程探针
├── db.go                   # SQLite 建表/迁移、CRUD、运行态、设置、探测历史
├── models.go               # Service / RuntimeState / View / 启动签名
├── config.go               # 全局 Settings 与热加载 ConfigStore
├── process_windows.go      # Detached spawn、PID 校验、taskkill、端口检查（Windows）
├── singleton_windows.go    # 命名 Mutex 单例（Windows）
├── web/
│   └── index.html          # 单页前端（原生 JS + fetch，无构建工具）
├── go.mod / go.sum
└── README.md

运行时自动生成（默认与 SvcDeck.exe 同目录）：
├── panel.db                # SQLite：全部配置 / 运行态 / 设置 / 探测历史
├── panel.db-wal / -shm
└── logs/
    ├── <service-id>.log
    └── <service-id>.log.1  # 超 log_max_mb 后的轮转文件
```

> 前端 `web/index.html` 通过 `//go:embed` 打进二进制，部署时只需要一个 `SvcDeck.exe`。

---

## 3. 构建与运行

### 3.1 依赖

- Go 1.26+（`modernc.org/sqlite` 要求 1.26；本仓库开发验证于 Go 1.27 / Windows amd64）
- 仅以下第三方依赖：
  - `golang.org/x/sys/windows`：进程校验、命名 Mutex；
  - `modernc.org/sqlite`：纯 Go、免 CGO 的 SQLite 驱动。

### 3.2 编译

```powershell
go mod tidy
go build -trimpath -ldflags "-s -w" -o SvcDeck.exe .
```

### 3.3 运行

```powershell
# 默认：数据目录 = SvcDeck.exe 所在目录，监听 127.0.0.1:8088
.\SvcDeck.exe

# 推荐把数据放到固定目录
.\SvcDeck.exe -data "D:\SvcDeckData"

# 临时覆盖监听地址 / 令牌（不会写回数据库）
.\SvcDeck.exe -data "D:\SvcDeckData" -addr 127.0.0.1:18088 -token my-secret
```

| 参数 | 说明 |
| --- | --- |
| `-data` | 数据目录；`panel.db`、`logs/` 都放在这里。默认取 exe 所在目录。 |
| `-db` | 直接指定 SQLite 文件路径（优先于 `-data` 推导）。 |
| `-addr` | 覆盖数据库中的 `listen_addr`，仅本次进程有效。 |
| `-token` | 覆盖数据库中的 `token`，仅本次进程有效。 |

浏览器打开 `http://127.0.0.1:8088/`。

---

## 4. SQLite 数据模型

数据库文件默认 `panel.db`，开启 WAL，连接数固定为 1，`busy_timeout=5000`，多步写入使用事务。

### 4.1 `services`（服务配置）

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `id` | TEXT PK | 服务唯一 ID，同时用于日志文件名与 API 路径；`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$` |
| `name` | TEXT | 显示名称 |
| `group_name` | TEXT | 分组（API JSON 中字段名为 `group`） |
| `enabled` | INTEGER | 1 = 纳入自动启动与保活；0 = 只展示探针，不自动拉起 |
| `exe` | TEXT | 启动 exe 的绝对路径（保存时校验存在且不是目录） |
| `args` | TEXT(JSON) | 参数数组，如 `["-t","127.0.0.1"]` |
| `cwd` | TEXT | 工作目录；为空时默认取 exe 所在目录 |
| `env` | TEXT(JSON) | 环境变量对象，覆盖面板同名环境变量；空对象表示继承面板环境 |
| `depends_on` | TEXT(JSON) | 依赖的 service id 数组；用于拓扑序与环检测 |
| `probe_type` | TEXT | `http` / `tcp` / `process` |
| `probe_url` | TEXT | HTTP 探针 URL |
| `probe_host` | TEXT | TCP 探针主机 |
| `probe_port` | INTEGER | TCP 探针端口 |
| `probe_keyword` | TEXT | HTTP 响应体必须包含的关键词（可选，本实现额外字段） |
| `probe_expect` | INTEGER | 期望 HTTP 状态码；0 表示接受 2xx/3xx |
| `probe_timeout_sec` | INTEGER | 单次探针超时（默认 5） |
| `restart_max_retries` | INTEGER | 连续失败后最多保活重启次数；0 = 关闭自动保活 |
| `restart_backoff_sec` | TEXT(JSON) | 退避序列（秒），如 `[1,2,5,30]`，超出末尾按最后一档封顶 |
| `created_at` / `updated_at` | TEXT | RFC3339 时间戳 |

### 4.2 `runtime_state`（接管与保活的持久化状态）

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `service_id` | TEXT PK | 关联 `services.id` |
| `pid` | INTEGER | 面板上次拉起的 PID；0 表示未托管 |
| `process_start_time` | INTEGER | 进程创建时间（Windows FILETIME，100ns 单位）；防 PID 复用的关键 |
| `exe_path` | TEXT | 拉起时记录的镜像路径 |
| `last_up` | INTEGER | 最近一次探针结果 |
| `last_error` | TEXT | 最近错误 |
| `consecutive_fails` | INTEGER | 连续失败次数 |
| `retries` | INTEGER | 已进行的保活重启次数（探针 UP 后清零） |
| `updated_at` | TEXT | 更新时间 |
| `circuit_open` | INTEGER | 熔断标记（持久化，面板重启后不会自动狂重启） |
| `manual_stop` | INTEGER | 手动 Stop 标记；启动时跳过该服务，保活也被抑制 |
| `next_retry_at` | INTEGER | 下一次保活重启的 unix 秒 |
| `last_check` | INTEGER | 最近探测 unix 秒 |
| `start_signature` | TEXT | 最后一次 spawn 时的启动配置签名（exe/args/cwd/env 的 SHA-256） |

### 4.3 `settings`

`key TEXT PRIMARY KEY, value TEXT`，每个设置一行：

`probe_interval_sec`、`fail_threshold`、`webhook_url`、`listen_addr`、`token`、`wait_deps`、`stop_grace_sec`、`log_max_mb`、`allow_delete_running`、`probe_history_limit`。

### 4.4 `probe_history`（可选）

```sql
id INTEGER PRIMARY KEY AUTOINCREMENT,
service_id TEXT, ts INTEGER, up INTEGER, rt_ms INTEGER, err TEXT
```

每次探针写一条，按服务保留最近 `probe_history_limit` 条（0 = 关闭）。可通过 `GET /api/history/{id}?lines=100` 查看。

---

## 5. 启动、接管、探针与保活状态机

### 5.1 面板启动

1. 读取 `enabled = 1` 的服务；
2. 按 `depends_on` 做拓扑排序（失败则回退为配置顺序并写日志）；
3. 逐个 `ensureRunning`：
   - **先 adopt**：`pid` 存活 + 创建时间一致 + exe 路径一致 → 直接接管，不 spawn；
   - **再用探针确认**：探针 UP → 认为在跑，按唯一 exe 找回 pid（若有）并接管；
   - 以上都失败 → spawn；
4. 若开启 `wait_deps`，按依赖等待被依赖服务探针 UP（最长 20 秒）后再启动下一个；
5. 若服务是 `manual_stop` 或 `circuit_open`，启动流程跳过，避免面板重启后破坏用户意图；
6. 探针循环独立并发运行，不阻塞启动流程。

### 5.2 探针

- 全局间隔默认 **5 秒**（`probe_interval_sec`，可热改）；
- 每个服务独立超时（`probe_timeout_sec`，默认 5 秒）；
- 三类探针：
  - `http`：GET，校验状态码（0 = 2xx/3xx）及可选响应关键词；支持 `https`；
  - `tcp`：`net.DialTimeout` 连接 host:port；
  - `process`：先做与 adopt 相同的 pid/时间/exe 三重校验；无效时按 exe 查找（仅当该 exe 在面板内唯一）。
- 失败不会立刻判 DOWN：连续失败达到 `fail_threshold`（默认 3）才进入保活判断；
- 刚 spawn 后有一段启动宽限（`max(15s, timeout+5s)`），宽限内 DOWN 不累计、不触发重启，避免慢启动服务被误杀；
- **探针结果会写回 `runtime_state`**，面板崩溃后状态不丢。

### 5.3 保活与熔断

```text
探针 DOWN 连续 >= fail_threshold
        │
        ├─ restart_max_retries == 0 ── 只显示 DOWN，不自动重启
        │
        ├─ retries >= max ────────── 置 circuit_open=1，发 Webhook，停止狂重启
        │
        └─ 到达退避时间 ─────────── 停止旧进程树（如可确认）→ spawn
                                      └─ 失败/仍未 UP → retries++，按 backoff 序列等下一次
```

- 退避序列默认 `[1,2,5,30]`，超过末尾取最后一档；
- 探针 UP 后清零 `consecutive_fails / retries / circuit_open`；
- 熔断写入 SQLite：面板重启后不会立刻继续狂重启，需要手动 **Start / Restart** 复位；
- 熔断时会调用一次可选 Webhook：
  ```json
  {
    "event": "circuit_open",
    "service_id": "order-api",
    "service_name": "订单服务",
    "message": "连续 3 次探测失败，且保活重启已达上限 5 次",
    "time": "2026-11-15T04:30:00+08:00"
  }
  ```

### 5.4 端口残留清理

spawn 前，如果探针目标端口是本机端口且已被占用：

1. 先看 `runtime_state.pid`，通过 adopt 三重校验才允许 `taskkill /PID /T`；
2. 若该 exe 在面板内**唯一**，再按 exe 找回残留实例并清理；
3. 清理后等待端口释放；
4. 仍被**无法确认归属**的进程占用 → **拒绝 spawn** 并记错误，绝不误杀未知进程。

### 5.5 手动 Stop 与“抑制保活”

- `POST /api/stop/{id}` 会：
  1. 校验 pid 归属；
  2. 先 `taskkill /PID <pid> /T`（不带 `/F`）；
  3. 超过 `stop_grace_sec` 仍存活再 `/F`；
  4. 清空 `runtime_state.pid`，写 `manual_stop=1`。
- `manual_stop=1` 后，探针仍会继续展示，但**不会**被保活自动拉起；面板重启也不会自动启动它；
- 只有 `POST /api/start/{id}`、`POST /api/restart/{id}`、`POST /api/restart-all`（enabled 服务）会清除该标记；
- 例外：通过 `PUT /api/services/{id}` 把 `enabled` 从 `false` 改为 `true` 也是一次明确的“启用”操作，会清除 `manual_stop` / 熔断并异步拉起（新增 `enabled=true` 的服务同理）。

### 5.6 服务粒度 taskkill

只有两处会主动终止业务进程：

| 场景 | 命令 |
| --- | --- |
| 手动 Stop / 手动 Restart / 删除前 Stop / restart-all 停止阶段 | `taskkill /PID <pid> /T`，超时后 `/F` |
| 保活重启 | 同上，先停旧进程树，再 spawn |

命令只针对该服务自己的 `pid` 及其子树（`/T`），不涉及其他服务，也不涉及面板。

---

## 6. REST API

监听地址默认 `127.0.0.1:8088`。若配置了 `token`，除静态页外的所有 `/api/*` 请求都需要：

```http
X-Panel-Token: <token>
```

或：

```http
Authorization: Bearer <token>
```

未授权返回 `401 {"error":"访问令牌无效..."}`。

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/health` | 健康检查 |
| GET | `/api/status` | 探针看板：id/name/group/up/rt_ms/last_check/consecutive_fails/err/probe_desc/pid/state 等 |
| GET | `/api/services` | 配置 + 运行态列表 |
| GET | `/api/services/{id}` | 单个服务 |
| POST | `/api/services` | 新增；校验 ID 唯一、exe 存在、依赖存在且不成环 |
| PUT | `/api/services/{id}` | 编辑；保存后热加载；运行中启动配置变化时返回 `pending_restart` |
| DELETE | `/api/services/{id}` | 删除；被其他服务依赖时 409；托管进程运行中默认 409，`?stop=true` 先停再删；`allow_delete_running` 可改为自动先停；探针 UP 但 pid 为空（未托管）时只删除配置 |
| POST | `/api/start/{id}` | 手动启动；清除 `manual_stop` 与熔断 |
| POST | `/api/stop/{id}` | 手动停止；仅杀该服务树；抑制后续保活 |
| POST | `/api/restart/{id}` | 先停后起；清除 `manual_stop` 与熔断 |
| POST | `/api/restart-all` | enabled 服务按依赖逆序停止、正序启动 |
| GET | `/api/logs/{id}?lines=100` | 读取 `logs/<id>.log` 尾部，`lines` 上限 2000 |
| GET | `/api/history/{id}?lines=100` | 探测历史（`probe_history`） |
| GET | `/api/config` | 全局设置 |
| PUT | `/api/config` | 保存并热加载；`listen_addr` 变更返回 `restart_required` |

### 6.1 新增服务示例（PowerShell）

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

### 6.2 返回约定

- 成功：`{"ok":true,...}`，列表为 `{"items":[...]}`；
- 失败：`{"error":"..."}`，HTTP 400 / 401 / 404 / 409 / 500；
- 所有写操作都校验输入；exe 必须存在且不是目录；依赖必须存在且不能成环。

---

## 7. 前端

- 位置：`web/index.html`，原生 JS + CSS，无构建工具、无第三方库；
- 通过 `//go:embed` 打进二进制；
- 两个 Tab：
  - **探针看板**：4 秒轮询 `/api/status`，DOWN 置顶，日志抽屉；
  - **服务管理**：CRUD、启用开关、依赖多选、表单校验；
- 访问令牌：首次 401 自动弹窗；Token 存 `localStorage`，通过 `X-Panel-Token` 发送；
- 全局设置弹窗；
- 响应式布局；原生 `<dialog>` 提供模态语义、焦点管理与 Esc 关闭；
- 所有用户数据用 `textContent` / DOM 构建，不使用不可信字符串拼接 HTML。

---

## 8. 面板单例（命名 Mutex）

实现位于 `singleton_windows.go`，名称：

```text
Local\SvcDeck_Panel_Singleton_v1
```

流程：

```go
h, err := windows.CreateMutex(nil, false, namePtr)
if err != nil {
    if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
        // 已有一个面板在运行：关闭本次返回的 handle 并退出
    }
}
```

要点：

- `CreateMutex` 在已存在同名对象时**同时返回有效 handle 和 `ERROR_ALREADY_EXISTS`**，代码会先 `CloseHandle` 再退出，不会泄漏；
- Mutex 随进程退出（包括崩溃）由内核自动释放，**不会留下需要手工删除的陈旧锁文件**；
- 单例检查发生在打开 SQLite、监听端口之前，第二个进程直接退出并打印原因；
- 使用 `Local\` 命名空间（按登录会话）。跨会话（例如服务态 session 0 与桌面会话）由监听端口绑定失败兜底；
- 实测：第二个实例退出码 1，输出「另一个 SvcDeck 面板已在运行」。

---

## 9. 优雅退出（不杀子进程）

实现位于 `main.go`：

1. `signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)`；
2. 收到信号或 HTTP Serve 异常后：
   - 不再接受新 HTTP 请求，`srv.Shutdown(10s)`；
   - `Supervisor.Close`：取消探针上下文、关闭循环，最多等 10 秒；
   - `store.Close()`、释放命名 Mutex；
3. **退出路径中没有对子进程的任何 `taskkill` / `Process.Kill`**。

为什么子进程能活：

- `DETACHED_PROCESS` → 子进程没有控制台，不接收面板控制台的 Ctrl+C / 关闭事件；
- `CREATE_NEW_PROCESS_GROUP` → 不与面板共享 Ctrl+Break 目标；
- 面板不持有子进程的 Wait/Job 生命周期；
- spawn 后立即 `cmd.Process.Release()`，不 `Wait()`，不建立父子退出联动。

实测结果：

- `taskkill /PID <panel>`（不带 `/F`）：面板日志打印「收到退出信号，开始优雅关闭」→「子进程未被终止」，业务进程继续运行；
- `Stop-Process -Force` / `taskkill /F` 强制结束面板：业务进程仍存活；
- 重启面板后日志出现 `adopt 成功：pid ... 仍是 ...`，PID 保持不变。

### 9.1 用 WinSW 包装时的注意

WinSW 只是**面板自身**的开机自启容器，不应把业务服务也注册进去。

**必须确认外层包装器不会在停止时对整棵进程树执行 `taskkill /T` 或使用带 `KILL_ON_JOB_CLOSE` 的 Job Object**。面板内部不杀子进程，但如果外层工具主动按进程树清理，`DETACHED_PROCESS` 也不能阻止外层工具杀进程。  
用任务计划或只终止 Panel 进程本身的包装方式最稳。

---

## 10. adopt 的 PID 复用校验细节

代码在 `process_windows.go` 的 `verifyProcess`：

```text
want = runtime_state 中的 (pid, process_start_time, exe_path)

1. pid != 0，且 OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION) 成功；
2. GetExitCodeProcess == STILL_ACTIVE；
3. QueryFullProcessImageNameW 得到实际镜像路径；
   - 与 exe_path 做 filepath.Clean + strings.EqualFold 比较（Windows 路径大小写不敏感）；
4. GetProcessTimes 得到进程创建时间（FILETIME，100ns）；
   - 与 process_start_time 精确比较（0 表示未采集到，退化为只校验路径）；
```

只有 1–4 全部通过，才认为「这还是我上次拉起的那个进程」。

失败时：

- **绝不杀任何进程**；
- 清空 `runtime_state.pid/exe_path/process_start_time`；
- 继续做探针判定；
- 探针 UP：
  - 若该 exe 在面板内**唯一**，按 exe 枚举进程，取创建时间最新的一个，重新写入 pid/时间/exe；
  - 若 exe 被多个服务共享，不猜测归属，只标记 UP、pid 留空（HTTP/TCP 外部端点场景；Stop/Restart 会提示无法定位）；
  - 进程探针在 exe 被共享且没有 pid 时会直接判 DOWN，从而不会把别的服务实例误认为自己的；
- 探针 DOWN：才进入 spawn。

启动配置（exe / args / cwd / env）变化不会触发面板擅自重启；`runtime_state.start_signature` 与新配置比对，API/前端提示「需 Restart 生效」。

### 10.1 为什么需要创建时间，而不只是 exe 路径

Windows 会复用 PID。一个服务退出后，它的 PID 可能很快被系统分配给另一个进程；如果只看「pid 存活」，Stop 可能杀错人。  
`process_start_time` 是进程创建时即固定的 FILETIME，PID 复用后创建时间必然不同，能可靠地区分「同一个进程」与「同号新进程」。

### 10.2 同 exe 多实例的限制

如果两个服务配置了**同一个 exe**：

- 进程探针在没有各自 pid 时无法判断某个进程属于哪个服务；
- 因此不会按 exe 接管，也不会按 exe 清理/停止（避免误杀另一个实例）；
- 建议这类服务使用 `http` / `tcp` 探针，或确保面板已记录各自 pid；
- 已实测：两个同 exe 服务各自 spawn 后分别持有 PID，探活互不干扰。

---

## 11. 开机自启面板

**只让面板自启；业务服务仍然由面板拉起，绝不注册为 Windows 服务。**

### 方案 A：任务计划（推荐，无额外依赖）

以管理员 PowerShell 执行：

```powershell
# 用变量承载带空格的 /TR，避免 PowerShell 的引号转义问题
$tr = '"C:\SvcDeck\SvcDeck.exe" -data "D:\SvcDeckData"'
schtasks /Create /TN "SvcDeck Panel" /F /SC ONLOGON /RL HIGHEST /TR $tr
```

> 若需要“开机即启动、无需用户登录”，把 `/SC ONLOGON` 改为 `/SC ONSTARTUP`。

然后在「任务计划程序」中调整：

- 勾选「不管用户是否登录都要运行」时，需要保存账户密码；面板在 session 0 运行，浏览器访问本机 127.0.0.1 仍可用；
- 勾选「隐藏」减少控制台窗口；
- 取消「如果任务运行时间超过以下时间，停止任务」；
- 设置「如果任务失败，每 1 分钟重启，最多 3 次」；
- 不要给任务配置「停止所有子进程」类选项。

验证：

```powershell
schtasks /Run /TN "SvcDeck Panel"
Get-Process SvcDeck
```

### 方案 B：WinSW 注册「唯一」自启项

1. 下载 WinSW，将 `WinSW-x64.exe` 重命名为 `SvcDeckService.exe`；
2. 同目录放 `SvcDeckService.xml`：

```xml
<service>
  <id>SvcDeck</id>
  <name>SvcDeck Panel</name>
  <description>SvcDeck local service panel (business processes are NOT Windows services)</description>
  <executable>C:\SvcDeck\SvcDeck.exe</executable>
  <arguments>-data "D:\SvcDeckData"</arguments>
  <workingdirectory>C:\SvcDeck</workingdirectory>
  <startmode>Automatic</startmode>
  <stoptimeout>20sec</stoptimeout>
  <onfailure action="restart" delay="10 sec"/>
  <log mode="roll-by-size">
    <sizeThreshold>10240</sizeThreshold>
    <keepFiles>8</keepFiles>
  </log>
</service>
```

3. 安装 / 卸载：

```powershell
.\SvcDeckService.exe install
.\SvcDeckService.exe start
.\SvcDeckService.exe stop
.\SvcDeckService.exe uninstall
```

**重要**：确认 WinSW 没有开启 `killProcessTree` 之类的“停止时杀整棵进程树”行为。若它用 Job Object 或 `taskkill /T` 清理，业务进程会被外层容器一起杀死，这与面板自身“退出不杀子进程”是两回事。只让 WinSW 终止 Panel 进程本身；业务进程由面板的 adopt/探针逻辑管理。

---

## 12. 安全建议

- 默认只监听 `127.0.0.1`；不要直接暴露公网。
- 需要跨机访问时，设置 `token`，并在前面套 HTTPS 反向代理（本项目不带 TLS）。
- Token 存在本机 SQLite，浏览器存在 localStorage；数据库目录建议限制 ACL（只允许运行面板的账户）。
- 前端发送 `X-Panel-Token`，服务端用常量时间比较；CSP / `X-Frame-Options` / `nosniff` 已开启。
- Webhook URL 可指向飞书/钉钉/企微机器人；请求内容只包含服务 id/名称/错误摘要。
- 面板本身不需要管理员权限即可管理当前用户自己的进程；如果业务进程以更高权限运行，`taskkill` 可能失败，需以相应权限运行面板。

---

## 13. 已知边界

1. **仅 Windows**：`process_windows.go` / `singleton_windows.go` 使用 Windows API；Linux/macOS 不会编译通过。
2. **同 exe 多实例**：进程探针在 pid 缺失时无法区分归属，见 §10.2。
3. **禁用服务**：仍参与探针展示，但不自动启动、不保活；状态显示为「已禁用」。禁用**不会**停止已经在运行的进程，需要 Stop 才会终止。
4. **手动停止**：会一直抑制保活，直到手动 Start/Restart（这是有意设计，避免“按了 Stop 又被拉起”）。
5. **监听地址**：修改 `/api/config` 的 `listen_addr` 后需要重启面板进程才生效。
6. **日志轮转**：在 spawn 时按 `log_max_mb` 检查并轮转，不对正在写入的旧文件做在线切割。
7. **删除服务**：被其他服务依赖时拒绝删除；`allow_delete_running` 只影响“运行中删除”的默认行为。
8. **面板自身单例按会话**：跨会话由端口占用兜底；不要把同一数据目录交给两个面板同时写。
9. **进程启动宽限**：默认至少 15 秒，慢启动服务建议调大 `probe_timeout_sec`，宽限会随之增大。

---

## 14. 冒烟验收步骤（手工）

1. `go build -o SvcDeck.exe .`，启动 `.\SvcDeck.exe -data .tmp-test`。
2. 新增一个服务：exe 用 `C:\Windows\System32\PING.EXE`，参数 `-t 127.0.0.1`，探针 `process`，启用。
3. 看板出现 UP，记录 PID。
4. **不经过面板**，直接 `taskkill /PID <panel>`（不带 `/F`）：
   - 面板日志显示优雅关闭；
   - `Get-Process ping` 仍存在。
5. 重新启动面板：
   - 日志出现 `adopt 成功：pid ... 仍是 ...`；
   - 看板 PID 与步骤 3 一致，没有新起一个 ping。
6. 手工 `Stop-Process` 掉 ping：
   - 连续失败达到阈值后，面板按退避重启并产生新 PID；
   - 看板状态恢复 UP。
7. 手动 `Stop` 服务：
   - ping 被 taskkill（只该进程树）；
   - 等待超过探针周期，ping 仍不会被保活拉起；
   - 手动 Start 后恢复。
8. 删除服务（勾选「先停止」）：
   - 进程与配置都被清理；
   - `logs/<id>.log` 保留（审计用）。

以上 4/5/7 已在开发环境实测通过。

---

## 15. 常见问题

**Q：为什么面板退出后服务还活着？**  
A：这是设计要求。面板不是生命周期绑定者，子进程 `DETACHED_PROCESS`，下次启动通过 `adopt` 接管。

**Q：如何确认接管的是旧进程而不是新起的？**  
A：日志里的 `adopt 成功：pid N 仍是 <exe>`；`runtime_state.process_start_time` 与进程创建时间精确匹配。若 PID 被复用，日志会打印 `adopt 失败...疑似 PID 复用`，随后按探针结果处理。

**Q：DOWN 很久但面板不重启？**  
A：检查 `restart_max_retries` 是否为 0；检查是否熔断（黄色）；检查是否 `manual_stop`（已停止）；检查启动宽限是否还没过。

**Q：启动失败，日志说端口被占用？**  
A：找占用端口且不是本服务可确认进程时，面板拒绝 spawn 以避免误杀。先手工处理占用者，或修正探针端口。

**Q：Token 忘了 / 页面一直弹令牌？**  
A：停掉面板，直接修改 SQLite `settings` 表中 `token` 的值为空；或用 `SvcDeck.exe -token newtoken` 临时覆盖启动，进入设置页把 token 改成空并保存。

**Q：能否管理需要管理员权限的服务？**  
A：可以配置，但面板自身需以足够权限运行，否则 `OpenProcess` / `taskkill` 会失败。

**Q：数据库能备份吗？**  
A：停面板后复制 `panel.db`（以及 `-wal` / `-shm`）即可；运行中请使用 SQLite 在线备份，不要直接复制。
