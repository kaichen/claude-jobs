# Claude Jobs - Minimal PostgreSQL Task Queue

A single-file Go implementation of a PostgreSQL-based task queue system, inspired by [Ruby's Good Job gem](https://github.com/bensheldon/good_job). The entire implementation is contained in `main.go`.

**Default Behavior**: The default job handler executes Claude CLI with the provided prompt and working directory:
```bash
claude --dangerously-skip-permissions -p --verbose <prompt>
```

## Features

- **PostgreSQL-based persistence** - All job data stored in a single `claude_jobs` table
- **Advisory locks** - Prevents duplicate job execution across workers
- **LISTEN/NOTIFY** - Real-time job notifications for immediate processing
- **Concurrent workers** - Configurable worker pool with graceful shutdown
- **Automatic retries** - Exponential backoff for failed jobs
- **Priority queues** - Jobs processed by priority and creation time
- **Delayed execution** - Schedule jobs to run in the future

## Prerequisites

- Go 1.24.4+
- PostgreSQL 12+
- pgx/v5 driver: `go get github.com/jackc/pgx/v5`

## Quick Start

### 1. Build the binary (optional)

```bash
# Build the binary
go build -o claude-jobs main.go

# Or run directly with go run
```

### 2. Set up the database

```bash
# Set your PostgreSQL connection string
export DATABASE_URL="postgres://user:password@localhost/mydb"

# Run migrations to create the claude_jobs table
go run main.go migrate
```

### 3. Start a worker

```bash
# Start a worker with 5 concurrent goroutines
go run main.go worker --concurrency 5 --queues high,default
```

### 4. Enqueue jobs

```bash
# Default job handler executes Claude CLI
go run main.go enqueue --payload '{"prompt":"write hello world in go","cwd":"/tmp"}'

# Execute claude in current directory
go run main.go enqueue --payload '{"prompt":"explain this codebase"}'

# With priority and delay
go run main.go enqueue --queue high --priority 10 --delay 5m \
  --payload '{"prompt":"analyze security vulnerabilities","cwd":"/path/to/project"}'

# Custom job type (requires registering handler)
go run main.go enqueue --payload '{"type":"email","to":"user@example.com"}'

# Explicitly use claude handler  
go run main.go enqueue --payload '{"type":"claude","prompt":"analyze this file","cwd":"."}'
```

## Usage as a Library

You can also integrate the queue directly into your Go application:

```go
package main

import (
    "context"
    "encoding/json"
    "log"
)

func main() {
    // Enqueue a Claude CLI job (default handler)
    payload := map[string]interface{}{
        "prompt": "write a hello world program in go",
        "cwd": "/tmp",
    }
    
    err := Enqueue(context.Background(), databaseURL, payload, &EnqueueOptions{
        Queue:    "default",
        Priority: 5,
    })
    if err != nil {
        log.Fatal(err)
    }
    
    // Start a worker with custom handlers
    config := &Config{
        DatabaseURL:  databaseURL,
        Concurrency:  10,
        Queues:       []string{"emails", "default"},
        PollInterval: 5 * time.Second,
    }
    
    worker, err := NewWorker(config)
    if err != nil {
        log.Fatal(err)
    }
    
    // Register job handlers
    worker.RegisterHandler("email", func(ctx context.Context, payload json.RawMessage) error {
        var email EmailJob
        if err := json.Unmarshal(payload, &email); err != nil {
            return err
        }
        return sendEmail(email)
    })
    
    // Start processing
    if err := worker.Start(); err != nil {
        log.Fatal(err)
    }
}
```

## Command Line Usage

```
Usage: <binary> <command> [options]
```

### Commands

#### migrate - Create/update database schema
```bash
go run main.go migrate --database-url "postgresql://localhost/myapp"
```

#### worker - Start processing jobs  
```bash
go run main.go worker [options]
  --database-url      PostgreSQL connection string (or DATABASE_URL env var)
  --concurrency       Number of concurrent workers (default: 5)
  --queues           Comma-separated queue names (default: "default")
```

#### enqueue - Add a new job
```bash
go run main.go enqueue [options]
  --database-url      PostgreSQL connection string (or DATABASE_URL env var)
  --queue            Queue name (default: "default")
  --priority         Job priority, higher processed first (default: 0)
  --payload          JSON payload for the job (default: "{}")
  --delay            Delay before job runs (e.g., "5m", "1h")
  --max-attempts     Maximum retry attempts (default: 5)
```

## Architecture

The implementation follows these key principles:

1. **Single Table Design**: All job data in one `claude_jobs` table
2. **Advisory Locks**: PostgreSQL advisory locks prevent duplicate execution
3. **LISTEN/NOTIFY**: Real-time notifications when new jobs are enqueued
4. **Graceful Shutdown**: Workers complete current jobs before stopping
5. **Automatic Cleanup**: Completed jobs marked with `finished_at`

## Job Lifecycle

1. Job enqueued with `INSERT` and `NOTIFY`
2. Worker fetches job with `SELECT ... WHERE pg_try_advisory_lock()`
3. Job executed by handler function
4. Success: Job marked with `finished_at` timestamp
5. Failure: Job rescheduled with exponential backoff until `max_attempts`
6. Advisory lock released in all cases

## Monitoring

Monitor job queue health with SQL queries:

```sql
-- Pending jobs by queue
SELECT queue, COUNT(*) as pending_count
FROM claude_jobs
WHERE finished_at IS NULL AND run_at <= now()
GROUP BY queue;

-- Failed jobs
SELECT id, queue, error, attempts, max_attempts
FROM claude_jobs
WHERE finished_at IS NOT NULL AND error IS NOT NULL;

-- Job processing rate
SELECT 
  date_trunc('minute', finished_at) as minute,
  COUNT(*) as jobs_completed
FROM claude_jobs
WHERE finished_at > now() - interval '1 hour'
GROUP BY minute
ORDER BY minute;
```

## Performance Considerations

- Workers use connection pooling via pgx/v5
- LISTEN/NOTIFY reduces polling overhead
- Index on `(queue, priority DESC, run_at)` for fast job fetching
- Advisory locks are automatically released on connection loss
- Exponential backoff prevents retry storms

## Limitations

This minimal implementation does not include:
- Web dashboard (use SQL queries for monitoring)
- Batch jobs
- Cron/scheduled recurring jobs
- Job dependencies
- Dead letter queues
- Distributed tracing

For production use, consider adding:
- Structured logging
- Metrics collection (Prometheus)
- Health check endpoints
- Configuration via environment variables
- Docker deployment

## License

MIT