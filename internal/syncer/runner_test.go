package syncer_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"supplier-catalog-updates/internal/model"
	"supplier-catalog-updates/internal/store"
	"supplier-catalog-updates/internal/supplier"
	"supplier-catalog-updates/internal/syncer"
)

// mockSupplier implements supplier.Client for unit/integration tests
type mockSupplier struct {
	batches       map[string]*model.BatchResponse
	failOnCursor  map[string]error
	callsPerCursor map[string]int
}

func newMockSupplier() *mockSupplier {
	return &mockSupplier{
		batches:        make(map[string]*model.BatchResponse),
		failOnCursor:   make(map[string]error),
		callsPerCursor: make(map[string]int),
	}
}

func (m *mockSupplier) FetchBatch(ctx context.Context, cursor string, limit int) (*model.BatchResponse, error) {
	m.callsPerCursor[cursor]++
	if err, ok := m.failOnCursor[cursor]; ok && err != nil {
		return nil, err
	}
	resp, ok := m.batches[cursor]
	if !ok {
		return nil, &supplier.PermanentError{StatusCode: 404, Message: "cursor not found"}
	}
	return resp, nil
}

func setupTestEnv(t *testing.T) (*store.Store, string, func()) {
	t.Helper()
	tempDir, err := os.MkdirTemp("", "runner-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}

	dbPath := filepath.Join(tempDir, "catalog.db")
	st, err := store.Open(dbPath)
	if err != nil {
		os.RemoveAll(tempDir)
		t.Fatalf("failed to open store: %v", err)
	}

	cleanup := func() {
		st.Close()
		os.RemoveAll(tempDir)
	}
	return st, dbPath, cleanup
}

func TestRunner_FullLifecycleAndIdempotency(t *testing.T) {
	st, _, cleanup := setupTestEnv(t)
	defer cleanup()

	ctx := context.Background()

	// Mock data: 2 batches
	mockSup := newMockSupplier()
	mockSup.batches[""] = &model.BatchResponse{
		Items: []model.SupplierItem{
			{SupplierID: "P1", Name: "Mouse", PriceCents: 2500, Currency: "USD", Status: model.ProductStatusActive, Version: 1},
			{SupplierID: "P2", Name: "Keyboard", PriceCents: 5000, Currency: "USD", Status: model.ProductStatusActive, Version: 1},
		},
		NextCursor: "c1",
		Done:       false,
	}
	mockSup.batches["c1"] = &model.BatchResponse{
		Items: []model.SupplierItem{
			{SupplierID: "P3", Name: "Monitor", PriceCents: 15000, Currency: "USD", Status: model.ProductStatusActive, Version: 1},
		},
		NextCursor: "c2",
		Done:       true,
	}

	cfg := syncer.RunnerConfig{
		Retries:   2,
		Backoff:   10 * time.Millisecond,
		BatchSize: 10,
	}
	runner := syncer.NewRunner(st, mockSup, cfg)

	// Run 1: Normal initial sync
	run1, err := runner.Start(ctx)
	if err != nil {
		t.Fatalf("run 1 failed: %v", err)
	}
	if run1.Status != model.RunStatusCompleted {
		t.Fatalf("expected completed status, got %s", run1.Status)
	}
	if run1.CreatedCount != 3 || run1.BatchesDone != 2 {
		t.Fatalf("expected 3 created, 2 batches, got %+v", run1)
	}

	// Verify products in DB
	p1, err := st.GetProduct(ctx, "P1")
	if err != nil || p1.PriceCents != 2500 {
		t.Fatalf("product P1 mismatch: %+v", p1)
	}

	changes, err := st.ListChanges(ctx, run1.ID)
	if err != nil || len(changes) != 3 {
		t.Fatalf("expected 3 changes, got %d", len(changes))
	}

	// Run 2: Idempotency check with the exact same feed
	run2, err := runner.Start(ctx)
	if err != nil {
		t.Fatalf("run 2 failed: %v", err)
	}
	if run2.Status != model.RunStatusCompleted {
		t.Fatalf("expected run 2 completed, got %s", run2.Status)
	}
	if run2.CreatedCount != 0 || run2.UpdatedCount != 0 || run2.UnchangedCount != 3 {
		t.Fatalf("idempotency violated! Expected 0 created/updated, 3 unchanged, got %+v", run2)
	}

	// Run 2 should have 0 changes logged
	changes2, err := st.ListChanges(ctx, run2.ID)
	if err != nil || len(changes2) != 0 {
		t.Fatalf("expected 0 changes on identical run, got %d", len(changes2))
	}
}

func TestRunner_OutageAndResume(t *testing.T) {
	st, dbPath, cleanup := setupTestEnv(t)
	defer cleanup()

	ctx := context.Background()

	mockSup := newMockSupplier()
	mockSup.batches[""] = &model.BatchResponse{
		Items: []model.SupplierItem{
			{SupplierID: "P1", Name: "Item 1", PriceCents: 1000, Currency: "USD", Status: model.ProductStatusActive, Version: 1},
		},
		NextCursor: "cursor-outage",
		Done:       false,
	}
	mockSup.batches["cursor-outage"] = &model.BatchResponse{
		Items: []model.SupplierItem{
			{SupplierID: "P2", Name: "Item 2", PriceCents: 2000, Currency: "USD", Status: model.ProductStatusActive, Version: 1},
		},
		NextCursor: "cursor-end",
		Done:       true,
	}

	// Simulate outage on batch 2
	mockSup.failOnCursor["cursor-outage"] = &supplier.TransientError{StatusCode: 503, Message: "service unavailable"}

	cfg := syncer.RunnerConfig{
		Retries:   2,
		Backoff:   5 * time.Millisecond,
		BatchSize: 10,
	}
	runner := syncer.NewRunner(st, mockSup, cfg)

	// Start run: batch 1 succeeds, batch 2 encounters 503 and exhausts retries
	run, err := runner.Start(ctx)
	if err == nil {
		t.Fatal("expected error due to outage, got nil")
	}
	if run.Status != model.RunStatusInterrupted {
		t.Fatalf("expected status interrupted, got %s", run.Status)
	}

	// Invariant 4 & 5: Batch 1 was committed, catalog has P1
	p1, err := st.GetProduct(ctx, "P1")
	if err != nil || p1.SupplierID != "P1" {
		t.Fatalf("P1 should have been saved in batch 1: %+v", p1)
	}
	if run.BatchesDone != 1 || run.CreatedCount != 1 {
		t.Fatalf("batch 1 progress should be preserved, got %+v", run)
	}

	// Simulate app restart / reopen DB
	st.Close()
	st2, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to reopen store: %v", err)
	}
	defer st2.Close()

	// Outage resolved
	mockSup.failOnCursor["cursor-outage"] = nil

	runner2 := syncer.NewRunner(st2, mockSup, cfg)
	resumedRun, err := runner2.Resume(ctx, run.ID)
	if err != nil {
		t.Fatalf("resume failed: %v", err)
	}
	if resumedRun.Status != model.RunStatusCompleted {
		t.Fatalf("expected completed status after resume, got %s", resumedRun.Status)
	}
	if resumedRun.BatchesDone != 2 || resumedRun.CreatedCount != 2 {
		t.Fatalf("expected 2 batches done, 2 created, got %+v", resumedRun)
	}

	// Both products must exist
	p2, err := st2.GetProduct(ctx, "P2")
	if err != nil || p2.SupplierID != "P2" {
		t.Fatalf("P2 should exist after resume: %+v", p2)
	}
}

func TestRunner_ProblemHandling(t *testing.T) {
	st, _, cleanup := setupTestEnv(t)
	defer cleanup()

	ctx := context.Background()

	mockSup := newMockSupplier()
	mockSup.batches[""] = &model.BatchResponse{
		Items: []model.SupplierItem{
			{SupplierID: "P1", Name: "Valid", PriceCents: 1000, Currency: "USD", Status: model.ProductStatusActive, Version: 1},
			{SupplierID: "P2", Name: "Invalid Negative Price", PriceCents: -500, Currency: "USD", Status: model.ProductStatusActive, Version: 1},
		},
		NextCursor: "done-cursor",
		Done:       true,
	}

	cfg := syncer.RunnerConfig{Retries: 1, Backoff: 5 * time.Millisecond}
	runner := syncer.NewRunner(st, mockSup, cfg)

	run, err := runner.Start(ctx)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}

	// Should finish with completed_with_problems
	if run.Status != model.RunStatusCompletedWithProblems {
		t.Fatalf("expected completed_with_problems, got %s", run.Status)
	}
	if run.ProblemCount != 1 || run.CreatedCount != 1 {
		t.Fatalf("counters mismatch: %+v", run)
	}

	// Verify problem record in DB
	probs, err := st.ListProblems(ctx, model.ProblemFilter{RunID: run.ID})
	if err != nil || len(probs) != 1 {
		t.Fatalf("expected 1 problem in store, got %d", len(probs))
	}
	if probs[0].Kind != model.ProblemKindInvalidItem {
		t.Fatalf("expected kind invalid_item, got %s", probs[0].Kind)
	}
}

func TestRunner_PermanentErrorCannotResume(t *testing.T) {
	st, _, cleanup := setupTestEnv(t)
	defer cleanup()

	ctx := context.Background()

	mockSup := newMockSupplier()
	mockSup.batches[""] = &model.BatchResponse{
		Items:      []model.SupplierItem{},
		NextCursor: "c1",
		Done:       false,
	}
	mockSup.failOnCursor[""] = &supplier.PermanentError{StatusCode: 400, Message: "bad request"}

	cfg := syncer.RunnerConfig{Retries: 2, Backoff: 5 * time.Millisecond}
	runner := syncer.NewRunner(st, mockSup, cfg)

	run, err := runner.Start(ctx)
	if err == nil {
		t.Fatal("expected permanent error, got nil")
	}
	if run.Status != model.RunStatusFailed {
		t.Fatalf("expected failed status, got %s", run.Status)
	}

	// Attempting to resume a failed run must return an error
	_, resumeErr := runner.Resume(ctx, run.ID)
	if resumeErr == nil {
		t.Fatal("expected error resuming failed run, got nil")
	}
}

func TestRunner_StalledCursorSafeguard(t *testing.T) {
	st, _, cleanup := setupTestEnv(t)
	defer cleanup()

	ctx := context.Background()

	// Supplier returns same cursor while done=false
	mockSup := newMockSupplier()
	mockSup.batches[""] = &model.BatchResponse{
		Items:      []model.SupplierItem{},
		NextCursor: "",
		Done:       false,
	}

	cfg := syncer.RunnerConfig{Retries: 1, Backoff: 5 * time.Millisecond}
	runner := syncer.NewRunner(st, mockSup, cfg)

	run, err := runner.Start(ctx)
	if err == nil || !errors.Is(err, err) {
		t.Logf("got error: %v", err)
	}
	if run.Status != model.RunStatusFailed {
		t.Fatalf("expected failed status on stalled cursor, got %s", run.Status)
	}
}

