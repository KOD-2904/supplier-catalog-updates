package supplier_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"supplier-catalog-updates/internal/supplier"
)

func TestHTTPClient_FetchBatch(t *testing.T) {
	t.Run("success response", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/products" {
				t.Fatalf("unexpected path: %s", r.URL.Path)
			}
			if r.URL.Query().Get("cursor") != "cur-1" {
				t.Fatalf("unexpected cursor: %s", r.URL.Query().Get("cursor"))
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{
				"items": [
					{"supplier_id": "P1", "name": "Item 1", "price_cents": 100, "currency": "USD", "status": "active", "version": 1}
				],
				"next_cursor": "cur-2",
				"done": false
			}`))
		}))
		defer ts.Close()

		client := supplier.NewHTTPClient(ts.URL, 2*time.Second)
		resp, err := client.FetchBatch(context.Background(), "cur-1", 10)
		if err != nil {
			t.Fatalf("expected success, got %v", err)
		}
		if len(resp.Items) != 1 || resp.NextCursor != "cur-2" || resp.Done {
			t.Fatalf("unexpected response content: %+v", resp)
		}
	})

	t.Run("transient 503 error", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("outage"))
		}))
		defer ts.Close()

		client := supplier.NewHTTPClient(ts.URL, 2*time.Second)
		_, err := client.FetchBatch(context.Background(), "", 10)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !supplier.IsTransient(err) {
			t.Fatalf("expected transient error, got %v", err)
		}
	})

	t.Run("transient 429 error", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte("rate limit exceeded"))
		}))
		defer ts.Close()

		client := supplier.NewHTTPClient(ts.URL, 2*time.Second)
		_, err := client.FetchBatch(context.Background(), "", 10)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !supplier.IsTransient(err) {
			t.Fatalf("expected transient error, got %v", err)
		}
	})

	t.Run("permanent 400 error", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("bad request"))
		}))
		defer ts.Close()

		client := supplier.NewHTTPClient(ts.URL, 2*time.Second)
		_, err := client.FetchBatch(context.Background(), "", 10)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if supplier.IsTransient(err) {
			t.Fatalf("expected permanent error, got transient %v", err)
		}
	})
}

