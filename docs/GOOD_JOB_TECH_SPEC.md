# Good Job Technical Specification

## Executive Summary

Good Job is a multithreaded, Postgres-based Active Job backend for Ruby on Rails that provides enterprise-grade background job processing without requiring additional infrastructure like Redis. It leverages PostgreSQL's advanced features to deliver a reliable, scalable, and feature-rich job queue system.

## Key Features

### Core Capabilities
- **Native PostgreSQL Integration**: Uses PostgreSQL as the sole backend, eliminating Redis dependency
- **Multithreaded Execution**: Built on Concurrent::Ruby for efficient thread pool management
- **Web Dashboard**: Full-featured monitoring UI with job inspection, retry capabilities, and performance metrics
- **Cron Jobs**: Built-in support for recurring scheduled jobs
- **Job Batches**: Group related jobs with callbacks for completion tracking
- **Concurrency Controls**: Advanced limiting, throttling, and rate limiting capabilities
- **Multiple Execution Modes**: Inline, async (in-process), and external (separate process) execution

### Advanced Features
- **LISTEN/NOTIFY**: Real-time job notifications via PostgreSQL pub/sub
- **Advisory Locks**: Prevents duplicate job execution without row-level locking
- **Bulk Operations**: Efficient batch job enqueuing
- **Job Preservation**: Optional retention of completed jobs for auditing
- **Process Monitoring**: Built-in health checks and process registration
- **Graceful Shutdown**: Ensures jobs complete before process termination

## System Architecture

### High-Level Architecture

```
┌─────────────────┐     ┌──────────────────┐     ┌────────────────┐
│   Rails App     │────▶│  ActiveJob API   │────▶│ GoodJob::      │
│                 │     │                  │     │   Adapter      │
└─────────────────┘     └──────────────────┘     └────────┬───────┘
                                                           │
                                ┌──────────────────────────┴────────────┐
                                │                                       │
                        ┌───────▼────────┐                     ┌───────▼────────┐
                        │  Inline Mode   │                     │ Async/External │
                        │                │                     │     Mode       │
                        └───────┬────────┘                     └───────┬────────┘
                                │                                       │
                                │                              ┌────────▼────────┐
                                │                              │    Capsule      │
                                │                              │  (Container)    │
                                │                              └────────┬────────┘
                                │                                       │
                                │         ┌─────────────────────────────┼─────────────────┐
                                │         │                             │                 │
                                │ ┌───────▼────────┐          ┌────────▼────────┐ ┌──────▼───────┐
                                │ │   Scheduler    │          │    Notifier     │ │ CronManager  │
                                │ │ (Thread Pool)  │◀─────────│  (LISTEN/NOTIFY)│ │              │
                                │ └───────┬────────┘          └─────────────────┘ └──────────────┘
                                │         │
                                │ ┌───────▼────────┐
                                │ │ JobPerformer   │
                                │ │                │
                                │ └───────┬────────┘
                                │         │
                                └─────────┼─────────────────────────────┐
                                          │                             │
                                  ┌───────▼────────┐           ┌────────▼────────┐
                                  │  PostgreSQL    │           │ Advisory Locks  │
                                  │   Database     │◀──────────│                 │
                                  └────────────────┘           └─────────────────┘
```

### Component Interaction Flow

1. **Job Submission**: Rails app submits jobs via ActiveJob API
2. **Adapter Routing**: GoodJob::Adapter determines execution mode
3. **Storage**: Jobs persisted to PostgreSQL with metadata
4. **Notification**: LISTEN/NOTIFY alerts waiting workers
5. **Scheduling**: Scheduler assigns jobs to thread pool
6. **Execution**: JobPerformer executes with advisory lock
7. **Completion**: Results recorded, optional preservation

## Core Data Structure Design

### Database Schema

#### Primary Tables

**`good_jobs`** - Main job storage
```sql
CREATE TABLE good_jobs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    queue_name TEXT,
    priority INTEGER,
    serialized_params JSONB NOT NULL,
    scheduled_at TIMESTAMP,
    performed_at TIMESTAMP,
    finished_at TIMESTAMP,
    error TEXT,
    active_job_id UUID NOT NULL,
    concurrency_key TEXT,
    cron_key TEXT,
    batch_id UUID,
    batch_callback_id UUID,
    labels TEXT[],
    locked_by_id UUID,
    locked_at TIMESTAMP,
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL
);
```

**`good_job_executions`** - Execution history
```sql
CREATE TABLE good_job_executions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    active_job_id UUID NOT NULL,
    process_id UUID,
    duration INTERVAL,
    error TEXT,
    error_event INTEGER,
    error_backtrace TEXT[],
    serialized_params JSONB NOT NULL,
    created_at TIMESTAMP NOT NULL
);
```

**`good_job_batches`** - Batch management
```sql
CREATE TABLE good_job_batches (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    serialized_properties JSONB,
    on_finish TEXT,
    on_success TEXT,
    on_discard TEXT,
    callback_queue_name TEXT,
    callback_priority INTEGER,
    jobs_finished_at TIMESTAMP,
    finished_at TIMESTAMP,
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL
);
```

### Index Strategy

- **Performance indexes**: Composite on queue_name, scheduled_at, priority
- **Conditional indexes**: Filter unfinished jobs to reduce index size
- **GIN indexes**: For array operations on labels
- **Unique constraints**: Prevent duplicate cron jobs

### Data Model Relationships

```
┌──────────────┐        ┌──────────────────┐
│ good_job_    │───────▶│   good_jobs      │
│ batches      │ 1:N    │                  │
└──────────────┘        └─────────┬────────┘
                                  │ 1:N
                        ┌─────────▼────────┐
                        │ good_job_        │
                        │ executions       │
                        └──────────────────┘
```

## Key Data Flow

### Job Lifecycle Flow

```
1. Enqueue
   ├─▶ Serialize job parameters
   ├─▶ Create database record
   ├─▶ Set queue, priority, schedule
   └─▶ Trigger notification

2. Dequeue
   ├─▶ Query eligible jobs
   ├─▶ Apply queue filters
   ├─▶ Check schedule time
   └─▶ Acquire advisory lock

3. Execute
   ├─▶ Create execution record
   ├─▶ Load job class
   ├─▶ Execute perform method
   └─▶ Handle errors/retries

4. Complete
   ├─▶ Update job status
   ├─▶ Record execution time
   ├─▶ Trigger batch callbacks
   └─▶ Release advisory lock
```

### Concurrency Control Flow

```
Thread requests job ──▶ Query available jobs
                          │
                          ▼
                    Select job candidate
                          │
                          ▼
                    Attempt advisory lock
                          │
         ┌────────────────┴────────────────┐
         │                                 │
         ▼                                 ▼
    Lock acquired                    Lock failed
         │                                 │
         ▼                                 ▼
    Execute job                      Try next job
         │
         ▼
    Release lock
```

## Core Components

### Execution Layer

**GoodJob::Adapter**
- ActiveJob integration point
- Execution mode routing
- Bulk enqueue optimization

**GoodJob::Scheduler**
- Thread pool management
- Work distribution
- Scheduled job caching
- Graceful shutdown

**GoodJob::JobPerformer**
- Database query optimization
- Queue filtering logic
- Advisory lock coordination
- Performance metrics

### Infrastructure Layer

**GoodJob::Notifier**
- PostgreSQL LISTEN/NOTIFY
- Cross-process communication
- Connection resilience
- Keepalive management

**GoodJob::Capsule**
- Resource container
- Lifecycle management
- Component coordination
- Configuration routing

### Data Layer

**GoodJob::Job**
- Core job model
- State management
- Execution logic
- Error handling

**GoodJob::Execution**
- Attempt tracking
- Performance metrics
- Error recording
- History preservation

### Feature Layer

**GoodJob::CronManager**
- Cron expression parsing
- Schedule management
- Job enqueueing
- Timezone handling

**GoodJob::Batch**
- Job grouping
- Callback management
- Progress tracking
- Completion detection

## Design Principles

### 1. **Simplicity First**
- Single database dependency
- Standard Rails patterns
- Minimal configuration
- Convention over configuration

### 2. **PostgreSQL-Native**
- Leverage database strengths
- Advisory locks for concurrency
- LISTEN/NOTIFY for communication
- JSONB for flexibility

### 3. **Reliability by Design**
- Transactional guarantees
- At-least-once execution
- Graceful error handling
- Process monitoring

### 4. **Performance Optimization**
- Efficient indexing strategy
- Thread pool management
- Connection pooling
- Smart caching

### 5. **Observability**
- Comprehensive logging
- Execution history
- Performance metrics
- Web dashboard

### 6. **Flexibility**
- Multiple execution modes
- Configurable preservation
- Extensible callbacks
- Plugin architecture

### 7. **Rails Integration**
- ActiveJob compatibility
- Rails engine architecture
- Respects Rails conventions
- Database migration support

## Trade-offs and Decisions

### Architectural Trade-offs

| Decision | Benefit | Trade-off |
|----------|---------|-----------|
| PostgreSQL-only | Simplicity, no Redis | Limited to PostgreSQL users |
| Advisory locks | Performance, simplicity | PostgreSQL-specific feature |
| Single table design | Query simplicity | Potential table size growth |
| Thread pools | Resource efficiency | Configuration complexity |
| LISTEN/NOTIFY | Low latency | Extra connection overhead |

### Design Decisions

1. **Database-centric**: Chose simplicity over distributed complexity
2. **Thread pools over processes**: Better resource utilization in Ruby
3. **Advisory locks over pessimistic locks**: Better performance, automatic cleanup
4. **Execution history tracking**: Debugging capability over storage efficiency
5. **Flexible preservation**: User choice on data retention

## Performance Characteristics

- **Latency**: Sub-second in async mode
- **Throughput**: Thousands of jobs/second per process
- **Scalability**: Horizontal via multiple processes
- **Memory**: Configurable thread pool limits
- **Database load**: Optimized queries with proper indexes

## Conclusion

Good Job represents a sophisticated yet pragmatic approach to background job processing in Rails. By fully embracing PostgreSQL's capabilities and following Rails conventions, it provides a production-ready solution that balances simplicity, reliability, and performance. The architecture demonstrates how leveraging database-native features can eliminate infrastructure complexity while delivering enterprise-grade functionality.