package provider

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestProviderHTTPRetryAndErrors(t *testing.T) {
	for _, kind := range []string{"anthropic", "openai"} {
		t.Run(kind, func(t *testing.T) {
			const fakeKey = "test-credential"
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if kind == "anthropic" && r.Header.Get("x-api-key") != fakeKey ||
					kind == "openai" && r.Header.Get("Authorization") != "Bearer "+fakeKey {
					t.Error("missing configured authentication")
				}
				switch requests.Add(1) {
				case 1:
					w.Header().Set("Retry-After", "400")
					w.WriteHeader(http.StatusTooManyRequests)
				case 2:
					if kind == "anthropic" {
						_, _ = w.Write([]byte(`{"content":[],"stop_reason":"max_tokens"}`))
					} else {
						_, _ = w.Write([]byte(`{"choices":[{"message":{},"finish_reason":"length"}]}`))
					}
				default:
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte("upstream error " + fakeKey + " " + strings.Repeat("x", 512) + " outside the excerpt"))
				}
			}))
			defer server.Close()

			p, err := New(Config{Kind: kind, BaseURL: server.URL, APIKeyEnv: "ADA_KEY"}, false, func(string) string { return fakeKey })
			if err != nil {
				t.Fatal(err)
			}
			var client *httpClient
			switch p := p.(type) {
			case *anthropicProvider:
				client = p.http
			case *openAIProvider:
				client = p.http
			}
			var waits []time.Duration
			client.sleep = func(_ context.Context, delay time.Duration) error {
				waits = append(waits, delay)
				return nil
			}
			result, err := p.Complete(t.Context(), Request{})
			if err != nil || result.StopReason != "max_tokens" || len(waits) != 1 || waits[0] != 30*time.Second {
				t.Fatalf("retry result = %+v, error = %v, waits = %v", result, err, waits)
			}
			_, err = p.Complete(t.Context(), Request{})
			if err == nil || !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "upstream error") || strings.Contains(err.Error(), fakeKey) || strings.Contains(err.Error(), "outside the excerpt") {
				t.Fatalf("unexpected error: %v", err)
			}
			if requests.Load() != 3 {
				t.Fatalf("requests = %d", requests.Load())
			}
		})
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestProviderHTTPConnectionRetries(t *testing.T) {
	p, err := New(Config{Kind: "anthropic", BaseURL: "http://localhost"}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := p.(*anthropicProvider).http
	calls := 0
	client.client.Transport = roundTripperFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, &net.OpError{Op: "dial", Err: errors.New("offline")}
	})
	client.sleep = func(context.Context, time.Duration) error { return nil }
	if _, err := p.Complete(t.Context(), Request{}); err == nil || calls != 3 {
		t.Fatalf("Complete = %v, calls = %d", err, calls)
	}
}

func TestProviderHTTPRedirectRefused(t *testing.T) {
	var reached atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Store(true) }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	p, err := New(Config{Kind: "anthropic", BaseURL: server.URL}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Complete(t.Context(), Request{}); err == nil || !strings.Contains(err.Error(), "307") || reached.Load() {
		t.Fatalf("Complete = %v, redirect target reached = %v", err, reached.Load())
	}
}

func TestProviderHTTPResponseLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat(" ", maxResponseBytes+1))
	}))
	defer server.Close()
	p, err := New(Config{Kind: "openai", BaseURL: server.URL}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Complete(t.Context(), Request{}); err == nil || !strings.Contains(err.Error(), "exceeds 8 MiB") {
		t.Fatalf("Complete = %v", err)
	}
}

func TestProviderHTTPCancellation(t *testing.T) {
	received := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(received)
		<-r.Context().Done()
	}))
	defer server.Close()
	p, err := New(Config{Kind: "anthropic", BaseURL: server.URL}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	go func() {
		select {
		case <-received:
			cancel()
		case <-ctx.Done():
		}
	}()
	if _, err := p.Complete(ctx, Request{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Complete = %v", err)
	}
}

func TestProviderHTTPServerErrorRetries(t *testing.T) {
	for _, tc := range []struct {
		name      string
		successAt int32
		calls     int32
		waits     []time.Duration
	}{
		{"recovers", 2, 2, []time.Duration{250 * time.Millisecond}},
		{"exhausted", 0, 3, []time.Duration{250 * time.Millisecond, 500 * time.Millisecond}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) == tc.successAt {
					_, _ = io.WriteString(w, `{"content":[],"stop_reason":"end_turn"}`)
					return
				}
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			defer srv.Close()
			p, err := New(Config{Kind: "anthropic", BaseURL: srv.URL}, false, nil)
			if err != nil {
				t.Fatal(err)
			}
			var waits []time.Duration
			p.(*anthropicProvider).http.sleep = func(_ context.Context, delay time.Duration) error {
				waits = append(waits, delay)
				return nil
			}
			result, err := p.Complete(t.Context(), Request{})
			if tc.successAt == 0 && (err == nil || !strings.Contains(err.Error(), "503")) ||
				tc.successAt != 0 && (err != nil || result.StopReason != "end_turn") ||
				calls.Load() != tc.calls || !slices.Equal(waits, tc.waits) {
				t.Fatalf("Complete = %+v, %v; calls = %d; waits = %v", result, err, calls.Load(), waits)
			}
		})
	}
}
