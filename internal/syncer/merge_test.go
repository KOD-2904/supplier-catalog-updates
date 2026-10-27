package syncer_test

import (
	"testing"
	"time"

	"supplier-catalog-updates/internal/model"
	"supplier-catalog-updates/internal/syncer"
)

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		item    model.SupplierItem
		wantErr bool
	}{
		{
			name: "valid active item",
			item: model.SupplierItem{
				SupplierID: "P-100",
				Name:       "Mechanical Keyboard",
				PriceCents: 9900,
				Currency:   "USD",
				Status:     model.ProductStatusActive,
				Version:    1,
			},
			wantErr: false,
		},
		{
			name: "valid withdrawn item with minimal fields",
			item: model.SupplierItem{
				SupplierID: "P-100",
				Status:     model.ProductStatusWithdrawn,
				Version:    2,
			},
			wantErr: false,
		},
		{
			name: "missing supplier_id",
			item: model.SupplierItem{
				SupplierID: "  ",
				Name:       "Keyboard",
				PriceCents: 1000,
				Currency:   "USD",
				Status:     model.ProductStatusActive,
				Version:    1,
			},
			wantErr: true,
		},
		{
			name: "invalid version 0",
			item: model.SupplierItem{
				SupplierID: "P-100",
				Name:       "Keyboard",
				PriceCents: 1000,
				Currency:   "USD",
				Status:     model.ProductStatusActive,
				Version:    0,
			},
			wantErr: true,
		},
		{
			name: "negative version",
			item: model.SupplierItem{
				SupplierID: "P-100",
				Name:       "Keyboard",
				PriceCents: 1000,
				Currency:   "USD",
				Status:     model.ProductStatusActive,
				Version:    -1,
			},
			wantErr: true,
		},
		{
			name: "unknown status",
			item: model.SupplierItem{
				SupplierID: "P-100",
				Name:       "Keyboard",
				PriceCents: 1000,
				Currency:   "USD",
				Status:     "archived",
				Version:    1,
			},
			wantErr: true,
		},
		{
			name: "active product missing name",
			item: model.SupplierItem{
				SupplierID: "P-100",
				Name:       "",
				PriceCents: 1000,
				Currency:   "USD",
				Status:     model.ProductStatusActive,
				Version:    1,
			},
			wantErr: true,
		},
		{
			name: "active product missing currency",
			item: model.SupplierItem{
				SupplierID: "P-100",
				Name:       "Keyboard",
				PriceCents: 1000,
				Currency:   "",
				Status:     model.ProductStatusActive,
				Version:    1,
			},
			wantErr: true,
		},
		{
			name: "active product negative price",
			item: model.SupplierItem{
				SupplierID: "P-100",
				Name:       "Keyboard",
				PriceCents: -50,
				Currency:   "USD",
				Status:     model.ProductStatusActive,
				Version:    1,
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := syncer.Validate(tt.item)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestMergeTable(t *testing.T) {
	now := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	runID := "run-001"

	t.Run("Case 1: No cur, in active -> Created", func(t *testing.T) {
		in := model.SupplierItem{
			SupplierID:  "SUP-1",
			SKU:         "SKU-1",
			Name:        "Item 1",
			Description: "Desc 1",
			PriceCents:  1500,
			Currency:    "USD",
			Status:      model.ProductStatusActive,
			Version:     1,
		}

		dec := syncer.Merge(nil, in, runID, now)
		if dec.Type != syncer.DecisionCreated {
			t.Fatalf("expected DecisionCreated, got %v", dec.Type)
		}
		if dec.Product == nil || dec.Product.Status != model.ProductStatusActive {
			t.Fatalf("expected product to be active, got %+v", dec.Product)
		}
		if dec.Change == nil || dec.Change.ChangeType != model.ChangeTypeCreated {
			t.Fatalf("expected ChangeTypeCreated, got %+v", dec.Change)
		}
		if dec.Delta.Created != 1 {
			t.Fatalf("expected Created delta = 1, got %d", dec.Delta.Created)
		}
	})

	t.Run("Case 2: No cur, in withdrawn -> Tombstone", func(t *testing.T) {
		in := model.SupplierItem{
			SupplierID: "SUP-2",
			Status:     model.ProductStatusWithdrawn,
			Version:    1,
		}

		dec := syncer.Merge(nil, in, runID, now)
		if dec.Type != syncer.DecisionTombstone {
			t.Fatalf("expected DecisionTombstone, got %v", dec.Type)
		}
		if dec.Product == nil || dec.Product.Status != model.ProductStatusWithdrawn {
			t.Fatalf("expected product status withdrawn, got %+v", dec.Product)
		}
		if dec.Product.WithdrawnAt == nil {
			t.Fatal("expected WithdrawnAt timestamp to be set")
		}
		if dec.Delta.Withdrawn != 1 {
			t.Fatalf("expected Withdrawn delta = 1, got %d", dec.Delta.Withdrawn)
		}
	})

	t.Run("Case 3: in.version < cur.version -> Stale ignored", func(t *testing.T) {
		cur := &model.Product{
			SupplierID: "SUP-3",
			Status:     model.ProductStatusActive,
			Version:    5,
		}
		in := model.SupplierItem{
			SupplierID: "SUP-3",
			Status:     model.ProductStatusActive,
			Version:    4,
		}

		dec := syncer.Merge(cur, in, runID, now)
		if dec.Type != syncer.DecisionStale {
			t.Fatalf("expected DecisionStale, got %v", dec.Type)
		}
		if dec.Product != nil || dec.Change != nil {
			t.Fatal("expected no product or change mutation for stale item")
		}
		if dec.Delta.Stale != 1 {
			t.Fatalf("expected Stale delta = 1, got %d", dec.Delta.Stale)
		}
	})

	t.Run("Case 4: in.version == cur.version, same content -> Unchanged", func(t *testing.T) {
		cur := &model.Product{
			SupplierID:  "SUP-4",
			SKU:         "SKU-4",
			Name:        "Name 4",
			Description: "Desc 4",
			PriceCents:  2000,
			Currency:    "USD",
			Status:      model.ProductStatusActive,
			Version:     2,
		}
		in := model.SupplierItem{
			SupplierID:  "SUP-4",
			SKU:         "SKU-4",
			Name:        "Name 4",
			Description: "Desc 4",
			PriceCents:  2000,
			Currency:    "USD",
			Status:      model.ProductStatusActive,
			Version:     2,
		}

		dec := syncer.Merge(cur, in, runID, now)
		if dec.Type != syncer.DecisionUnchanged {
			t.Fatalf("expected DecisionUnchanged, got %v", dec.Type)
		}
		if dec.Product != nil || dec.Change != nil || dec.Problem != nil {
			t.Fatal("expected no product, change, or problem for identical item")
		}
		if dec.Delta.Unchanged != 1 {
			t.Fatalf("expected Unchanged delta = 1, got %d", dec.Delta.Unchanged)
		}
	})

	t.Run("Case 5: in.version == cur.version, different content -> Conflict problem", func(t *testing.T) {
		cur := &model.Product{
			SupplierID:  "SUP-5",
			SKU:         "SKU-5",
			Name:        "Name Old",
			Description: "Desc",
			PriceCents:  2000,
			Currency:    "USD",
			Status:      model.ProductStatusActive,
			Version:     2,
		}
		in := model.SupplierItem{
			SupplierID:  "SUP-5",
			SKU:         "SKU-5",
			Name:        "Name New",
			Description: "Desc",
			PriceCents:  2500,
			Currency:    "USD",
			Status:      model.ProductStatusActive,
			Version:     2,
		}

		dec := syncer.Merge(cur, in, runID, now)
		if dec.Type != syncer.DecisionConflict {
			t.Fatalf("expected DecisionConflict, got %v", dec.Type)
		}
		if dec.Product != nil {
			t.Fatal("conflict item must not overwrite database product")
		}
		if dec.Problem == nil || dec.Problem.Kind != model.ProblemKindVersionConflict {
			t.Fatalf("expected ProblemKindVersionConflict, got %+v", dec.Problem)
		}
		if dec.Delta.Problem != 1 {
			t.Fatalf("expected Problem delta = 1, got %d", dec.Delta.Problem)
		}
	})

	t.Run("Case 6: in.version > cur.version, in withdrawn -> Withdrawn", func(t *testing.T) {
		cur := &model.Product{
			SupplierID: "SUP-6",
			Name:       "Old Product",
			Status:     model.ProductStatusActive,
			Version:    2,
		}
		in := model.SupplierItem{
			SupplierID: "SUP-6",
			Status:     model.ProductStatusWithdrawn,
			Version:    3,
		}

		dec := syncer.Merge(cur, in, runID, now)
		if dec.Type != syncer.DecisionUpdated {
			t.Fatalf("expected DecisionUpdated, got %v", dec.Type)
		}
		if dec.Product == nil || dec.Product.Status != model.ProductStatusWithdrawn {
			t.Fatalf("expected product to become withdrawn, got %+v", dec.Product)
		}
		if dec.Product.Version != 3 {
			t.Fatalf("expected version 3, got %d", dec.Product.Version)
		}
		if dec.Change == nil || dec.Change.ChangeType != model.ChangeTypeWithdrawn {
			t.Fatalf("expected ChangeTypeWithdrawn, got %+v", dec.Change)
		}
		if dec.Delta.Withdrawn != 1 {
			t.Fatalf("expected Withdrawn delta = 1, got %d", dec.Delta.Withdrawn)
		}
	})

	t.Run("Case 7: in.version > cur.version, in active, cur active, details changed -> Updated", func(t *testing.T) {
		cur := &model.Product{
			SupplierID: "SUP-7",
			SKU:        "SKU-7",
			Name:       "Old Name",
			PriceCents: 1000,
			Currency:   "USD",
			Status:     model.ProductStatusActive,
			Version:    1,
		}
		in := model.SupplierItem{
			SupplierID: "SUP-7",
			SKU:        "SKU-7",
			Name:       "New Name",
			PriceCents: 1500,
			Currency:   "USD",
			Status:     model.ProductStatusActive,
			Version:    2,
		}

		dec := syncer.Merge(cur, in, runID, now)
		if dec.Type != syncer.DecisionUpdated {
			t.Fatalf("expected DecisionUpdated, got %v", dec.Type)
		}
		if dec.Product == nil || dec.Product.Name != "New Name" || dec.Product.PriceCents != 1500 {
			t.Fatalf("product details not updated correctly: %+v", dec.Product)
		}
		if dec.Change == nil || dec.Change.ChangeType != model.ChangeTypeUpdated {
			t.Fatalf("expected ChangeTypeUpdated, got %+v", dec.Change)
		}
		if dec.Delta.Updated != 1 {
			t.Fatalf("expected Updated delta = 1, got %d", dec.Delta.Updated)
		}
	})

	t.Run("Case 8: in.version > cur.version, in active, cur active, details identical -> Bump Version Only", func(t *testing.T) {
		cur := &model.Product{
			SupplierID:  "SUP-8",
			SKU:         "SKU-8",
			Name:        "Same Name",
			Description: "Same Desc",
			PriceCents:  1000,
			Currency:    "USD",
			Status:      model.ProductStatusActive,
			Version:     1,
		}
		in := model.SupplierItem{
			SupplierID:  "SUP-8",
			SKU:         "SKU-8",
			Name:        "Same Name",
			Description: "Same Desc",
			PriceCents:  1000,
			Currency:    "USD",
			Status:      model.ProductStatusActive,
			Version:     2,
		}

		dec := syncer.Merge(cur, in, runID, now)
		if dec.Type != syncer.DecisionBumpVersion {
			t.Fatalf("expected DecisionBumpVersion, got %v", dec.Type)
		}
		if dec.Product == nil || dec.Product.Version != 2 {
			t.Fatalf("expected product version bumped to 2, got %+v", dec.Product)
		}
		if dec.Change != nil {
			t.Fatal("expected no audit change recorded for empty bump")
		}
		if dec.Delta.Unchanged != 1 {
			t.Fatalf("expected Unchanged delta = 1, got %d", dec.Delta.Unchanged)
		}
	})

	t.Run("Case 9: in.version > cur.version, in active, cur withdrawn -> Relisted", func(t *testing.T) {
		withdrawnTime := now.Add(-24 * time.Hour)
		cur := &model.Product{
			SupplierID:  "SUP-9",
			SKU:         "SKU-9",
			Name:        "Old Name",
			PriceCents:  1000,
			Currency:    "USD",
			Status:      model.ProductStatusWithdrawn,
			Version:     2,
			WithdrawnAt: &withdrawnTime,
		}
		in := model.SupplierItem{
			SupplierID:  "SUP-9",
			SKU:         "SKU-9",
			Name:        "Relisted Name",
			PriceCents:  1200,
			Currency:    "USD",
			Status:      model.ProductStatusActive,
			Version:     3,
		}

		dec := syncer.Merge(cur, in, runID, now)
		if dec.Type != syncer.DecisionRelisted {
			t.Fatalf("expected DecisionRelisted, got %v", dec.Type)
		}
		if dec.Product == nil || dec.Product.Status != model.ProductStatusActive {
			t.Fatalf("expected active status on relist, got %+v", dec.Product)
		}
		if dec.Product.WithdrawnAt != nil {
			t.Fatal("expected WithdrawnAt to be cleared on relist")
		}
		if dec.Change == nil || dec.Change.ChangeType != model.ChangeTypeRelisted {
			t.Fatalf("expected ChangeTypeRelisted, got %+v", dec.Change)
		}
		if dec.Delta.Updated != 1 {
			t.Fatalf("expected Updated delta = 1, got %d", dec.Delta.Updated)
		}
	})

	t.Run("Invariant 3: Old active record cannot revive withdrawn product", func(t *testing.T) {
		withdrawnTime := now.Add(-time.Hour)
		cur := &model.Product{
			SupplierID:  "SUP-INV",
			Name:        "Dead Item",
			Status:      model.ProductStatusWithdrawn,
			Version:     5,
			WithdrawnAt: &withdrawnTime,
		}
		// Incoming old active record with version 4
		in := model.SupplierItem{
			SupplierID: "SUP-INV",
			Name:       "Old Active",
			PriceCents: 1000,
			Currency:   "USD",
			Status:     model.ProductStatusActive,
			Version:    4,
		}

		dec := syncer.Merge(cur, in, runID, now)
		if dec.Type != syncer.DecisionStale {
			t.Fatalf("expected DecisionStale, got %v", dec.Type)
		}
		if dec.Product != nil {
			t.Fatal("must not modify product")
		}
		if dec.Delta.Stale != 1 {
			t.Fatalf("expected Stale delta = 1, got %d", dec.Delta.Stale)
		}
	})
}

