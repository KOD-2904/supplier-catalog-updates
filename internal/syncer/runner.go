package syncer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"supplier-catalog-updates/internal/model"
	"supplier-catalog-updates/internal/store"
	"supplier-catalog-updates/internal/supplier"
)

// RunnerConfig holds tuning parameters for the catalog sync runner.
type RunnerConfig struct {
	Retries   int
	Backoff   time.Duration
	BatchSize int
	Verbose   bool
}

// DefaultRunnerConfig provides standard production defaults.
func DefaultRunnerConfig() RunnerConfig {
	return RunnerConfig{
		Retries:   3,
		Backoff:   200 * time.Millisecond,
		BatchSize: 20,
		Verbose:   false,
	}
}

// Runner orchestrates the end-to-end synchronization lifecycle.
type Runner struct {
	store    *store.Store
	supplier supplier.Client
	cfg      RunnerConfig
}

// NewRunner creates a new syncer Runner.
func NewRunner(st *store.Store, sup supplier.Client, cfg RunnerConfig) *Runner {
	if cfg.Retries < 0 {
		cfg.Retries = 0
	}
	if cfg.Backoff <= 0 {
		cfg.Backoff = 200 * time.Millisecond
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 20
	}
	return &Runner{
		store:    st,
		supplier: sup,
		cfg:      cfg,
	}
}

func (r *Runner) logf(format string, v ...any) {
	if r.cfg.Verbose {
		log.Printf("[runner] "+format, v...)
	}
}

// Start begins a brand new update run.
func (r *Runner) Start(ctx context.Context) (*model.UpdateRun, error) {
	// 1. Mark any previously crashed 'running' run as interrupted
	if err := r.store.InterruptStaleRunningRuns(ctx, "interrupted by new run start"); err != nil {
		return nil, fmt.Errorf("failed to clean up stale runs: %w", err)
	}

	now := time.Now()
	run := &model.UpdateRun{
		ID:        fmt.Sprintf("run-%s-%s", now.Format("20060102-150405"), uuid.NewString()[:8]),
		Status:    model.RunStatusRunning,
		StartedAt: now,
		UpdatedAt: now,
	}

	if err := r.store.CreateRun(ctx, run); err != nil {
		return nil, fmt.Errorf("failed to create run in store: %w", err)
	}

	return r.execute(ctx, run)
}

// Resume continues an existing interrupted run. If runID is empty, it finds the latest interrupted run.
func (r *Runner) Resume(ctx context.Context, runID string) (*model.UpdateRun, error) {
	var run *model.UpdateRun
	var err error

	if runID != "" {
		run, err = r.store.GetRun(ctx, runID)
	} else {
		run, err = r.store.GetLatestInterruptedRun(ctx)
		if err != nil && errors.Is(err, store.ErrNotFound) {
			run, err = r.store.GetLatestRun(ctx)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("no run found to resume: %w", err)
	}

	if run.Status == model.RunStatusCompleted || run.Status == model.RunStatusCompletedWithProblems {
		return nil, fmt.Errorf("run %s has already completed (%s)", run.ID, run.Status)
	}
	if run.Status == model.RunStatusFailed {
		return nil, fmt.Errorf("run %s failed with permanent error and cannot be resumed", run.ID)
	}

	// Mark other dead running runs as interrupted
	if err := r.store.InterruptStaleRunningRuns(ctx, "interrupted before resume"); err != nil {
		return nil, fmt.Errorf("failed to clean up stale runs: %w", err)
	}

	if err := r.store.SetRunStatus(ctx, run.ID, model.RunStatusRunning, ""); err != nil {
		return nil, fmt.Errorf("failed to mark run %s as running: %w", run.ID, err)
	}
	run.Status = model.RunStatusRunning

	r.logf("resuming run %s at cursor %q (batches_done: %d)", run.ID, run.NextCursor, run.BatchesDone)
	return r.execute(ctx, run)
}

// execute runs the fetch-merge-commit loop until feed completion or fatal interruption.
func (r *Runner) execute(ctx context.Context, run *model.UpdateRun) (*model.UpdateRun, error) {
	cursor := run.NextCursor

	for {
		select {
		case <-ctx.Done():
			r.markInterrupted(run.ID, "interrupted by context cancellation / signal")
			return run, ctx.Err()
		default:
		}

		// 1. Fetch batch with retry & exponential backoff for transient errors
		resp, err := r.fetchWithRetry(ctx, cursor)
		if err != nil {
			if errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
				r.markInterrupted(run.ID, "interrupted by context cancellation / signal")
				return run, ctx.Err()
			}
			if supplier.IsTransient(err) {
				// Retry exhausted on transient error -> interrupted (can resume later)
				errMsg := fmt.Sprintf("supplier error after retries: %v", err)
				r.markInterrupted(run.ID, errMsg)
				run.Status = model.RunStatusInterrupted
				run.LastError = errMsg
				return run, fmt.Errorf("run interrupted: %w", err)
			}

			// Permanent error -> failed (cannot resume)
			errMsg := fmt.Sprintf("permanent error: %v", err)
			r.markFailed(run.ID, errMsg)
			run.Status = model.RunStatusFailed
			run.LastError = errMsg
			return run, fmt.Errorf("run failed permanently: %w", err)
		}

		// 2. Safeguard against stalled cursor (infinite loop)
		if !resp.Done && resp.NextCursor == cursor {
			errMsg := fmt.Sprintf("cursor stalled at %q while done=false (possible infinite loop)", cursor)
			r.markFailed(run.ID, errMsg)
			run.Status = model.RunStatusFailed
			run.LastError = errMsg
			return run, errors.New(errMsg)
		}

		// 3. Process items in the batch
		batchNo := run.BatchesDone + 1
		now := time.Now()
		commitData := &model.BatchCommitData{
			RunID:      run.ID,
			BatchNo:    batchNo,
			NextCursor: resp.NextCursor,
			IsDone:     resp.Done,
		}

		// Cache products touched within the current batch to handle intra-batch updates
		intraBatchProducts := make(map[string]*model.Product)

		for _, item := range resp.Items {
			if valErr := Validate(item); valErr != nil {
				rawBytes, _ := json.Marshal(item)
				supID := item.SupplierID
				if supID == "" {
					supID = "UNKNOWN"
				}
				commitData.Problems = append(commitData.Problems, model.Problem{
					ID:         uuid.NewString(),
					RunID:      run.ID,
					BatchNo:    batchNo,
					SupplierID: supID,
					Kind:       model.ProblemKindInvalidItem,
					Message:    valErr.Error(),
					RawJSON:    string(rawBytes),
					Resolved:   false,
					CreatedAt:  now,
				})
				commitData.Delta.Problem++
				continue
			}

			// Find current product in intraBatchProducts cache, then database
			var cur *model.Product
			if cached, ok := intraBatchProducts[item.SupplierID]; ok {
				cur = cached
			} else {
				dbProd, pErr := r.store.GetProduct(ctx, item.SupplierID)
				if pErr == nil {
					cur = dbProd
				}
			}

			dec := Merge(cur, item, run.ID, now)

			if dec.Product != nil {
				intraBatchProducts[item.SupplierID] = dec.Product
				upsertSliceProduct(&commitData.ProductsToStore, dec.Product)
			}
			if dec.Change != nil {
				commitData.Changes = append(commitData.Changes, *dec.Change)
			}
			if dec.Problem != nil {
				dec.Problem.BatchNo = batchNo
				commitData.Problems = append(commitData.Problems, *dec.Problem)
			}

			commitData.Delta.Created += dec.Delta.Created
			commitData.Delta.Updated += dec.Delta.Updated
			commitData.Delta.Withdrawn += dec.Delta.Withdrawn
			commitData.Delta.Unchanged += dec.Delta.Unchanged
			commitData.Delta.Stale += dec.Delta.Stale
			commitData.Delta.Problem += dec.Delta.Problem
		}

		// 4. Determine completion status if done = true
		if resp.Done {
			totalProblems := run.ProblemCount + commitData.Delta.Problem
			if totalProblems > 0 {
				commitData.FinalStatus = model.RunStatusCompletedWithProblems
			} else {
				commitData.FinalStatus = model.RunStatusCompleted
			}
			commitData.FinishedAt = &now
		}

		// 5. Commit batch atomically to SQLite
		if err := r.store.CommitBatch(ctx, commitData); err != nil {
			r.markInterrupted(run.ID, fmt.Sprintf("batch commit failed: %v", err))
			return run, fmt.Errorf("failed to commit batch %d: %w", batchNo, err)
		}

		// 6. Update in-memory run state
		run.BatchesDone++
		run.CreatedCount += commitData.Delta.Created
		run.UpdatedCount += commitData.Delta.Updated
		run.WithdrawnCount += commitData.Delta.Withdrawn
		run.UnchangedCount += commitData.Delta.Unchanged
		run.StaleCount += commitData.Delta.Stale
		run.ProblemCount += commitData.Delta.Problem
		run.NextCursor = resp.NextCursor
		run.UpdatedAt = now

		r.logf("batch %d committed: items=%d, changes=%d, problems=%d, cursor=%q",
			batchNo, len(resp.Items), len(commitData.Changes), len(commitData.Problems), run.NextCursor)

		if resp.Done {
			run.Status = commitData.FinalStatus
			run.FinishedAt = commitData.FinishedAt
			r.logf("run %s finished successfully with status %s", run.ID, run.Status)
			return run, nil
		}

		cursor = resp.NextCursor
	}
}

// fetchWithRetry performs calls to supplier with exponential backoff on transient errors.
func (r *Runner) fetchWithRetry(ctx context.Context, cursor string) (*model.BatchResponse, error) {
	backoff := r.cfg.Backoff
	for attempt := 0; ; attempt++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		resp, err := r.supplier.FetchBatch(ctx, cursor, r.cfg.BatchSize)
		if err == nil {
			return resp, nil
		}

		if !supplier.IsTransient(err) {
			// Permanent error, fail immediately without retry
			return nil, err
		}

		if attempt >= r.cfg.Retries {
			return nil, fmt.Errorf("transient error after %d retries: %w", attempt, err)
		}

		r.logf("fetch attempt %d failed: %v. Retrying in %v...", attempt+1, err, backoff)

		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}

		backoff *= 2
	}
}

// markInterrupted uses a fresh detached context so it persists even if caller's ctx was canceled.
func (r *Runner) markInterrupted(runID, reason string) {
	detachedCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = r.store.SetRunStatus(detachedCtx, runID, model.RunStatusInterrupted, reason)
}

// markFailed uses a fresh detached context to record non-resumable failure.
func (r *Runner) markFailed(runID, reason string) {
	detachedCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = r.store.SetRunStatus(detachedCtx, runID, model.RunStatusFailed, reason)
}

// upsertSliceProduct updates or appends product in slice.
func upsertSliceProduct(list *[]*model.Product, p *model.Product) {
	for i, existing := range *list {
		if existing.SupplierID == p.SupplierID {
			(*list)[i] = p
			return
		}
	}
	*list = append(*list, p)
}
