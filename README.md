实际用时：6h

# 问题
## - 为什么选这个语言；
go的并发跟ts、python不是一个层级的，python有全局解释锁，虽然可以使用async进行异步操作，但是只要代码或者引用包中有个同步就会直接影响全局。ts是单线程循环，可以用worker模拟，但是失踪受主线程制约。不过按题目来说，这么小的并发量用什么语言无所谓，不过考虑扩展，还是决定使用go
## - 你们打算怎么保证"并发测试是真实的并发"
1. 可以使用 go test -race 检测竞态条件，如果看到 WARNING: DATA RACE，关注它给出的两个访问同一内存地址的 goroutine 调用栈，这就是并发冲突。
2. Go 的基准测试默认是单线程串行执行的。即使代码里写了 go f()，testing.B 也不会自动并发运行。若需精确控制并发数，需手动使用 sync.WaitGroup + go 启动，且必须在 b.ResetTimer() 后开始计时，避免初始化开销被计入耗时


# 任务调度看板

一个 Go + PostgreSQL 的小型任务调度系统。Go 适合把 Web 服务、worker 和真实多进程测试做成独立可执行程序；PostgreSQL 原生行锁足以覆盖题目中不超过 10 个 worker、每秒不超过 5 个任务的规模，无需额外分布式锁。

## 启动

需要 Go 1.26+ 和 Docker。以下命令会启动数据库、创建演示任务，再分别启动服务和 worker：

```bash
make db-up
make seed
make server             # 终端 1：http://localhost:8080
make worker             # 终端 2：每个 Step 模拟执行 15 秒
```

也可用 `make up` 在 Docker 中启动数据库和 Web 服务。页面无需登录，打开后直接进入看板；支持创建、取消、每秒轮询、业务事件日志，以及并发认领和完成上报演示。

## 架构

- `cmd/server` 提供原生 HTML 看板和 JSON API；`cmd/worker` 是可启动多个实例的模拟 worker；`internal/store` 集中管理所有事务和状态转换。
- 认领在一个事务中执行 `SELECT ... FOR UPDATE SKIP LOCKED`，随后写入 `claimed_by` 再提交。同一行锁在事务提交前不会交给第二个连接，适用于同机多进程和跨机器 PostgreSQL 客户端。
- 认领时快照并合并 L1 base 与 L2 group；每步启动时从当前有效参数应用 L3。非空 L3 override 会粘性写回，空字符串忽略并沿用当前值；L2 空字符串按字面保留。
- `step_logs` 的 `(task_id, step_position)` 唯一约束保证只有一行。upsert 使用布尔 OR：失败可升级为成功，成功永不降级；任务行锁保证同一步骤只推进一次。
- 每次领取都会递增 `ownership_version` 作为 fencing token，并设置数据库时钟控制的租约。Worker 执行期间续租；失联租约过期后新 Worker 使用首次启动时的 L2/L3 快照接管当前 Step，旧 token 的续租、启动和完成写入都会被拒绝。每个 Step 还会产生稳定的 `task:<id>:step:<position>` 幂等键，真实消息渠道必须把它传给下游做副作用去重。
- 取消会在单个事务中将 Task 和所有未完成 Step 置为 `cancelled`、清除租约并记录业务事件；旧 Worker 后续写入会被 fencing token 拒绝。
- 看板每页读取 25 个任务，统计值由数据库单独聚合；生产默认清理 30 天前的终态任务，并只保留最近 10 次并发认领演示。

## 生产部署

准备域名并让 80/443 端口可达，然后基于 `.env.production.example` 创建 `.env.production`。数据库密码和完整连接串通过只读 Docker secrets 文件提供：密码使用 URL-safe 字符，`database_url.txt` 写入使用同一密码的完整连接串。`secrets/` 已从构建上下文排除，文件权限应设为 `0600`。

```bash
make prod-check
make prod-up       # PostgreSQL + Web + 2 Worker + Caddy HTTPS
```

生产 Compose 不暴露数据库端口，应用使用只读非 root 容器、真实健康检查、自动重启、资源/日志上限和 Caddy HTTPS。可用 `--scale worker=N` 调整 Worker 数量；迁移由 PostgreSQL advisory lock 串行化。`backup` 服务每天向 `backup-data` 卷写一份自校验格式的 `pg_dump`，默认保留 14 天；生产还应把该卷复制到异机或对象存储，并定期做恢复演练。

恢复会覆盖当前库，应先停止 `app` 和 `worker`，再将备份通过 `pg_restore --clean --if-exists` 导入。备份文件可在 `backup` 容器的 `/backups` 中查看。

## 测试

```bash
make test                 # 参数演变等纯单元测试
make test-integration     # API、真实 PostgreSQL 幂等与状态机测试
make test-concurrency     # 10 个 OS 进程争抢 250 个任务
```

多进程测试为每次运行创建隔离数据库，每个子进程建立自己的连接，在同一时刻开始认领，并断言每个任务恰有一个 owner；样例输出见 `evidence/claim-race.txt`。集成测试还覆盖失联接管、旧 token 拒绝、运行中取消、L2/L3 边界和重复上报。模拟 Worker 只延时并成功上报，不发送真实消息；接入真实消息渠道时应把 `task_id + step_position` 作为下游幂等键。
