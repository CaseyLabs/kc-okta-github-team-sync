package util

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	defaultMaxAttempts = 4
	defaultBaseDelay   = 500 * time.Millisecond
	// Windows 11 user agent, ref: https://www.whatismybrowser.com/guides/the-latest-user-agent/windows
	userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"
)

var (
	httpDebugEnabled atomic.Bool
	httpDebugLogger  atomic.Value
)

// EnableHTTPDebug turns on HTTP debug logging using the provided logger.
func EnableHTTPDebug(logger *slog.Logger) {
	if logger == nil {
		return
	}
	httpDebugLogger.Store(logger)
	httpDebugEnabled.Store(true)
}

// HTTPDebugEnabled reports whether HTTP debug logging is active.
func HTTPDebugEnabled() bool {
	return httpDebugEnabled.Load()
}

func logHTTPDebug(msg string, args ...any) {
	if !HTTPDebugEnabled() {
		return
	}
	if logger, ok := httpDebugLogger.Load().(*slog.Logger); ok && logger != nil {
		logger.Debug(msg, args...)
	}
}

func summarizeBody(body []byte, limit int) string {
	if limit <= 0 {
		limit = 2048
	}
	if len(body) <= limit {
		return strings.TrimSpace(string(body))
	}
	return strings.TrimSpace(string(body[:limit])) + fmt.Sprintf("… (%d bytes truncated)", len(body)-limit)
}

// LogHTTPResponseBody emits a debug log entry for the provided response payload.
func LogHTTPResponseBody(method, url string, status int, body []byte) {
	logHTTPDebug("http_response_body",
		"method", method,
		"url", url,
		"status", status,
		"body", summarizeBody(body, 2048),
	)
}

// HTTPError wraps a non-successful HTTP response with the status code and body snippet.
type HTTPError struct {
	StatusCode int
	Body       string
	URL        string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("request to %s failed with status %d: %s", e.URL, e.StatusCode, e.Body)
}

// BuildJSONRequest marshals the body (if provided) and constructs an HTTP request with JSON headers.
func BuildJSONRequest(ctx context.Context, method, rawURL string, body any) (*http.Request, error) {
	var reader io.Reader
	var payload []byte
	var err error

	if body != nil {
		payload, err = json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request body: %w", err)
		}
		reader = bytes.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(ctx, method, rawURL, reader)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
		copyPayload := append([]byte(nil), payload...)
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(copyPayload)), nil
		}
	}

	req.Header.Set("Accept", "application/json")
	if _, ok := req.Header["User-Agent"]; !ok {
		req.Header.Set("User-Agent", userAgent)
	}

	return req, nil
}

// DoJSON executes the request with retry semantics and decodes the JSON response into v.
func DoJSON(ctx context.Context, client *http.Client, req *http.Request, v any) error {
	resp, err := DoRaw(ctx, client, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := readBody(resp.Body, 4096)
		if HTTPDebugEnabled() {
			LogHTTPResponseBody(req.Method, req.URL.String(), resp.StatusCode, []byte(body))
		}
		return &HTTPError{StatusCode: resp.StatusCode, Body: body, URL: req.URL.String()}
	}

	if HTTPDebugEnabled() {
		payload, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("read response from %s: %w", req.URL.String(), err)
		}
		LogHTTPResponseBody(req.Method, req.URL.String(), resp.StatusCode, payload)
		if v == nil {
			return nil
		}
		if len(payload) == 0 {
			return nil
		}
		if err := json.Unmarshal(payload, v); err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("decode response from %s: %w", req.URL.String(), err)
		}
		return nil
	}

	if v == nil {
		// Caller is not interested in the payload.
		io.Copy(io.Discard, resp.Body)
		return nil
	}

	if err := json.NewDecoder(resp.Body).Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("decode response from %s: %w", req.URL.String(), err)
	}

	return nil
}

// DoRaw executes the request with retry semantics and returns the raw response.
// The caller is responsible for closing the response body.
func DoRaw(ctx context.Context, client *http.Client, req *http.Request) (*http.Response, error) {
	if client == nil {
		client = http.DefaultClient
	}

	var lastErr error
	baseDelay := defaultBaseDelay

	for attempt := 1; attempt <= defaultMaxAttempts; attempt++ {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}

		reqAttempt, err := cloneRequest(ctx, req)
		if err != nil {
			return nil, err
		}

		logHTTPDebug("http_request", "method", reqAttempt.Method, "url", reqAttempt.URL.String(), "attempt", attempt)

		resp, err := client.Do(reqAttempt)
		if err != nil {
			logHTTPDebug("http_request_error", "method", reqAttempt.Method, "url", reqAttempt.URL.String(), "attempt", attempt, "error", err)
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			lastErr = err
		} else {
			logHTTPDebug("http_response", "method", reqAttempt.Method, "url", reqAttempt.URL.String(), "status", resp.StatusCode, "attempt", attempt)
			delay, shouldRetry := retryDelay(resp, attempt, baseDelay)
			if !shouldRetry {
				return resp, nil
			}

			// Consume and close body before retrying.
			resp.Body.Close()

			if attempt == defaultMaxAttempts {
				return nil, &HTTPError{StatusCode: resp.StatusCode, Body: "retry attempts exhausted", URL: req.URL.String()}
			}

			if err := wait(ctx, delay); err != nil {
				return nil, err
			}
			continue
		}

		if attempt == defaultMaxAttempts {
			if lastErr != nil {
				return nil, fmt.Errorf("max retries reached: %w", lastErr)
			}
			return nil, fmt.Errorf("max retries reached for %s", req.URL.String())
		}

		if err := wait(ctx, backoffDelay(baseDelay, attempt)); err != nil {
			return nil, err
		}
	}

	if lastErr != nil {
		return nil, lastErr
	}

	return nil, fmt.Errorf("request to %s failed without response", req.URL.String())
}

func wait(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}

	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func cloneRequest(ctx context.Context, req *http.Request) (*http.Request, error) {
	req2 := req.Clone(ctx)

	if req.Body == nil || req.Body == http.NoBody {
		return req2, nil
	}

	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return nil, fmt.Errorf("clone request body: %w", err)
		}
		req2.Body = body
		return req2, nil
	}

	// Fall back to consuming the existing body once and caching it for future use.
	payload, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, fmt.Errorf("read request body: %w", err)
	}
	_ = req.Body.Close()

	copyPayload := append([]byte(nil), payload...)
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(copyPayload)), nil
	}
	req.Body = io.NopCloser(bytes.NewReader(payload))

	body, err := req.GetBody()
	if err != nil {
		return nil, fmt.Errorf("clone regenerated body: %w", err)
	}
	req2.Body = body

	return req2, nil
}

func retryDelay(resp *http.Response, attempt int, baseDelay time.Duration) (time.Duration, bool) {
	switch resp.StatusCode {
	case http.StatusTooManyRequests, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return retryAfter(resp, baseDelay), true
	case http.StatusForbidden:
		if hasRetryAfter(resp) {
			return retryAfter(resp, baseDelay), true
		}
	}

	if resp.StatusCode >= 500 {
		return backoffDelay(baseDelay, attempt), true
	}

	return 0, false
}

func retryAfter(resp *http.Response, baseDelay time.Duration) time.Duration {
	header := resp.Header.Get("Retry-After")
	if header != "" {
		if secs, err := strconv.Atoi(header); err == nil {
			return time.Duration(secs) * time.Second
		}
		if when, err := http.ParseTime(header); err == nil {
			d := time.Until(when)
			if d > 0 {
				return d
			}
		}
	}

	if reset := resp.Header.Get("X-RateLimit-Reset"); reset != "" {
		if epoch, err := strconv.ParseInt(reset, 10, 64); err == nil {
			d := time.Until(time.Unix(epoch, 0))
			if d > 0 {
				return d
			}
		}
	}

	return baseDelay
}

func hasRetryAfter(resp *http.Response) bool {
	if resp.Header.Get("Retry-After") != "" {
		return true
	}
	if resp.Header.Get("X-RateLimit-Reset") != "" {
		return true
	}
	return false
}

func backoffDelay(base time.Duration, attempt int) time.Duration {
	if attempt <= 1 {
		return base
	}
	d := base * time.Duration(1<<uint(attempt-1))
	max := 30 * time.Second
	if d > max {
		return max
	}
	return d
}

func readBody(r io.Reader, limit int64) (string, error) {
	if limit <= 0 {
		limit = 4096
	}
	buf := &bytes.Buffer{}
	if _, err := io.CopyN(buf, r, limit); err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(buf.String()), nil
}
