# 最小化 Go 版 Good Job 任务队列实现计划

> 目标：用 **Go + PostgreSQL** 重现 Good Job 的核心能力（持久化队列、多并发执行、LISTEN/NOTIFY、Advisory Lock 防重），并保持接口简单、可直接嵌入任意 Go 服务。

## 0. 范围与非目标

|          | 说明                                                                                                  |
| -------- | --------------------------------------------------------------------------------------------------- |
| **必须实现** | 持久化 Job 表、`Enqueue`/`Fetch` API、基于 Advisory Lock 的互斥执行、LISTEN/NOTIFY 推送、基础失败重试、优先级 & 延迟执行、优雅停机与中断恢复 |
| **暂不实现** | 批处理（Batches）、Cron、DashBoard、标签搜索、并发限流、执行历史保留、分布式 Tracing                                            |

## 1. 总体架构

```
┌──────────┐    Enqueue      ┌────────────┐
│ App Code │ ───────────────▶│  Queue API │──┐
└──────────┘                 └────────────┘  │
                                             │
                             LISTEN/NOTIFY   │
                                             ▼
                          ┌───────────────────────┐  Advisory Lock
                          │     Worker Pool       │◀───────────────┐
                          │  (goroutines + DB)    │                │
                          └───────────────────────┘                │
                                             │                     │
                    graceful shutdown ◀──────┘                     │
                                             │  UPDATE finished_at │
                                             ▼                     │
                                    ┌──────────────────┐           │
                                    │   good_jobs tbl   │──────────┘
                                    └──────────────────┘
```

* **单表设计**：仿照 Good Job `good_jobs` 单表存储，一切状态都在行级字段完成。
* **互斥执行**：使用 `pg_try_advisory_lock(lock_key)` 抢锁，类似 Good Job 的 Ruby 实现。
* **实时唤醒**：`NOTIFY good_job, '<queue>'` + 独立连接 `LISTEN good_job`。

## 2. 数据库模式（初版）

```sql
CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE good_jobs (
  id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  queue            TEXT NOT NULL DEFAULT 'default',
  priority         INTEGER NOT NULL DEFAULT 0,
  run_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
  payload          JSONB NOT NULL,
  error            TEXT,
  attempts         INTEGER NOT NULL DEFAULT 0,
  max_attempts     INTEGER NOT NULL DEFAULT 5,
  finished_at      TIMESTAMPTZ,
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 仅待执行 job 索引
CREATE INDEX idx_good_jobs_pending
  ON good_jobs (queue, priority DESC, run_at)
  WHERE finished_at IS NULL;
```

> 🔑 **锁键计算**：`(id)::bigint` 已足够（UUID ↓64 bit），或模仿 Good Job MD5→bit64 的方式。

## 3. Go 代码结构

```
/cmd
  worker/          # goodjob-worker 二进制
  migrate/         # 数据库迁移工具
/internal
  db/              # 连接池 wrapper（pgx）
  notifier/        # LISTEN/NOTIFY 封装
  locker/          # Advisory lock helper
  queue/           # Enqueue / Fetch / Retry
  worker/          # WorkerPool, graceful shutdown
  models/          # Job struct, scan helpers
  config/          # 读取 env / flags
```

### 关键包说明

| 包             | 责任                                                                                           |
| ------------- | -------------------------------------------------------------------------------------------- |
| `queue`       | `Enqueue(ctx, payload, opts)`、`Fetch(ctx, queues []string)`（含锁）                              |
| `notifier`    | 后台 goroutine 监听 `LISTEN good_job`，收到消息后立即唤醒等待中的 worker                                       |
| `worker`      | `Start(ctx, concurrency, queues)`：开 N 个 goroutine；对 `Fetch` 失败做指数退避；支持 `SIGINT/SIGTERM` 优雅停机 |
| `locker`      | `TryLock(id int64)`/`Unlock(id int64)` —— PG advisory lock 封装                                |
| `cmd/worker`  | CLI：`goodjob-worker --concurrency 5 --queues "high,default"`                                 |
| `cmd/migrate` | CLI：创建/升级表；支持 `--rollback`                                                                   |

> ✅ **依赖**：`github.com/jackc/pgx/v5`, `github.com/cenkalti/backoff/v4`, `github.com/alecthomas/kingpin`（CLI），标准库 `context`, `sync`, `time`.

## 4. 核心流程

1. **Enqueue**

   ```go
   func Enqueue(ctx, payload, queue, priority, runAt, maxAttempts)
   INSERT INTO good_jobs … RETURNING id;
   NOTIFY good_job, queue;
   ```

2. **Worker Fetch**

   ```sql
   WITH job AS (
     SELECT id
     FROM good_jobs
     WHERE queue = ANY($queues)
       AND run_at <= now()
       AND finished_at IS NULL
     ORDER BY priority DESC, created_at ASC
     LIMIT 1
   )
   SELECT id, payload, max_attempts, attempts
   FROM job
   WHERE pg_try_advisory_lock(('x'||substr(md5(id::text),1,16))::bit(64)::bigint);
   ```

3. **Execute & Retry**

   * 成功：`UPDATE SET finished_at = now()` + `pg_advisory_unlock`
   * 失败：`attempts++`, 计算下一次 `run_at = now() + retryDelay`, `pg_advisory_unlock`

## 5. 退出与容错

| 场景   | 策略                                       |
| ---- | ---------------------------------------- |
| 进程崩溃 | Advisory Lock 自动释放，其他 worker 可接管         |
| 网络抖动 | `notifier` 自动重连 + backoff；`Fetch` 在事务中检查 |
| 优雅关停 | 捕获信号，停止接新 job，等待在途 goroutine 完成或超时       |

## 6. 测试计划

* **单元测试**：`queue`、`locker` 包 —— 使用 `dockertest` 旋转 PG 容器
* **集成测试**：模拟并发 100 worker 抢同一队列，验证“不重复执行”
* **回归测试**：损坏连接、杀进程、磁盘满等 chaos case

## 7. 里程碑 & 工期估算

| 周期     | 任务                                         |
| ------ | ------------------------------------------ |
| Week 1 | 项目骨架 & migrate CLI；完成 `queue.Enqueue`      |
| Week 2 | `Fetch` + `locker` + 单表 schema；完成最小 worker |
| Week 3 | LISTEN/NOTIFY 集成；优雅停机；重试策略                 |
| Week 4 | 基准压测、文档、Dockerfile、CI（GitHub Actions）      |
| Week 5 | 首个 Tag `v0.1.0`，在实际服务中灰度                   |

## 8. 未来可拓展

* Cron 表达式调度（解析库：`github.com/robfig/cron/v3`）
* Web Dashboard（可用 Go HTMX + PG 视图）
* 批处理/回调、限流器、JSON Schema 校验
* 支持多数据库（MySQL 版本需替换 Advisory Lock）
* 插件式中间件（Tracing、指标、DLQ）

---

完成以上计划，即可得到一个 **可生产可维护** 的 Go 版 Good Job 核心任务队列，实现 Ruby 社区成熟模式在 Go 生态的落地。
