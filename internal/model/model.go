package model

import "time"

// Product status constants
const (
	ProductStatusActive    = "active"
	ProductStatusWithdrawn = "withdrawn"
)

// Product represents the current state of a catalog item in the database.
type Product struct {
	SupplierID   string     `json:"supplier_id"`
	SKU          string     `json:"sku"`
	Name         string     `json:"name"`
	Description  string     `json:"description"`
	PriceCents   int64      `json:"price_cents"`
	Currency     string     `json:"currency"`
	Status       string     `json:"status"` // active | withdrawn
	Version      int64      `json:"version"`
	WithdrawnAt  *time.Time `json:"withdrawn_at,omitempty"`
	UpdatedInRun string     `json:"updated_in_run"`
}

// SupplierItem represents an item payload received from the supplier feed.
type SupplierItem struct {
	SupplierID  string `json:"supplier_id"`
	SKU         string `json:"sku"`
	Name        string `json:"name"`
	Description string `json:"description"`
	PriceCents  int64  `json:"price_cents"`
	Currency    string `json:"currency"`
	Status      string `json:"status"` // active | withdrawn
	Version     int64  `json:"version"`
}

// RunStatus represents the state of an update run.
type RunStatus string

const (
	RunStatusRunning               RunStatus = "running"
	RunStatusInterrupted           RunStatus = "interrupted"
	RunStatusCompleted             RunStatus = "completed"
	RunStatusCompletedWithProblems RunStatus = "completed_with_problems"
	RunStatusFailed                RunStatus = "failed"
)

// UpdateRun records metadata and progress of a synchronization run.
type UpdateRun struct {
	ID             string     `json:"id"`
	Status         RunStatus  `json:"status"`
	NextCursor     string     `json:"next_cursor"`
	BatchesDone    int        `json:"batches_done"`
	CreatedCount   int        `json:"created_count"`
	UpdatedCount   int        `json:"updated_count"`
	WithdrawnCount int        `json:"withdrawn_count"`
	UnchangedCount int        `json:"unchanged_count"`
	StaleCount     int        `json:"stale_count"`
	ProblemCount   int        `json:"problem_count"`
	LastError      string     `json:"last_error"`
	StartedAt      time.Time  `json:"started_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
}

// ChangeType represents the classification of a product mutation.
type ChangeType string

const (
	ChangeTypeCreated   ChangeType = "created"
	ChangeTypeUpdated   ChangeType = "updated"
	ChangeTypeWithdrawn ChangeType = "withdrawn"
	ChangeTypeRelisted  ChangeType = "relisted"
)

// ProductChange records what changed for auditing and reporting.
type ProductChange struct {
	ID         int64      `json:"id"`
	RunID      string     `json:"run_id"`
	SupplierID string     `json:"supplier_id"`
	ChangeType ChangeType `json:"change_type"`
	Summary    string     `json:"summary"`
	CreatedAt  time.Time  `json:"created_at"`
}

// ProblemKind defines the category of an encountered problem.
type ProblemKind string

const (
	ProblemKindInvalidItem     ProblemKind = "invalid_item"
	ProblemKindVersionConflict ProblemKind = "version_conflict"
)

// Problem records anomalies needing human attention.
type Problem struct {
	ID         string      `json:"id"`
	RunID      string      `json:"run_id"`
	BatchNo    int         `json:"batch_no"`
	SupplierID string      `json:"supplier_id"`
	Kind       ProblemKind `json:"kind"`
	Message    string      `json:"message"`
	RawJSON    string      `json:"raw_json"`
	Resolved   bool        `json:"resolved"`
	CreatedAt  time.Time   `json:"created_at"`
}

// BatchResponse is the response format from supplier's GET /v1/products.
type BatchResponse struct {
	Items      []SupplierItem `json:"items"`
	NextCursor string         `json:"next_cursor"`
	Done       bool           `json:"done"`
}

// BatchDelta accumulates counters for a single processed batch.
type BatchDelta struct {
	Created   int
	Updated   int
	Withdrawn int
	Unchanged int
	Stale     int
	Problem   int
}

// BatchCommitData bundles all changes produced in a single batch for atomic DB commit.
type BatchCommitData struct {
	RunID           string
	BatchNo         int
	ProductsToStore []*Product
	Changes         []ProductChange
	Problems        []Problem
	Delta           BatchDelta
	NextCursor      string
	IsDone          bool
	FinalStatus     RunStatus
	FinishedAt      *time.Time
}

// ProductFilter defines filtering criteria when querying products.
type ProductFilter struct {
	Status string // "active", "withdrawn", or "" for all
}

// ProblemFilter defines filtering criteria when querying problems.
type ProblemFilter struct {
	RunID          string
	UnresolvedOnly bool
}

