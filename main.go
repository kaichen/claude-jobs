package main

import (
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/big"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
)

var logger = slog.Default()

// SQL schema for the claude_jobs table
const createTableSQL = `
CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE IF NOT EXISTS claude_jobs (
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

-- Index for efficient pending job queries
CREATE INDEX IF NOT EXISTS idx_claude_jobs_pending
    ON claude_jobs (queue, priority DESC, run_at)
    WHERE finished_at IS NULL;
`

// Job represents a queued job
type Job struct {
	ID          string          `db:"id"`
	Queue       string          `db:"queue"`
	Priority    int             `db:"priority"`
	RunAt       time.Time       `db:"run_at"`
	Payload     json.RawMessage `db:"payload"`
	Error       *string         `db:"error"`
	Attempts    int             `db:"attempts"`
	MaxAttempts int             `db:"max_attempts"`
	FinishedAt  *time.Time      `db:"finished_at"`
	CreatedAt   time.Time       `db:"created_at"`
}

// Config holds worker configuration
type Config struct {
	DatabaseURL  string
	Concurrency  int
	Queues       []string
	PollInterval time.Duration
}

// Worker manages the job processing
type Worker struct {
	config   *Config
	pool     *pgxpool.Pool
	wg       sync.WaitGroup
	ctx      context.Context
	cancel   context.CancelFunc
	notifyCh chan string
	handlers map[string]JobHandler
}

// JobHandler is a function that processes a job
type JobHandler func(context.Context, *Job) error

// EnqueueOptions for job enqueuing
type EnqueueOptions struct {
	Queue       string
	Priority    int
	RunAt       time.Time
	MaxAttempts int
}

// NewWorker creates a new worker instance
func NewWorker(config *Config) (*Worker, error) {
	poolConfig, err := pgxpool.ParseConfig(config.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse database URL: %w", err)
	}

	pool, err := pgxpool.NewWithConfig(context.Background(), poolConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create connection pool: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &Worker{
		config:   config,
		pool:     pool,
		ctx:      ctx,
		cancel:   cancel,
		notifyCh: make(chan string, 100),
		handlers: make(map[string]JobHandler),
	}, nil
}

// RegisterHandler registers a job handler for a specific job type
func (w *Worker) RegisterHandler(jobType string, handler JobHandler) {
	w.handlers[jobType] = handler
}

// DefaultHandler executes claude CLI with the provided prompt and working directory
func DefaultHandler(ctx context.Context, job *Job) error {
	var params struct {
		Prompt string `json:"prompt"`
		Cwd    string `json:"cwd"`
	}

	if err := json.Unmarshal(job.Payload, &params); err != nil {
		return fmt.Errorf("failed to parse payload: %w", err)
	}

	if params.Prompt == "" {
		return fmt.Errorf("prompt is required")
	}

	// Use current directory if cwd not specified
	if params.Cwd == "" {
		var err error
		params.Cwd, err = os.Getwd()
		if err != nil {
			return fmt.Errorf("failed to get working directory: %w", err)
		}
	} else {
		// Validate cwd path exists
		info, err := os.Stat(params.Cwd)
		if err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("working directory does not exist: %s", params.Cwd)
			}
			return fmt.Errorf("failed to check working directory: %w", err)
		}
		if !info.IsDir() {
			return fmt.Errorf("cwd is not a directory: %s", params.Cwd)
		}
	}

	// Create log directory in user's home directory
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}
	logDir := filepath.Join(homeDir, ".local", "shared", "claude-jobs", "logs")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return fmt.Errorf("failed to create log directory: %w", err)
	}

	// Create log file paths using job ID and timestamp
	timestamp := time.Now().Format("20060102-150405")
	stdoutPath := filepath.Join(logDir, fmt.Sprintf("%s-%s.stdout.log", job.ID, timestamp))
	stderrPath := filepath.Join(logDir, fmt.Sprintf("%s-%s.stderr.log", job.ID, timestamp))

	// Create stdout log file
	stdoutFile, err := os.Create(stdoutPath)
	if err != nil {
		return fmt.Errorf("failed to create stdout log file: %w", err)
	}
	defer stdoutFile.Close()

	// Create stderr log file
	stderrFile, err := os.Create(stderrPath)
	if err != nil {
		return fmt.Errorf("failed to create stderr log file: %w", err)
	}
	defer stderrFile.Close()

	logger.Info("Executing claude",
		"directory", params.Cwd,
		"prompt", params.Prompt,
		"jobID", job.ID,
		"stdoutLog", stdoutPath,
		"stderrLog", stderrPath)

	cmd := exec.CommandContext(ctx, "claude", "--dangerously-skip-permissions", "--verbose", "-p", params.Prompt)
	cmd.Dir = params.Cwd

	// Redirect output to both console and log files
	cmd.Stdout = io.MultiWriter(os.Stdout, stdoutFile)
	cmd.Stderr = io.MultiWriter(os.Stderr, stderrFile)

	// Execute the command
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("claude execution failed: %w", err)
	}

	logger.Info("Job output logged", "jobID", job.ID, "stdout", stdoutPath, "stderr", stderrPath)
	return nil
}

// Enqueue adds a new job to the queue
func Enqueue(ctx context.Context, databaseURL string, payload interface{}, opts *EnqueueOptions) error {
	if opts == nil {
		opts = &EnqueueOptions{
			Queue:       "default",
			Priority:    0,
			RunAt:       time.Now(),
			MaxAttempts: 1,
		}
	}

	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}

	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("failed to connect to database: %w", err)
	}
	defer conn.Close(ctx)

	query := `
		INSERT INTO claude_jobs (queue, priority, run_at, payload, max_attempts)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id
	`

	var jobID string
	err = conn.QueryRow(ctx, query, opts.Queue, opts.Priority, opts.RunAt, payloadJSON, opts.MaxAttempts).Scan(&jobID)
	if err != nil {
		return fmt.Errorf("failed to insert job: %w", err)
	}

	// Notify listeners about the new job
	_, err = conn.Exec(ctx, fmt.Sprintf("NOTIFY good_job, '%s'", opts.Queue))
	if err != nil {
		logger.Error("Failed to send notification", "error", err)
	}

	logger.Info("Enqueued job", "jobID", jobID, "queue", opts.Queue)
	return nil
}

// calculateLockKey generates an advisory lock key from job ID
func calculateLockKey(jobID string) int64 {
	h := md5.Sum([]byte(jobID))
	hexStr := hex.EncodeToString(h[:8])

	bigInt := new(big.Int)
	bigInt.SetString(hexStr, 16)

	return bigInt.Int64()
}

// fetch attempts to fetch and lock a job
func (w *Worker) fetch(ctx context.Context) (*Job, error) {
	query := `
		WITH candidate AS (
			SELECT id, queue, priority, run_at, payload, error, attempts, max_attempts, finished_at, created_at
			FROM claude_jobs
			WHERE queue = ANY($1)
				AND run_at <= now()
				AND finished_at IS NULL
			ORDER BY priority DESC, created_at ASC
			LIMIT 1
		)
		SELECT id, queue, priority, run_at, payload, error, attempts, max_attempts, finished_at, created_at
		FROM candidate
		WHERE pg_try_advisory_lock($2::bigint)
	`

	// Try each job until we get a lock
	for i := 0; i < 10; i++ {
		var job Job
		rows, err := w.pool.Query(ctx, query, w.config.Queues, 0)
		if err != nil {
			return nil, fmt.Errorf("failed to query jobs: %w", err)
		}
		defer rows.Close()

		// Find first available job
		for rows.Next() {
			err = rows.Scan(&job.ID, &job.Queue, &job.Priority, &job.RunAt,
				&job.Payload, &job.Error, &job.Attempts, &job.MaxAttempts,
				&job.FinishedAt, &job.CreatedAt)
			if err != nil {
				continue
			}

			lockKey := calculateLockKey(job.ID)

			// Try to acquire advisory lock
			var locked bool
			err = w.pool.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", lockKey).Scan(&locked)
			if err == nil && locked {
				return &job, nil
			}
		}

		// No jobs found, return nil
		if err := rows.Err(); err != nil {
			return nil, err
		}

		// Small delay before retry
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}

	return nil, nil
}

// execute processes a job
func (w *Worker) execute(ctx context.Context, job *Job) error {
	logger.Info("Executing job",
		"jobID", job.ID,
		"queue", job.Queue,
		"attempt", job.Attempts+1,
		"maxAttempts", job.MaxAttempts)

	// Parse payload to determine job type
	var payload map[string]interface{}
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		// If we can't parse payload, use default handler
		payload = make(map[string]interface{})
	}

	// Get handler based on job type
	jobType, _ := payload["type"].(string)
	handler, exists := w.handlers[jobType]
	if !exists {
		handler = DefaultHandler
	}

	// Execute the job
	startTime := time.Now()
	err := handler(ctx, job)
	duration := time.Since(startTime)

	if err != nil {
		return w.handleJobError(ctx, job, err)
	}

	// Mark job as completed
	return w.completeJob(ctx, job, duration)
}

// handleJobError handles job execution errors
func (w *Worker) handleJobError(ctx context.Context, job *Job, jobErr error) error {
	job.Attempts++
	errStr := jobErr.Error()

	if job.Attempts >= job.MaxAttempts {
		// Max attempts reached, mark as finished with error
		query := `
			UPDATE claude_jobs
			SET attempts = $1, error = $2, finished_at = now()
			WHERE id = $3
		`
		_, err := w.pool.Exec(ctx, query, job.Attempts, errStr, job.ID)
		if err != nil {
			return fmt.Errorf("failed to update failed job: %w", err)
		}

		logger.Error("Job failed after max attempts",
			"jobID", job.ID,
			"attempts", job.Attempts,
			"error", jobErr)
	} else {
		// Schedule retry with exponential backoff
		retryDelay := time.Duration(math.Pow(2, float64(job.Attempts-1))) * time.Second
		if retryDelay > 5*time.Minute {
			retryDelay = 5 * time.Minute
		}

		nextRunAt := time.Now().Add(retryDelay)

		query := `
			UPDATE claude_jobs
			SET attempts = $1, error = $2, run_at = $3
			WHERE id = $4
		`
		_, err := w.pool.Exec(ctx, query, job.Attempts, errStr, nextRunAt, job.ID)
		if err != nil {
			return fmt.Errorf("failed to update job for retry: %w", err)
		}

		logger.Info("Job scheduled for retry",
			"jobID", job.ID,
			"nextRunAt", nextRunAt,
			"attempt", job.Attempts,
			"maxAttempts", job.MaxAttempts)
	}

	// Release advisory lock
	lockKey := calculateLockKey(job.ID)
	_, err := w.pool.Exec(ctx, "SELECT pg_advisory_unlock($1)", lockKey)
	if err != nil {
		logger.Error("Failed to release lock", "jobID", job.ID, "error", err)
	}

	return nil
}

// completeJob marks a job as successfully completed
func (w *Worker) completeJob(ctx context.Context, job *Job, duration time.Duration) error {
	query := `
		UPDATE claude_jobs
		SET finished_at = now(), error = NULL
		WHERE id = $1
	`

	_, err := w.pool.Exec(ctx, query, job.ID)
	if err != nil {
		return fmt.Errorf("failed to mark job as completed: %w", err)
	}

	// Release advisory lock
	lockKey := calculateLockKey(job.ID)
	_, err = w.pool.Exec(ctx, "SELECT pg_advisory_unlock($1)", lockKey)
	if err != nil {
		logger.Error("Failed to release lock", "jobID", job.ID, "error", err)
	}

	logger.Info("Job completed successfully", "jobID", job.ID, "duration", duration)
	return nil
}

// listenForNotifications listens for NOTIFY events
func (w *Worker) listenForNotifications() {
	conn, err := w.pool.Acquire(w.ctx)
	if err != nil {
		logger.Error("Failed to acquire connection for LISTEN", "error", err)
		return
	}
	defer conn.Release()

	_, err = conn.Exec(w.ctx, "LISTEN good_job")
	if err != nil {
		logger.Error("Failed to LISTEN", "error", err)
		return
	}

	logger.Info("Listening for job notifications")

	for {
		notification, err := conn.Conn().WaitForNotification(w.ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			logger.Error("Error waiting for notification", "error", err)
			time.Sleep(5 * time.Second)
			continue
		}

		select {
		case w.notifyCh <- notification.Payload:
		case <-w.ctx.Done():
			return
		default:
			// Channel full, drop notification
		}
	}
}

// processJobs is the main job processing loop for a worker goroutine
func (w *Worker) processJobs(workerID int) {
	defer w.wg.Done()

	logger.Info("Worker started", "workerID", workerID)

	for {
		select {
		case <-w.ctx.Done():
			logger.Info("Worker stopping", "workerID", workerID)
			return
		default:
		}

		// Try to fetch a job
		job, err := w.fetch(w.ctx)
		if err != nil {
			logger.Error("Error fetching job", "workerID", workerID, "error", err)
			time.Sleep(time.Second)
			continue
		}

		if job == nil {
			// No jobs available, wait for notification or timeout
			select {
			case <-w.ctx.Done():
				return
			case <-w.notifyCh:
				// New job notification received, try again immediately
			case <-time.After(w.config.PollInterval):
				// Periodic check
			}
			continue
		}

		// Process the job
		if err := w.execute(w.ctx, job); err != nil {
			logger.Error("Error executing job", "workerID", workerID, "jobID", job.ID, "error", err)
		}
	}
}

// Start begins processing jobs
func (w *Worker) Start() error {
	logger.Info("Starting worker",
		"concurrency", w.config.Concurrency,
		"queues", w.config.Queues)

	// Start notification listener
	go w.listenForNotifications()

	// Start worker goroutines
	for i := 0; i < w.config.Concurrency; i++ {
		w.wg.Add(1)
		go w.processJobs(i)
	}

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		logger.Info("Received signal, shutting down gracefully", "signal", sig)
	case <-w.ctx.Done():
		logger.Info("Context cancelled, shutting down")
	}

	// Cancel context to stop all workers
	w.cancel()

	// Wait for all workers to finish with timeout
	done := make(chan struct{})
	go func() {
		w.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		logger.Info("All workers stopped gracefully")
	case <-time.After(30 * time.Second):
		logger.Warn("Shutdown timeout exceeded")
	}

	// Close database pool
	w.pool.Close()

	return nil
}

// migrate creates the database schema
func migrate(databaseURL string) error {
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return fmt.Errorf("failed to connect to database: %w", err)
	}
	defer db.Close()

	_, err = db.Exec(createTableSQL)
	if err != nil {
		return fmt.Errorf("failed to create table: %w", err)
	}

	logger.Info("Database migration completed successfully")
	return nil
}

func printUsage() {
	fmt.Println("GoodJob - PostgreSQL-based job queue for Go")
	fmt.Println("")
	fmt.Println("Usage:")
	fmt.Println("  goodjob <command> [options]")
	fmt.Println("")
	fmt.Println("Commands:")
	fmt.Println("  migrate              Create or update the database schema")
	fmt.Println("  enqueue              Add a new job to the queue")
	fmt.Println("  worker               Start processing jobs")
	fmt.Println("")
	fmt.Println("Global Options:")
	fmt.Println("  --database-url       PostgreSQL connection string (or DATABASE_URL env var)")
	fmt.Println("")
	fmt.Println("Examples:")
	fmt.Println("  goodjob migrate --database-url postgresql://localhost/myapp")
	fmt.Println("  goodjob enqueue --payload '{\"prompt\":\"write hello world in go\",\"cwd\":\"/tmp\"}'")
	fmt.Println("  goodjob worker --concurrency 10 --queues high,default")
	fmt.Println("")
	fmt.Println("Default job handler executes: claude --dangerously-skip-permissions <prompt>")
	fmt.Println("Required payload fields: prompt (string), optional: cwd (string)")
}

func runMigrate(args []string) {
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	databaseURL := fs.String("database-url", os.Getenv("DATABASE_URL"), "PostgreSQL connection string")

	fs.Parse(args)

	if *databaseURL == "" {
		logger.Error("Database URL is required (use --database-url or DATABASE_URL env var)")
		os.Exit(1)
	}

	if err := migrate(*databaseURL); err != nil {
		logger.Error("Migration failed", "error", err)
		os.Exit(1)
	}
}

func runEnqueue(args []string) {
	fs := flag.NewFlagSet("enqueue", flag.ExitOnError)
	databaseURL := fs.String("database-url", os.Getenv("DATABASE_URL"), "PostgreSQL connection string")
	queue := fs.String("queue", "default", "Queue name for enqueuing")
	priority := fs.Int("priority", 0, "Job priority (higher is processed first)")
	payload := fs.String("payload", "{}", "JSON payload for the job")
	delay := fs.Duration("delay", 0, "Delay before job should run")
	maxAttempts := fs.Int("max-attempts", 5, "Maximum retry attempts")

	fs.Parse(args)

	if *databaseURL == "" {
		logger.Error("Database URL is required (use --database-url or DATABASE_URL env var)")
		os.Exit(1)
	}

	var payloadData map[string]interface{}
	if err := json.Unmarshal([]byte(*payload), &payloadData); err != nil {
		logger.Error("Invalid JSON payload", "error", err)
		os.Exit(1)
	}

	// Validate prompt field
	prompt, ok := payloadData["prompt"].(string)
	if !ok || strings.TrimSpace(prompt) == "" {
		logger.Error("Payload must contain a non-empty 'prompt' field")
		os.Exit(1)
	}

	// Validate cwd field if provided
	if cwdInterface, exists := payloadData["cwd"]; exists {
		cwd, ok := cwdInterface.(string)
		if !ok {
			logger.Error("The 'cwd' field must be a string")
			os.Exit(1)
		}

		if strings.TrimSpace(cwd) == "" {
			logger.Error("The 'cwd' field cannot be blank")
			os.Exit(1)
		}

		// Check if cwd path exists
		info, err := os.Stat(cwd)
		if err != nil {
			if os.IsNotExist(err) {
				logger.Error("The specified working directory does not exist", "cwd", cwd)
				os.Exit(1)
			}
			logger.Error("Error checking working directory", "error", err)
			os.Exit(1)
		}

		if !info.IsDir() {
			logger.Error("The specified working directory is not a directory", "cwd", cwd)
			os.Exit(1)
		}
	}

	opts := &EnqueueOptions{
		Queue:       *queue,
		Priority:    *priority,
		RunAt:       time.Now().Add(*delay),
		MaxAttempts: *maxAttempts,
	}

	if err := Enqueue(context.Background(), *databaseURL, payloadData, opts); err != nil {
		logger.Error("Enqueue failed", "error", err)
		os.Exit(1)
	}
}

func runWorker(args []string) {
	fs := flag.NewFlagSet("worker", flag.ExitOnError)
	databaseURL := fs.String("database-url", os.Getenv("DATABASE_URL"), "PostgreSQL connection string")
	concurrency := fs.Int("concurrency", 5, "Number of concurrent workers")
	queues := fs.String("queues", "default", "Comma-separated list of queues to process")

	fs.Parse(args)

	if *databaseURL == "" {
		logger.Error("Database URL is required (use --database-url or DATABASE_URL env var)")
		os.Exit(1)
	}

	config := &Config{
		DatabaseURL:  *databaseURL,
		Concurrency:  *concurrency,
		Queues:       strings.Split(*queues, ","),
		PollInterval: 5 * time.Second,
	}

	worker, err := NewWorker(config)
	if err != nil {
		logger.Error("Failed to create worker", "error", err)
		os.Exit(1)
	}

	// Register claude handler for jobs with type "claude"
	worker.RegisterHandler("claude", DefaultHandler)

	if err := worker.Start(); err != nil {
		logger.Error("Worker failed to start", "error", err)
		os.Exit(1)
	}
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	command := os.Args[1]
	args := os.Args[2:]

	switch command {
	case "migrate":
		runMigrate(args)
	case "enqueue":
		runEnqueue(args)
	case "worker":
		runWorker(args)
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Printf("Unknown command: %s\n\n", command)
		printUsage()
		os.Exit(1)
	}
}
