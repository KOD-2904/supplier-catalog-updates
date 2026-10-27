package store_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"supplier-catalog-updates/internal/model"
	"supplier-catalog-updates/internal/store"
)

func newTestStore(t *testing.T) (*store.Store, func()) {
	t.Helper()
	tempDir, err := os.MkdirTemp("", "catalog-store-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}

	dbPath := filepath.Join(tempDir, "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		os.RemoveAll(tempDir)
		t.Fatalf("store.Open failed: %v", err)
	}

	cleanup := func() {
		st.Close()
		os.RemoveAll(tempDir)
	}
	return st, cleanup
}

func TestStore_SingleRunningRunConstraint(t *testing.T) {
	st, cleanup := newTestStore(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now()

	run1 := &model.UpdateRun{
		ID:        "run-1",
		Status:    model.RunStatusRunning,
		StartedAt: now,
		UpdatedAt: now,
	}
	if err := st.CreateRun(ctx, run1); err != nil {
		t.Fatalf("expected first running run to succeed: %v", err)
	}

	// Attempting to create a second running run must fail due to partial unique index
	run2 := &model.UpdateRun{
		ID:        "run-2",
		Status:    model.RunStatusRunning,
		StartedAt: now,
		UpdatedAt: now,
	}
	if err := st.CreateRun(ctx, run2); err == nil {
		t.Fatal("expected second running run to fail with unique constraint, but got nil")
	}

	// But an interrupted or completed run is allowed
	run3 := &model.UpdateRun{
		ID:        "run-3",
		Status:    model.RunStatusInterrupted,
		StartedAt: now,
		UpdatedAt: now,
	}
	if err := st.CreateRun(ctx, run3); err != nil {
		t.Fatalf("interrupted run should succeed: %v", err)
	}
}

func TestStore_InterruptStaleRunningRuns(t *testing.T) {
	st, cleanup := newTestStore(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now()

	run := &model.UpdateRun{
		ID:        "run-crashed",
		Status:    model.RunStatusRunning,
		StartedAt: now,
		UpdatedAt: now,
	}
	if err := st.CreateRun(ctx, run); err != nil {
		t.Fatalf("failed to create run: %v", err)
	}

	if err := st.InterruptStaleRunningRuns(ctx, "crashed process recovered"); err != nil {
		t.Fatalf("InterruptStaleRunningRuns failed: %v", err)
	}

	loaded, err := st.GetRun(ctx, "run-crashed")
	if err != nil {
		t.Fatalf("failed to get run: %v", err)
	}
	if loaded.Status != model.RunStatusInterrupted {
		t.Fatalf("expected status interrupted, got %s", loaded.Status)
	}
	if loaded.LastError != "crashed process recovered" {
		t.Fatalf("expected error message to be set, got %s", loaded.LastError)
	}
}

func TestStore_CommitBatch_AtomicityAndSQLSafeguard(t *testing.T) {
	st, cleanup := newTestStore(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now()

	run := &model.UpdateRun{
		ID:        "run-batch-1",
		Status:    model.RunStatusRunning,
		StartedAt: now,
		UpdatedAt: now,
	}
	if err := st.CreateRun(ctx, run); err != nil {
		t.Fatalf("failed to create run: %v", err)
	}

	prod1 := &model.Product{
		SupplierID:   "ITEM-1",
		SKU:          "SKU-1",
		Name:         "Widget v2",
		PriceCents:   5000,
		Currency:     "USD",
		Status:       model.ProductStatusActive,
		Version:      2,
		UpdatedInRun: run.ID,
	}

	batchData := &model.BatchCommitData{
		RunID:           run.ID,
		BatchNo:         1,
		ProductsToStore: []*model.Product{prod1},
		Changes: []model.ProductChange{
			{
				RunID:      run.ID,
				SupplierID: "ITEM-1",
				ChangeType: model.ChangeTypeCreated,
				Summary:    "created Widget v2",
				CreatedAt:  now,
			},
		},
		Problems: []model.Problem{
			{
				ID:         "prob-1",
				RunID:      run.ID,
				BatchNo:    1,
				SupplierID: "ITEM-X",
				Kind:       model.ProblemKindInvalidItem,
				Message:    "missing name",
				RawJSON:    `{}`,
				Resolved:   false,
				CreatedAt:  now,
			},
		},
		Delta: model.BatchDelta{
			Created: 1,
			Problem: 1,
		},
		NextCursor:  "cursor-2",
		IsDone:      false,
		FinalStatus: model.RunStatusRunning,
	}

	if err := st.CommitBatch(ctx, batchData); err != nil {
		t.Fatalf("CommitBatch failed: %v", err)
	}

	// Verify run state updated
	loadedRun, err := st.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("failed to get run: %v", err)
	}
	if loadedRun.BatchesDone != 1 || loadedRun.CreatedCount != 1 || loadedRun.ProblemCount != 1 {
		t.Fatalf("counters mismatch: %+v", loadedRun)
	}
	if loadedRun.NextCursor != "cursor-2" {
		t.Fatalf("cursor mismatch: %s", loadedRun.NextCursor)
	}

	// Verify product saved
	loadedProd, err := st.GetProduct(ctx, "ITEM-1")
	if err != nil {
		t.Fatalf("failed to get product: %v", err)
	}
	if loadedProd.Version != 2 || loadedProd.Name != "Widget v2" {
		t.Fatalf("unexpected product content: %+v", loadedProd)
	}

	// SQL safeguard: simulate an upsert with lower version (version 1)
	lowerProd := &model.Product{
		SupplierID:   "ITEM-1",
		SKU:          "SKU-1",
		Name:         "Widget v1 - Stale Attempt",
		PriceCents:   1000,
		Currency:     "USD",
		Status:       model.ProductStatusActive,
		Version:      1, // lower than version 2 in DB!
		UpdatedInRun: run.ID,
	}
	batchData2 := &model.BatchCommitData{
		RunID:           run.ID,
		BatchNo:         2,
		ProductsToStore: []*model.Product{lowerProd},
		Delta:           model.BatchDelta{Stale: 1},
		NextCursor:      "cursor-3",
		IsDone:          true,
		FinalStatus:     model.RunStatusCompletedWithProblems,
		FinishedAt:      &now,
	}

	if err := st.CommitBatch(ctx, batchData2); err != nil {
		t.Fatalf("CommitBatch 2 failed: %v", err)
	}

	// The product in DB should STILL be version 2 and "Widget v2", not overwritten!
	refreshedProd, err := st.GetProduct(ctx, "ITEM-1")
	if err != nil {
		t.Fatalf("failed to get product: %v", err)
	}
	if refreshedProd.Version != 2 || refreshedProd.Name != "Widget v2" {
		t.Fatalf("SQL safeguard failed! Product was overwritten by lower version: %+v", refreshedProd)
	}

	// The run must now be completed_with_problems
	finalRun, err := st.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("failed to get final run: %v", err)
	}
	if finalRun.Status != model.RunStatusCompletedWithProblems {
		t.Fatalf("expected completed_with_problems, got %s", finalRun.Status)
	}
	if finalRun.FinishedAt == nil {
		t.Fatal("expected FinishedAt to be populated")
	}
}

func TestStore_ProblemsAndResolve(t *testing.T) {
	st, cleanup := newTestStore(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now()

	run := &model.UpdateRun{
		ID:        "run-prob",
		Status:    model.RunStatusRunning,
		StartedAt: now,
		UpdatedAt: now,
	}
	_ = st.CreateRun(ctx, run)

	batch := &model.BatchCommitData{
		RunID:      run.ID,
		BatchNo:    1,
		NextCursor: "end",
		Problems: []model.Problem{
			{
				ID:         "p-1",
				RunID:      run.ID,
				BatchNo:    1,
				SupplierID: "SUP-A",
				Kind:       model.ProblemKindInvalidItem,
				Message:    "negative price",
				Resolved:   false,
				CreatedAt:  now,
			},
		},
		Delta:       model.BatchDelta{Problem: 1},
		IsDone:      true,
		FinalStatus: model.RunStatusCompletedWithProblems,
	}
	_ = st.CommitBatch(ctx, batch)

	probs, err := st.ListProblems(ctx, model.ProblemFilter{RunID: run.ID, UnresolvedOnly: true})
	if err != nil || len(probs) != 1 {
		t.Fatalf("expected 1 unresolved problem, got %d (err: %v)", len(probs), err)
	}

	// Resolve the problem
	if err := st.ResolveProblem(ctx, "p-1"); err != nil {
		t.Fatalf("failed to resolve problem: %v", err)
	}

	unresolved, err := st.ListProblems(ctx, model.ProblemFilter{RunID: run.ID, UnresolvedOnly: true})
	if err != nil || len(unresolved) != 0 {
		t.Fatalf("expected 0 unresolved problems after resolve, got %d", len(unresolved))
	}
}

