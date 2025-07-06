# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Repository Overview

This is a single-file Go implementation of a PostgreSQL-based job queue system inspired by Ruby's Good Job gem. The entire implementation is contained in `main.go`.

## Key Commands

### Build and Run
```bash
# Build the binary
go build -o claude-jobs main.go

# Run directly without building
go run main.go [options]

# Install dependencies
go mod download
```

### Database Setup
```bash
# Set PostgreSQL connection
export DATABASE_URL="postgres://user:password@localhost/mydb"

# Run migrations
go run main.go migrate
```

### Worker Operations
```bash
# Start worker with default settings
go run main.go worker

# Start worker with custom configuration
go run main.go worker --concurrency=10 --queues=high,default,low
```

### Job Enqueuing
```bash
# Enqueue Claude CLI job (default handler)
go run main.go enqueue --payload='{"prompt":"explain this code","cwd":"."}'

# Enqueue with priority and delay
go run main.go enqueue --queue=high --priority=10 --delay=5m --payload='{"prompt":"analyze security"}'
```

### Testing
```bash
# Run all tests (when tests are added)
go test ./...

# Run with verbose output
go test -v ./...

# Run with race detection
go test -race ./...
```

## Architecture

### Single-File Design
The entire job queue system is implemented in `main.go` containing:
- Database schema definition (embedded SQL)
- Job struct and queue logic
- Worker pool implementation
- CLI interface
- Default Claude CLI handler

### Core Components
1. **Job** - Represents a queued job with payload, status, and metadata
2. **Worker** - Manages concurrent job processing with connection pooling
3. **Config** - Worker configuration (concurrency, queues, poll interval)
4. **JobHandler** - Function type for processing specific job types
5. **EnqueueOptions** - Options for job scheduling (queue, priority, delay)

### PostgreSQL Features Used
- Advisory locks for preventing duplicate job execution
- LISTEN/NOTIFY for real-time job notifications
- JSONB for flexible job payloads
- UUID generation for job IDs
- Indexes for performance optimization

### Default Handler Behavior
The default job handler executes Claude CLI:
```bash
claude --dangerously-skip-permissions -p "<prompt>"
```
With optional working directory specified in payload.

## Development Guidelines

### Adding Features
- All code changes go in `main.go`
- Follow existing patterns for database operations
- Use pgx/v5 features directly (no ORM)
- Maintain single-file simplicity

### Database Schema
The `claude_jobs` table uses:
- UUID primary key
- JSONB payload storage
- Priority-based queue processing
- Exponential backoff for retries
- Advisory locks via job ID

### Error Handling
- Jobs retry with exponential backoff
- Max attempts configurable per job
- Failed jobs keep error details
- Advisory locks auto-release on disconnect

### Performance Considerations
- Connection pooling via pgx
- Real-time notifications reduce polling
- Composite index on (queue, priority DESC, run_at)
- Graceful shutdown completes active jobs