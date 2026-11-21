package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	maxResponseBytes = 8 << 20
	maxErrorBytes    = 512
)

type httpClient struct {
	client   *http.Client
	endpoint string
	key      string
	sleep    func(context.Context, time.Duration) error
}

func newHTTPClient(endpoint, key string, local bool) *httpClient {
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	dial := dialer.DialContext
	if local {
		dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil || !strings.EqualFold(host, "localhost") {
				return dialer.DialContext(ctx, network, addr)
			}
			// Resolve localhost ourselves so a local endpoint cannot dial a non-loopback address.
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort("127.0.0.1", port))
			if err == nil {
				return conn, nil
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort("::1", port))
		}
	}
	return &httpClient{
		endpoint: endpoint,
		key:      key,
		sleep:    sleepContext,
		client: &http.Client{
			Transport: &http.Transport{
				DialContext:         dial,
				ForceAttemptHTTP2:   true,
				IdleConnTimeout:     90 * time.Second,
				TLSHandshakeTimeout: 10 * time.Second,
			},
			// A redirect can change the destination whose billing policy was checked.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

func (h *httpClient) post(ctx context.Context, payload, result any, headers http.Header) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode model request: %s", h.redact(err.Error()))
	}
	for attempt := 0; ; attempt++ {
		retry, after, err := h.send(ctx, body, result, headers)
		if !retry || attempt == 2 {
			return err
		}
		if err := h.sleep(ctx, retryDelay(after, attempt)); err != nil {
			return err
		}
	}
}

func (h *httpClient) send(ctx context.Context, body []byte, result any, headers http.Header) (bool, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.endpoint, bytes.NewReader(body))
	if err != nil {
		return false, "", fmt.Errorf("create model request: %s", h.redact(err.Error()))
	}
	req.Header = headers.Clone()
	req.Header.Set("Content-Type", "application/json")
	res, err := h.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return false, "", ctx.Err()
		}
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		var netErr net.Error
		retry := errors.As(err, &netErr) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
		return retry, "", fmt.Errorf("send model request: %s", h.redact(err.Error()))
	}
	defer res.Body.Close()

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		// Read enough to redact a credential starting at the edge of the excerpt.
		limit := int64(maxErrorBytes) + int64(len(h.key))
		excerpt, _ := io.ReadAll(io.LimitReader(res.Body, min(limit, maxResponseBytes)))
		message := h.redact(string(excerpt))
		if len(message) > maxErrorBytes {
			message = message[:maxErrorBytes]
		}
		status := fmt.Errorf("model request returned HTTP %d: %s", res.StatusCode, strings.TrimSpace(message))
		retry := res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500 && res.StatusCode <= 599
		return retry, res.Header.Get("Retry-After"), status
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return false, "", ctx.Err()
		}
		return true, "", fmt.Errorf("read model response (HTTP %d): %s", res.StatusCode, h.redact(err.Error()))
	}
	if len(data) > maxResponseBytes {
		return false, "", fmt.Errorf("model response (HTTP %d) exceeds 8 MiB", res.StatusCode)
	}
	if err := json.Unmarshal(data, result); err != nil {
		return false, "", fmt.Errorf("decode model response (HTTP %d): %s", res.StatusCode, h.redact(err.Error()))
	}
	return false, "", nil
}

func (h *httpClient) redact(text string) string {
	if h.key == "" {
		return text
	}
	return strings.ReplaceAll(text, h.key, "[redacted]")
}

func retryDelay(after string, attempt int) time.Duration {
	seconds, err := strconv.ParseUint(strings.TrimSpace(after), 10, 64)
	if err == nil || errors.Is(err, strconv.ErrRange) {
		return time.Duration(min(seconds, 30)) * time.Second
	}
	return (250 * time.Millisecond) << attempt
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
