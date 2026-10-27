package syncer

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"supplier-catalog-updates/internal/model"
)

// DecisionType represents the outcome of merging an item against current state.
type DecisionType string

const (
	DecisionCreated     DecisionType = "created"
	DecisionTombstone   DecisionType = "tombstone"
	DecisionStale       DecisionType = "stale"
	DecisionUnchanged   DecisionType = "unchanged"
	DecisionConflict    DecisionType = "version_conflict"
	DecisionUpdated     DecisionType = "updated"
	DecisionBumpVersion DecisionType = "bump_version"
	DecisionRelisted    DecisionType = "relisted"
)

// Decision bundles the computed DB mutation, audit change, problem, and counter delta.
type Decision struct {
	Type    DecisionType
	Product *model.Product
	Change  *model.ProductChange
	Problem *model.Problem
	Delta   model.BatchDelta
}

// Validate checks basic syntax and business constraints of a supplier item.
func Validate(in model.SupplierItem) error {
	if strings.TrimSpace(in.SupplierID) == "" {
		return errors.New("missing supplier_id")
	}
	if in.Version <= 0 {
		return errors.New("version must be positive integer (> 0)")
	}
	if in.Status != model.ProductStatusActive && in.Status != model.ProductStatusWithdrawn {
		return fmt.Errorf("invalid status: %q (must be 'active' or 'withdrawn')", in.Status)
	}

	if in.Status == model.ProductStatusActive {
		if strings.TrimSpace(in.Name) == "" {
			return errors.New("missing name for active product")
		}
		if strings.TrimSpace(in.Currency) == "" {
			return errors.New("missing currency for active product")
		}
		if in.PriceCents < 0 {
			return errors.New("price_cents cannot be negative for active product")
		}
	}

	return nil
}

// FormatPrice formats cents and currency as "10.50 USD".
func FormatPrice(cents int64, currency string) string {
	sign := ""
	if cents < 0 {
		sign = "-"
		cents = -cents
	}
	return fmt.Sprintf("%s%d.%02d %s", sign, cents/100, cents%100, currency)
}

// itemsHaveSameContent compares content between current product and incoming item.
func itemsHaveSameContent(cur *model.Product, in model.SupplierItem) bool {
	if cur.Status != in.Status {
		return false
	}
	if cur.Status == model.ProductStatusWithdrawn {
		// As per spec: withdrawn items only need id + version. Details are for reference.
		return true
	}
	return cur.SKU == in.SKU &&
		cur.Name == in.Name &&
		cur.Description == in.Description &&
		cur.PriceCents == in.PriceCents &&
		cur.Currency == in.Currency
}

// diffDetails builds a human-readable summary of differences between cur and in.
func diffDetails(cur *model.Product, in model.SupplierItem) string {
	var diffs []string
	if cur.Name != in.Name {
		diffs = append(diffs, fmt.Sprintf("name %q -> %q", cur.Name, in.Name))
	}
	if cur.SKU != in.SKU {
		diffs = append(diffs, fmt.Sprintf("sku %q -> %q", cur.SKU, in.SKU))
	}
	if cur.PriceCents != in.PriceCents || cur.Currency != in.Currency {
		diffs = append(diffs, fmt.Sprintf("price %s -> %s",
			FormatPrice(cur.PriceCents, cur.Currency),
			FormatPrice(in.PriceCents, in.Currency),
		))
	}
	if cur.Description != in.Description {
		diffs = append(diffs, "description updated")
	}
	if len(diffs) == 0 {
		return "details updated"
	}
	return strings.Join(diffs, ", ")
}

// Merge is a pure function that compares the incoming supplier item with current DB state
// and returns the exact Decision (mutations, changes, problems, and counters).
func Merge(cur *model.Product, in model.SupplierItem, runID string, now time.Time) Decision {
	// Case 1 & 2: Product does not exist yet
	if cur == nil {
		if in.Status == model.ProductStatusActive {
			prod := &model.Product{
				SupplierID:   in.SupplierID,
				SKU:          in.SKU,
				Name:         in.Name,
				Description:  in.Description,
				PriceCents:   in.PriceCents,
				Currency:     in.Currency,
				Status:       model.ProductStatusActive,
				Version:      in.Version,
				UpdatedInRun: runID,
			}
			return Decision{
				Type:    DecisionCreated,
				Product: prod,
				Change: &model.ProductChange{
					RunID:      runID,
					SupplierID: in.SupplierID,
					ChangeType: model.ChangeTypeCreated,
					Summary:    fmt.Sprintf("created %s (%s)", in.Name, FormatPrice(in.PriceCents, in.Currency)),
					CreatedAt:  now,
				},
				Delta: model.BatchDelta{Created: 1},
			}
		}

		// Case 2: in.Status == withdrawn (initial tombstone)
		prod := &model.Product{
			SupplierID:   in.SupplierID,
			SKU:          in.SKU,
			Name:         in.Name,
			Description:  in.Description,
			PriceCents:   in.PriceCents,
			Currency:     in.Currency,
			Status:       model.ProductStatusWithdrawn,
			Version:      in.Version,
			WithdrawnAt:  &now,
			UpdatedInRun: runID,
		}
		return Decision{
			Type:    DecisionTombstone,
			Product: prod,
			Change: &model.ProductChange{
				RunID:      runID,
				SupplierID: in.SupplierID,
				ChangeType: model.ChangeTypeWithdrawn,
				Summary:    fmt.Sprintf("created tombstone at v%d", in.Version),
				CreatedAt:  now,
			},
			Delta: model.BatchDelta{Withdrawn: 1},
		}
	}

	// Case 3: Stale record (in.version < cur.version)
	if in.Version < cur.Version {
		return Decision{
			Type:  DecisionStale,
			Delta: model.BatchDelta{Stale: 1},
		}
	}

	// Case 4 & 5: Same version
	if in.Version == cur.Version {
		if itemsHaveSameContent(cur, in) {
			// Case 4: Identical data at same version -> unchanged
			return Decision{
				Type:  DecisionUnchanged,
				Delta: model.BatchDelta{Unchanged: 1},
			}
		}

		// Case 5: Conflicting data at same version
		rawBytes, _ := json.Marshal(in)
		diffSummary := diffDetails(cur, in)
		return Decision{
			Type: DecisionConflict,
			Problem: &model.Problem{
				ID:         uuid.NewString(),
				RunID:      runID,
				SupplierID: in.SupplierID,
				Kind:       model.ProblemKindVersionConflict,
				Message: fmt.Sprintf("conflicting content at version %d (current: status=%s vs incoming: status=%s; diff: %s)",
					cur.Version, cur.Status, in.Status, diffSummary),
				RawJSON:   string(rawBytes),
				Resolved:  false,
				CreatedAt: now,
			},
			Delta: model.BatchDelta{Problem: 1},
		}
	}

	// From here: in.Version > cur.Version

	// Case 6: Becoming withdrawn
	if in.Status == model.ProductStatusWithdrawn {
		withdrawnAt := now
		if cur.WithdrawnAt != nil {
			withdrawnAt = *cur.WithdrawnAt
		}
		prod := &model.Product{
			SupplierID:   in.SupplierID,
			SKU:          cur.SKU,
			Name:         cur.Name,
			Description:  cur.Description,
			PriceCents:   cur.PriceCents,
			Currency:     cur.Currency,
			Status:       model.ProductStatusWithdrawn,
			Version:      in.Version,
			WithdrawnAt:  &withdrawnAt,
			UpdatedInRun: runID,
		}
		return Decision{
			Type:    DecisionUpdated,
			Product: prod,
			Change: &model.ProductChange{
				RunID:      runID,
				SupplierID: in.SupplierID,
				ChangeType: model.ChangeTypeWithdrawn,
				Summary:    fmt.Sprintf("withdrawn at v%d", in.Version),
				CreatedAt:  now,
			},
			Delta: model.BatchDelta{Withdrawn: 1},
		}
	}

	// Incoming status is active

	// Case 9: Was withdrawn, now relisted (in.Version > cur.Version && in.Status == active)
	if cur.Status == model.ProductStatusWithdrawn {
		prod := &model.Product{
			SupplierID:   in.SupplierID,
			SKU:          in.SKU,
			Name:         in.Name,
			Description:  in.Description,
			PriceCents:   in.PriceCents,
			Currency:     in.Currency,
			Status:       model.ProductStatusActive,
			Version:      in.Version,
			WithdrawnAt:  nil,
			UpdatedInRun: runID,
		}
		return Decision{
			Type:    DecisionRelisted,
			Product: prod,
			Change: &model.ProductChange{
				RunID:      runID,
				SupplierID: in.SupplierID,
				ChangeType: model.ChangeTypeRelisted,
				Summary:    fmt.Sprintf("relisted active at v%d (%s, %s)", in.Version, in.Name, FormatPrice(in.PriceCents, in.Currency)),
				CreatedAt:  now,
			},
			Delta: model.BatchDelta{Updated: 1},
		}
	}

	// Both cur and in are active (cur.Status == active, in.Status == active)
	if itemsHaveSameContent(cur, in) {
		// Case 8: Details same, bump version only
		prod := &model.Product{
			SupplierID:   cur.SupplierID,
			SKU:          cur.SKU,
			Name:         cur.Name,
			Description:  cur.Description,
			PriceCents:   cur.PriceCents,
			Currency:     cur.Currency,
			Status:       model.ProductStatusActive,
			Version:      in.Version,
			WithdrawnAt:  nil,
			UpdatedInRun: runID,
		}
		return Decision{
			Type:    DecisionBumpVersion,
			Product: prod,
			Change:  nil, // No change log needed for empty version bump
			Delta:   model.BatchDelta{Unchanged: 1},
		}
	}

	// Case 7: Details changed, update product
	diff := diffDetails(cur, in)
	prod := &model.Product{
		SupplierID:   in.SupplierID,
		SKU:          in.SKU,
		Name:         in.Name,
		Description:  in.Description,
		PriceCents:   in.PriceCents,
		Currency:     in.Currency,
		Status:       model.ProductStatusActive,
		Version:      in.Version,
		WithdrawnAt:  nil,
		UpdatedInRun: runID,
	}
	return Decision{
		Type:    DecisionUpdated,
		Product: prod,
		Change: &model.ProductChange{
			RunID:      runID,
			SupplierID: in.SupplierID,
			ChangeType: model.ChangeTypeUpdated,
			Summary:    diff,
			CreatedAt:  now,
		},
		Delta: model.BatchDelta{Updated: 1},
	}
}

