package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
	"supplier-catalog-updates/internal/model"
)

var (
	ErrNotFound = errors.New("record not found")
)

type Store struct {
	db *sql.DB
}

// Open initializes SQLite connection, applies necessary Pragmas, and migrates schema.
func Open(dbPath string) (*Store, error) {
	// Enable modernc sqlite connection
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite database: %w", err)
	}

	// MaxOpenConns = 1 is essential for single-process SQLite WAL mode without write-lock contention
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	if err := s.initPragmasAndSchema(); err != nil {
		_ = db.Close()
		return nil, err
	}

	return s, nil
}

func (s *Store) initPragmasAndSchema() error {
	pragmas := []string{
		"PRAGMA journal_mode = WAL;",
		"PRAGMA foreign_keys = ON;",
		"PRAGMA synchronous = FULL;",
		"PRAGMA busy_timeout = 5000;",
	}
	for _, pragma := range pragmas {
		if _, err := s.db.Exec(pragma); err != nil {
			return fmt.Errorf("failed pragma %q: %w", pragma, err)
		}
	}

	schema := `
	CREATE TABLE IF NOT EXISTS products (
		supplier_id     TEXT PRIMARY KEY,
		sku             TEXT NOT NULL DEFAULT '',
		name            TEXT NOT NULL DEFAULT '',
		description     TEXT NOT NULL DEFAULT '',
		price_cents     INTEGER NOT NULL DEFAULT 0,
		currency        TEXT NOT NULL DEFAULT '',
		status          TEXT NOT NULL CHECK(status IN ('active', 'withdrawn')),
		version         INTEGER NOT NULL,
		withdrawn_at    TEXT NULL,
		updated_in_run  TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_products_status ON products(status);

	CREATE TABLE IF NOT EXISTS update_runs (
		id                      TEXT PRIMARY KEY,
		status                  TEXT NOT NULL CHECK(status IN ('running', 'interrupted', 'completed', 'completed_with_problems', 'failed')),
		next_cursor             TEXT NOT NULL DEFAULT '',
		batches_done            INTEGER NOT NULL DEFAULT 0,
		created_count           INTEGER NOT NULL DEFAULT 0,
		updated_count           INTEGER NOT NULL DEFAULT 0,
		withdrawn_count         INTEGER NOT NULL DEFAULT 0,
		unchanged_count         INTEGER NOT NULL DEFAULT 0,
		stale_count             INTEGER NOT NULL DEFAULT 0,
		problem_count           INTEGER NOT NULL DEFAULT 0,
		last_error              TEXT NOT NULL DEFAULT '',
		started_at              TEXT NOT NULL,
		updated_at              TEXT NOT NULL,
		finished_at             TEXT NULL
	);
	CREATE UNIQUE INDEX IF NOT EXISTS idx_single_running_run 
	ON update_runs(status) WHERE status = 'running';

	CREATE TABLE IF NOT EXISTS product_changes (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		run_id          TEXT NOT NULL REFERENCES update_runs(id),
		supplier_id     TEXT NOT NULL,
		change_type     TEXT NOT NULL CHECK(change_type IN ('created', 'updated', 'withdrawn', 'relisted')),
		summary         TEXT NOT NULL,
		created_at      TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_product_changes_run ON product_changes(run_id);

	CREATE TABLE IF NOT EXISTS problems (
		id              TEXT PRIMARY KEY,
		run_id          TEXT NOT NULL REFERENCES update_runs(id),
		batch_no        INTEGER NOT NULL,
		supplier_id     TEXT NOT NULL,
		kind            TEXT NOT NULL CHECK(kind IN ('invalid_item', 'version_conflict')),
		message         TEXT NOT NULL,
		raw_json        TEXT NOT NULL DEFAULT '',
		resolved        INTEGER NOT NULL DEFAULT 0 CHECK(resolved IN (0, 1)),
		created_at      TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_problems_run ON problems(run_id);
	CREATE INDEX IF NOT EXISTS idx_problems_unresolved ON problems(resolved) WHERE resolved = 0;
	`

	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("failed to apply schema: %w", err)
	}

	return nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

// GetProduct retrieves a product by its supplier_id.
func (s *Store) GetProduct(ctx context.Context, supplierID string) (*model.Product, error) {
	query := `
		SELECT supplier_id, sku, name, description, price_cents, currency, status, version, withdrawn_at, updated_in_run
		FROM products
		WHERE supplier_id = ?
	`
	row := s.db.QueryRowContext(ctx, query, supplierID)

	var p model.Product
	var withdrawnAt sql.NullString
	if err := row.Scan(&p.SupplierID, &p.SKU, &p.Name, &p.Description, &p.PriceCents, &p.Currency, &p.Status, &p.Version, &withdrawnAt, &p.UpdatedInRun); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("failed to get product %s: %w", supplierID, err)
	}

	if withdrawnAt.Valid && withdrawnAt.String != "" {
		t, err := time.Parse(time.RFC3339Nano, withdrawnAt.String)
		if err == nil {
			p.WithdrawnAt = &t
		}
	}
	return &p, nil
}

// ListProducts lists products according to the given filter.
func (s *Store) ListProducts(ctx context.Context, filter model.ProductFilter) ([]model.Product, error) {
	query := `
		SELECT supplier_id, sku, name, description, price_cents, currency, status, version, withdrawn_at, updated_in_run
		FROM products
	`
	var args []any
	if filter.Status != "" {
		query += " WHERE status = ?"
		args = append(args, filter.Status)
	}
	query += " ORDER BY supplier_id ASC"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list products: %w", err)
	}
	defer rows.Close()

	var products []model.Product
	for rows.Next() {
		var p model.Product
		var withdrawnAt sql.NullString
		if err := rows.Scan(&p.SupplierID, &p.SKU, &p.Name, &p.Description, &p.PriceCents, &p.Currency, &p.Status, &p.Version, &withdrawnAt, &p.UpdatedInRun); err != nil {
			return nil, fmt.Errorf("failed to scan product: %w", err)
		}
		if withdrawnAt.Valid && withdrawnAt.String != "" {
			t, err := time.Parse(time.RFC3339Nano, withdrawnAt.String)
			if err == nil {
				p.WithdrawnAt = &t
			}
		}
		products = append(products, p)
	}
	return products, rows.Err()
}

// CreateRun inserts a new update_run. Fails with unique constraint if another run is running.
func (s *Store) CreateRun(ctx context.Context, run *model.UpdateRun) error {
	query := `
		INSERT INTO update_runs (
			id, status, next_cursor, batches_done,
			created_count, updated_count, withdrawn_count, unchanged_count, stale_count, problem_count,
			last_error, started_at, updated_at, finished_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`
	var finishedAt sql.NullString
	if run.FinishedAt != nil {
		finishedAt = sql.NullString{String: run.FinishedAt.Format(time.RFC3339Nano), Valid: true}
	}

	_, err := s.db.ExecContext(ctx, query,
		run.ID,
		string(run.Status),
		run.NextCursor,
		run.BatchesDone,
		run.CreatedCount,
		run.UpdatedCount,
		run.WithdrawnCount,
		run.UnchangedCount,
		run.StaleCount,
		run.ProblemCount,
		run.LastError,
		run.StartedAt.Format(time.RFC3339Nano),
		run.UpdatedAt.Format(time.RFC3339Nano),
		finishedAt,
	)
	if err != nil {
		return fmt.Errorf("failed to insert run: %w", err)
	}
	return nil
}

// GetRun retrieves a run by its ID.
func (s *Store) GetRun(ctx context.Context, runID string) (*model.UpdateRun, error) {
	query := `
		SELECT id, status, next_cursor, batches_done,
		       created_count, updated_count, withdrawn_count, unchanged_count, stale_count, problem_count,
		       last_error, started_at, updated_at, finished_at
		FROM update_runs
		WHERE id = ?
	`
	return s.scanRun(s.db.QueryRowContext(ctx, query, runID))
}

// GetLatestRun retrieves the most recently created run.
func (s *Store) GetLatestRun(ctx context.Context) (*model.UpdateRun, error) {
	query := `
		SELECT id, status, next_cursor, batches_done,
		       created_count, updated_count, withdrawn_count, unchanged_count, stale_count, problem_count,
		       last_error, started_at, updated_at, finished_at
		FROM update_runs
		ORDER BY started_at DESC, id DESC
		LIMIT 1
	`
	return s.scanRun(s.db.QueryRowContext(ctx, query))
}

// GetLatestInterruptedRun retrieves the latest run with 'interrupted' status.
func (s *Store) GetLatestInterruptedRun(ctx context.Context) (*model.UpdateRun, error) {
	query := `
		SELECT id, status, next_cursor, batches_done,
		       created_count, updated_count, withdrawn_count, unchanged_count, stale_count, problem_count,
		       last_error, started_at, updated_at, finished_at
		FROM update_runs
		WHERE status = 'interrupted'
		ORDER BY started_at DESC, id DESC
		LIMIT 1
	`
	return s.scanRun(s.db.QueryRowContext(ctx, query))
}

func (s *Store) scanRun(row *sql.Row) (*model.UpdateRun, error) {
	var r model.UpdateRun
	var statusStr, startedAtStr, updatedAtStr string
	var finishedAt sql.NullString

	err := row.Scan(
		&r.ID, &statusStr, &r.NextCursor, &r.BatchesDone,
		&r.CreatedCount, &r.UpdatedCount, &r.WithdrawnCount, &r.UnchangedCount, &r.StaleCount, &r.ProblemCount,
		&r.LastError, &startedAtStr, &updatedAtStr, &finishedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("failed to scan run: %w", err)
	}

	r.Status = model.RunStatus(statusStr)
	r.StartedAt, _ = time.Parse(time.RFC3339Nano, startedAtStr)
	r.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAtStr)
	if finishedAt.Valid && finishedAt.String != "" {
		t, err := time.Parse(time.RFC3339Nano, finishedAt.String)
		if err == nil {
			r.FinishedAt = &t
		}
	}
	return &r, nil
}

// InterruptStaleRunningRuns marks any currently dangling 'running' run as 'interrupted'.
// Used during startup/resume when a previous process terminated ungracefully.
func (s *Store) InterruptStaleRunningRuns(ctx context.Context, reason string) error {
	query := `
		UPDATE update_runs
		SET status = 'interrupted',
		    last_error = CASE WHEN last_error = '' THEN ? ELSE last_error END,
		    updated_at = ?
		WHERE status = 'running'
	`
	now := time.Now().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx, query, reason, now)
	return err
}

// SetRunStatus updates the status, error message, and optionally marks running run as resumed or terminated.
func (s *Store) SetRunStatus(ctx context.Context, runID string, status model.RunStatus, lastError string) error {
	query := `
		UPDATE update_runs
		SET status = ?, last_error = ?, updated_at = ?
		WHERE id = ?
	`
	now := time.Now().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx, query, string(status), lastError, now, runID)
	return err
}

// CommitBatch commits all products, change history, problems, and run progress in a SINGLE atomic transaction.
func (s *Store) CommitBatch(ctx context.Context, data *model.BatchCommitData) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	// 1. Upsert products with version monotonicity safeguard
	upsertProductStmt := `
		INSERT INTO products (
			supplier_id, sku, name, description, price_cents, currency, status, version, withdrawn_at, updated_in_run
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(supplier_id) DO UPDATE SET
			sku = excluded.sku,
			name = excluded.name,
			description = excluded.description,
			price_cents = excluded.price_cents,
			currency = excluded.currency,
			status = excluded.status,
			version = excluded.version,
			withdrawn_at = excluded.withdrawn_at,
			updated_in_run = excluded.updated_in_run
		WHERE excluded.version >= products.version;
	`
	pStmt, err := tx.PrepareContext(ctx, upsertProductStmt)
	if err != nil {
		return fmt.Errorf("failed to prepare product upsert: %w", err)
	}
	defer pStmt.Close()

	for _, p := range data.ProductsToStore {
		var withdrawnAt sql.NullString
		if p.WithdrawnAt != nil {
			withdrawnAt = sql.NullString{String: p.WithdrawnAt.Format(time.RFC3339Nano), Valid: true}
		}
		if _, err := pStmt.ExecContext(ctx,
			p.SupplierID, p.SKU, p.Name, p.Description, p.PriceCents, p.Currency,
			p.Status, p.Version, withdrawnAt, p.UpdatedInRun,
		); err != nil {
			return fmt.Errorf("failed to upsert product %s: %w", p.SupplierID, err)
		}
	}

	// 2. Insert audit changes
	if len(data.Changes) > 0 {
		insertChangeStmt := `
			INSERT INTO product_changes (run_id, supplier_id, change_type, summary, created_at)
			VALUES (?, ?, ?, ?, ?)
		`
		cStmt, err := tx.PrepareContext(ctx, insertChangeStmt)
		if err != nil {
			return fmt.Errorf("failed to prepare change insert: %w", err)
		}
		defer cStmt.Close()

		for _, ch := range data.Changes {
			if _, err := cStmt.ExecContext(ctx,
				ch.RunID, ch.SupplierID, string(ch.ChangeType), ch.Summary, ch.CreatedAt.Format(time.RFC3339Nano),
			); err != nil {
				return fmt.Errorf("failed to insert change for %s: %w", ch.SupplierID, err)
			}
		}
	}

	// 3. Insert problems
	if len(data.Problems) > 0 {
		insertProblemStmt := `
			INSERT INTO problems (id, run_id, batch_no, supplier_id, kind, message, raw_json, resolved, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		`
		prStmt, err := tx.PrepareContext(ctx, insertProblemStmt)
		if err != nil {
			return fmt.Errorf("failed to prepare problem insert: %w", err)
		}
		defer prStmt.Close()

		for _, prob := range data.Problems {
			resolvedVal := 0
			if prob.Resolved {
				resolvedVal = 1
			}
			if _, err := prStmt.ExecContext(ctx,
				prob.ID, prob.RunID, prob.BatchNo, prob.SupplierID, string(prob.Kind),
				prob.Message, prob.RawJSON, resolvedVal, prob.CreatedAt.Format(time.RFC3339Nano),
			); err != nil {
				return fmt.Errorf("failed to insert problem for %s: %w", prob.SupplierID, err)
			}
		}
	}

	// 4. Update run metadata, counters, cursor, and status if done
	nowStr := time.Now().Format(time.RFC3339Nano)
	if data.IsDone {
		var finishedAtStr string
		if data.FinishedAt != nil {
			finishedAtStr = data.FinishedAt.Format(time.RFC3339Nano)
		} else {
			finishedAtStr = nowStr
		}

		updateRunStmt := `
			UPDATE update_runs
			SET next_cursor = ?,
			    batches_done = batches_done + 1,
			    created_count = created_count + ?,
			    updated_count = updated_count + ?,
			    withdrawn_count = withdrawn_count + ?,
			    unchanged_count = unchanged_count + ?,
			    stale_count = stale_count + ?,
			    problem_count = problem_count + ?,
			    status = ?,
			    updated_at = ?,
			    finished_at = ?
			WHERE id = ?
		`
		if _, err := tx.ExecContext(ctx, updateRunStmt,
			data.NextCursor,
			data.Delta.Created, data.Delta.Updated, data.Delta.Withdrawn,
			data.Delta.Unchanged, data.Delta.Stale, data.Delta.Problem,
			string(data.FinalStatus), nowStr, finishedAtStr, data.RunID,
		); err != nil {
			return fmt.Errorf("failed to update final run status: %w", err)
		}
	} else {
		updateRunStmt := `
			UPDATE update_runs
			SET next_cursor = ?,
			    batches_done = batches_done + 1,
			    created_count = created_count + ?,
			    updated_count = updated_count + ?,
			    withdrawn_count = withdrawn_count + ?,
			    unchanged_count = unchanged_count + ?,
			    stale_count = stale_count + ?,
			    problem_count = problem_count + ?,
			    updated_at = ?
			WHERE id = ?
		`
		if _, err := tx.ExecContext(ctx, updateRunStmt,
			data.NextCursor,
			data.Delta.Created, data.Delta.Updated, data.Delta.Withdrawn,
			data.Delta.Unchanged, data.Delta.Stale, data.Delta.Problem,
			nowStr, data.RunID,
		); err != nil {
			return fmt.Errorf("failed to update run progress: %w", err)
		}
	}

	return tx.Commit()
}

// ListChanges retrieves audit changes recorded for a specific run.
func (s *Store) ListChanges(ctx context.Context, runID string) ([]model.ProductChange, error) {
	query := `
		SELECT id, run_id, supplier_id, change_type, summary, created_at
		FROM product_changes
		WHERE run_id = ?
		ORDER BY id ASC
	`
	rows, err := s.db.QueryContext(ctx, query, runID)
	if err != nil {
		return nil, fmt.Errorf("failed to list changes: %w", err)
	}
	defer rows.Close()

	var changes []model.ProductChange
	for rows.Next() {
		var ch model.ProductChange
		var chType, createdAtStr string
		if err := rows.Scan(&ch.ID, &ch.RunID, &ch.SupplierID, &chType, &ch.Summary, &createdAtStr); err != nil {
			return nil, fmt.Errorf("failed to scan change: %w", err)
		}
		ch.ChangeType = model.ChangeType(chType)
		ch.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAtStr)
		changes = append(changes, ch)
	}
	return changes, rows.Err()
}

// ListProblems retrieves recorded problems.
func (s *Store) ListProblems(ctx context.Context, filter model.ProblemFilter) ([]model.Problem, error) {
	query := `
		SELECT id, run_id, batch_no, supplier_id, kind, message, raw_json, resolved, created_at
		FROM problems
		WHERE 1=1
	`
	var args []any
	if filter.RunID != "" {
		query += " AND run_id = ?"
		args = append(args, filter.RunID)
	}
	if filter.UnresolvedOnly {
		query += " AND resolved = 0"
	}
	query += " ORDER BY created_at ASC, id ASC"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list problems: %w", err)
	}
	defer rows.Close()

	var problems []model.Problem
	for rows.Next() {
		var pr model.Problem
		var kindStr, createdAtStr string
		var resolvedInt int
		if err := rows.Scan(&pr.ID, &pr.RunID, &pr.BatchNo, &pr.SupplierID, &kindStr, &pr.Message, &pr.RawJSON, &resolvedInt, &createdAtStr); err != nil {
			return nil, fmt.Errorf("failed to scan problem: %w", err)
		}
		pr.Kind = model.ProblemKind(kindStr)
		pr.Resolved = resolvedInt == 1
		pr.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAtStr)
		problems = append(problems, pr)
	}
	return problems, rows.Err()
}

// ResolveProblem marks a problem as resolved.
func (s *Store) ResolveProblem(ctx context.Context, problemID string) error {
	query := `UPDATE problems SET resolved = 1 WHERE id = ?`
	res, err := s.db.ExecContext(ctx, query, problemID)
	if err != nil {
		return fmt.Errorf("failed to resolve problem %s: %w", problemID, err)
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

