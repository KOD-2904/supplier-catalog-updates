package supplier

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"supplier-catalog-updates/internal/model"
)

// Client is the interface used by the syncer to fetch catalog batches.
type Client interface {
	FetchBatch(ctx context.Context, cursor string, limit int) (*model.BatchResponse, error)
}

// TransientError indicates a temporary failure (5xx, 429, network timeout) that can be retried.
type TransientError struct {
	StatusCode int
	Message    string
	Err        error
}

func (e *TransientError) Error() string {
	if e.StatusCode > 0 {
		return fmt.Sprintf("transient HTTP error (%d): %s", e.StatusCode, e.Message)
	}
	if e.Err != nil {
		return fmt.Sprintf("transient network error: %v", e.Err)
	}
	return e.Message
}

func (e *TransientError) Unwrap() error {
	return e.Err
}

// PermanentError indicates a non-retryable fatal error (e.g. 400, 401, 404, invalid payload).
type PermanentError struct {
	StatusCode int
	Message    string
	Err        error
}

func (e *PermanentError) Error() string {
	if e.StatusCode > 0 {
		return fmt.Sprintf("permanent HTTP error (%d): %s", e.StatusCode, e.Message)
	}
	if e.Err != nil {
		return fmt.Sprintf("permanent error: %v", e.Err)
	}
	return e.Message
}

func (e *PermanentError) Unwrap() error {
	return e.Err
}

// IsTransient checks if an error is categorized as transient.
func IsTransient(err error) bool {
	var te *TransientError
	if errors.As(err, &te) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	return false
}

// HTTPClient implements Client against a real or mock HTTP supplier endpoint.
type HTTPClient struct {
	baseURL    string
	httpClient *http.Client
}

// NewHTTPClient creates an HTTP supplier client.
func NewHTTPClient(baseURL string, timeout time.Duration) *HTTPClient {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &HTTPClient{
		baseURL: stringsTrimRight(baseURL, "/"),
		httpClient: &http.Client{
			Timeout: timeout,
		},
	}
}

func stringsTrimRight(s, cutset string) string {
	for len(s) > 0 && len(cutset) > 0 && s[len(s)-1] == cutset[0] {
		s = s[:len(s)-1]
	}
	return s
}

// FetchBatch fetches a single batch of products using cursor pagination.
func (c *HTTPClient) FetchBatch(ctx context.Context, cursor string, limit int) (*model.BatchResponse, error) {
	reqURL, err := url.Parse(c.baseURL + "/v1/products")
	if err != nil {
		return nil, &PermanentError{Err: fmt.Errorf("invalid base URL: %w", err)}
	}

	q := reqURL.Query()
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	reqURL.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL.String(), nil)
	if err != nil {
		return nil, &PermanentError{Err: fmt.Errorf("failed to create request: %w", err)}
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, ctx.Err()
		}
		return nil, &TransientError{Err: err}
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &TransientError{Err: fmt.Errorf("failed to read response body: %w", err)}
	}

	// Categorize status codes
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return nil, &TransientError{
			StatusCode: resp.StatusCode,
			Message:    string(bodyBytes),
		}
	}
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		return nil, &PermanentError{
			StatusCode: resp.StatusCode,
			Message:    string(bodyBytes),
		}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &PermanentError{
			StatusCode: resp.StatusCode,
			Message:    fmt.Sprintf("unexpected status code: %d", resp.StatusCode),
		}
	}

	var batchResp model.BatchResponse
	if err := json.Unmarshal(bodyBytes, &batchResp); err != nil {
		return nil, &PermanentError{
			Err: fmt.Errorf("failed to unmarshal JSON response: %w", err),
		}
	}

	return &batchResp, nil
}

