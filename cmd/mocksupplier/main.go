package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"supplier-catalog-updates/internal/model"
)

type OutageManager struct {
	mu           sync.Mutex
	outageAfter  int
	outageFor    time.Duration
	batchCount   int
	outageActive bool
	outageStart  time.Time
}

func (om *OutageManager) checkOutage() bool {
	om.mu.Lock()
	defer om.mu.Unlock()

	if om.outageAfter <= 0 {
		return false
	}

	if om.outageActive {
		if time.Since(om.outageStart) > om.outageFor {
			om.outageActive = false
			return false
		}
		return true
	}

	if om.batchCount >= om.outageAfter {
		om.outageActive = true
		om.outageStart = time.Now()
		return true
	}
	return false
}

func (om *OutageManager) incBatch() {
	om.mu.Lock()
	defer om.mu.Unlock()
	om.batchCount++
}

func (om *OutageManager) recoverNow() {
	om.mu.Lock()
	defer om.mu.Unlock()
	om.outageActive = false
	om.outageAfter = 0
}

func generateBaseItems() []model.SupplierItem {
	var items []model.SupplierItem
	// 120 products
	for i := 1; i <= 120; i++ {
		supID := fmt.Sprintf("SUP-%03d", i)
		sku := fmt.Sprintf("SKU-%04d", i)

		// Base v1
		items = append(items, model.SupplierItem{
			SupplierID:  supID,
			SKU:         sku,
			Name:        fmt.Sprintf("Product %03d", i),
			Description: fmt.Sprintf("High quality product #%d from supplier", i),
			PriceCents:  int64(i*100 + 99),
			Currency:    "USD",
			Status:      model.ProductStatusActive,
			Version:     1,
		})

		// Every 5th product is updated to v2
		if i%5 == 0 {
			items = append(items, model.SupplierItem{
				SupplierID:  supID,
				SKU:         sku,
				Name:        fmt.Sprintf("Product %03d (v2 Enhanced)", i),
				Description: fmt.Sprintf("Updated description for product #%d", i),
				PriceCents:  int64(i*120 + 50),
				Currency:    "USD",
				Status:      model.ProductStatusActive,
				Version:     2,
			})
		}

		// Every 17th product is withdrawn at v3
		if i%17 == 0 {
			items = append(items, model.SupplierItem{
				SupplierID:  supID,
				SKU:         sku,
				Name:        fmt.Sprintf("Product %03d (Withdrawn)", i),
				Description: "Discontinued",
				PriceCents:  int64(i * 100),
				Currency:    "USD",
				Status:      model.ProductStatusWithdrawn,
				Version:     3,
			})
		}
	}
	return items
}

func applyScenarios(items []model.SupplierItem, scenariosStr string) []model.SupplierItem {
	scenarios := strings.Split(scenariosStr, ",")
	isAll := false
	hasScenario := func(s string) bool {
		if isAll {
			return true
		}
		for _, sc := range scenarios {
			if strings.TrimSpace(sc) == s {
				return true
			}
		}
		return false
	}
	for _, sc := range scenarios {
		if strings.TrimSpace(sc) == "all" {
			isAll = true
			break
		}
	}

	result := make([]model.SupplierItem, len(items))
	copy(result, items)

	// Invalids
	if hasScenario("invalid") {
		result = append(result,
			model.SupplierItem{SupplierID: "", Name: "Missing ID", PriceCents: 1000, Currency: "USD", Status: model.ProductStatusActive, Version: 1},
			model.SupplierItem{SupplierID: "SUP-NEG", Name: "Negative Price", PriceCents: -500, Currency: "USD", Status: model.ProductStatusActive, Version: 1},
			model.SupplierItem{SupplierID: "SUP-BADST", Name: "Bad Status", PriceCents: 1000, Currency: "USD", Status: "destroyed", Version: 1},
			model.SupplierItem{SupplierID: "SUP-NONAME", Name: "", PriceCents: 1000, Currency: "USD", Status: model.ProductStatusActive, Version: 1},
		)
	}

	// Conflicts: Same version but different details
	if hasScenario("conflict") {
		result = append(result,
			model.SupplierItem{
				SupplierID:  "SUP-001",
				SKU:         "SKU-0001",
				Name:        "Product 001 - Contradictory Version 1",
				Description: "Conflicting duplicate with version 1",
				PriceCents:  99999,
				Currency:    "USD",
				Status:      model.ProductStatusActive,
				Version:     1,
			},
		)
	}

	// Duplicates: Duplicate several items
	if hasScenario("duplicates") {
		if len(result) > 10 {
			dups := append([]model.SupplierItem{}, result[:10]...)
			result = append(result, dups...)
		}
	}

	// Out of order: Shuffle items randomly
	if hasScenario("outoforder") {
		r := rand.New(rand.NewSource(time.Now().UnixNano()))
		r.Shuffle(len(result), func(i, j int) {
			result[i], result[j] = result[j], result[i]
		})
	}

	return result
}

func main() {
	port := flag.Int("port", 8080, "Port for mock supplier HTTP server")
	scenario := flag.String("scenario", "normal", "Scenarios: normal, outoforder, duplicates, invalid, conflict, or 'all'")
	outageAfter := flag.Int("outage-after", 0, "Trigger 503 outage after N batches (0 to disable)")
	outageFor := flag.Duration("outage-for", 5*time.Minute, "Outage duration before automatic recovery")
	flag.Parse()

	items := generateBaseItems()
	items = applyScenarios(items, *scenario)

	outage := &OutageManager{
		outageAfter: *outageAfter,
		outageFor:   *outageFor,
	}

	mux := http.NewServeMux()

	// GET /v1/products?cursor=&limit=
	mux.HandleFunc("/v1/products", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		if outage.checkOutage() {
			http.Error(w, "Service Unavailable (simulated outage)", http.StatusServiceUnavailable)
			return
		}

		cursorStr := r.URL.Query().Get("cursor")
		limitStr := r.URL.Query().Get("limit")

		start := 0
		if cursorStr != "" {
			parsed, err := strconv.Atoi(cursorStr)
			if err != nil || parsed < 0 || parsed > len(items) {
				http.Error(w, "Invalid cursor", http.StatusBadRequest)
				return
			}
			start = parsed
		}

		limit := 20
		if limitStr != "" {
			if parsed, err := strconv.Atoi(limitStr); err == nil && parsed > 0 {
				limit = parsed
			}
		}

		end := start + limit
		done := false
		nextCursor := ""

		if end >= len(items) {
			end = len(items)
			done = true
			nextCursor = ""
		} else {
			nextCursor = strconv.Itoa(end)
		}

		batchItems := items[start:end]
		outage.incBatch()

		resp := model.BatchResponse{
			Items:      batchItems,
			NextCursor: nextCursor,
			Done:       done,
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	// POST /admin/recover
	mux.HandleFunc("/admin/recover", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		outage.recoverNow()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status":  "recovered",
			"message": "outage cleared immediately",
		})
	})

	addr := fmt.Sprintf(":%d", *port)
	log.Printf("Mock Supplier running on http://localhost%s (items: %d, scenario: %s, outageAfter: %d)",
		addr, len(items), *scenario, *outageAfter)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}

