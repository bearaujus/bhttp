package bhttp_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/time/rate"

	"github.com/bearaujus/bhttp"
)

func TestNew(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "default client should not be nil"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := bhttp.New()
			if got == nil {
				t.Fatalf("New() returned nil")
			}
			if got.Client() == nil {
				t.Fatalf("New().Client() returned nil")
			}
			if got.Client() != http.DefaultClient {
				t.Fatalf("New().Client() = %p, want http.DefaultClient %p", got.Client(), http.DefaultClient)
			}
		})
	}
}

func TestNewWithClient(t *testing.T) {
	custom := &http.Client{Timeout: 123 * time.Millisecond}

	tests := []struct {
		name   string
		client *http.Client
		want   *http.Client
	}{
		{
			name:   "nil client uses http.DefaultClient",
			client: nil,
			want:   http.DefaultClient,
		},
		{
			name:   "custom client is used",
			client: custom,
			want:   custom,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := bhttp.NewWithClient(tt.client)
			if got == nil {
				t.Fatalf("NewWithClient() returned nil")
			}
			if got.Client() != tt.want {
				t.Fatalf("Client() = %p, want %p", got.Client(), tt.want)
			}
		})
	}
}

func TestBHTTP_Do(t *testing.T) {
	tests := []struct {
		name        string
		statusCode  int
		body        string
		wantErr     bool
		errContains []string
	}{
		{
			name:       "200 OK should pass",
			statusCode: http.StatusOK,
			body:       `{"ok":true}`,
			wantErr:    false,
		},
		{
			name:       "unexpected status should return error with body",
			statusCode: http.StatusBadRequest,
			body:       `{"error":"bad request","code":400}`,
			wantErr:    true,
			errContains: []string{
				"expected status code",
				`"error"`, // body included (pretty JSON or raw)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.statusCode)
				_, _ = w.Write([]byte(tt.body))
			}))
			t.Cleanup(srv.Close)

			req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
			h := bhttp.NewWithClient(srv.Client())

			err := h.Do(req)
			if tt.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("expected nil error, got: %v", err)
			}
			if err != nil {
				for _, s := range tt.errContains {
					if !strings.Contains(err.Error(), s) {
						t.Fatalf("error %q does not contain %q", err.Error(), s)
					}
				}
			}
		})
	}
}

func TestBHTTP_DoAndUnwrapWithOptions(t *testing.T) {
	type Resp struct {
		Message string `json:"message"`
	}

	tests := []struct {
		name        string
		statusCode  int
		body        string
		dest        any
		wantMessage string
		wantErr     bool
		errContains []string
	}{
		{
			name:        "unwrap success",
			statusCode:  http.StatusOK,
			body:        `{"message":"hello"}`,
			dest:        &Resp{},
			wantMessage: "hello",
			wantErr:     false,
		},
		{
			name:       "dest must be non-nil pointer",
			statusCode: http.StatusOK,
			body:       `{"message":"hello"}`,
			dest:       Resp{}, // not a pointer
			wantErr:    true,
			errContains: []string{
				"dest must be a non-nil pointer",
			},
		},
		{
			name:       "invalid json should error",
			statusCode: http.StatusOK,
			body:       `{"message":`,
			dest:       &Resp{},
			wantErr:    true,
			errContains: []string{
				"fail to unmarshal response body",
			},
		},
		{
			name:       "unexpected status should error",
			statusCode: http.StatusBadRequest,
			body:       `{"error":"nope"}`,
			dest:       &Resp{},
			wantErr:    true,
			errContains: []string{
				"expected status code",
				`"error"`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.statusCode)
				_, _ = w.Write([]byte(tt.body))
			}))
			t.Cleanup(srv.Close)

			req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
			h := bhttp.NewWithClient(srv.Client())

			opts := &bhttp.Options{
				ExpectedStatusCodes: []int{http.StatusOK},
			}

			err := h.DoAndUnwrapWithOptions(req, tt.dest, opts)

			if tt.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("expected nil error, got: %v", err)
			}
			if err != nil {
				for _, s := range tt.errContains {
					if !strings.Contains(err.Error(), s) {
						t.Fatalf("error %q does not contain %q", err.Error(), s)
					}
				}
				return
			}

			// validate unwrapped value if success
			if out, ok := tt.dest.(*Resp); ok {
				if out.Message != tt.wantMessage {
					t.Fatalf("dest.Message = %q, want %q", out.Message, tt.wantMessage)
				}
			}
		})
	}
}

func TestBHTTP_DoWithOptions_Retry(t *testing.T) {
	tests := []struct {
		name        string
		attempts    int
		retryCodes  []int
		handler     func(hit int32, w http.ResponseWriter, r *http.Request)
		wantErr     bool
		wantHits    int32
		errContains []string
	}{
		{
			name:       "retries then succeeds (503,503,200)",
			attempts:   2, // total tries = 3
			retryCodes: []int{http.StatusServiceUnavailable},
			handler: func(hit int32, w http.ResponseWriter, r *http.Request) {
				if hit <= 2 {
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = w.Write([]byte(`{"error":"temporary"}`))
					return
				}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"ok":true}`))
			},
			wantErr:  false,
			wantHits: 3,
		},
		{
			name:       "retry exhausted returns wrapped error with body",
			attempts:   2, // total tries = 3
			retryCodes: []int{http.StatusServiceUnavailable},
			handler: func(hit int32, w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"error":"still down"}`))
			},
			wantErr:  true,
			wantHits: 3,
			errContains: []string{
				"retries exhausted",
				"expected status code",
				`"still down"`,
			},
		},
		{
			name:       "last try disables retry codes (so it becomes an expected-status error)",
			attempts:   1, // total tries = 2
			retryCodes: []int{http.StatusServiceUnavailable},
			handler: func(hit int32, w http.ResponseWriter, r *http.Request) {
				// Always 503; last try should return expected-status error, not retry again.
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"error":"no recovery"}`))
			},
			wantErr:  true,
			wantHits: 2,
			errContains: []string{
				"retries exhausted",
				"expected status code",
				`"no recovery"`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var hits int32

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hit := atomic.AddInt32(&hits, 1)
				tt.handler(hit, w, r)
			}))
			t.Cleanup(srv.Close)

			req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
			h := bhttp.NewWithClient(srv.Client())

			opts := &bhttp.Options{
				ExpectedStatusCodes: []int{http.StatusOK},
				Retry: &bhttp.RetryConfig{
					Attempts:         tt.attempts,
					RetryStatusCodes: tt.retryCodes,
				},
			}

			err := h.DoWithOptions(req, opts)

			if tt.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("expected nil error, got: %v", err)
			}
			if err != nil {
				for _, s := range tt.errContains {
					if !strings.Contains(err.Error(), s) {
						t.Fatalf("error %q does not contain %q", err.Error(), s)
					}
				}
			}

			if got := atomic.LoadInt32(&hits); got != tt.wantHits {
				t.Fatalf("hits = %d, want %d", got, tt.wantHits)
			}
		})
	}
}

func TestBHTTP_DoWithOptions_RateLimiter(t *testing.T) {
	tests := []struct {
		name    string
		limiter *rate.Limiter
		wantErr bool
	}{
		{
			name:    "rate limiter allows request",
			limiter: rate.NewLimiter(rate.Every(1*time.Millisecond), 1),
			wantErr: false,
		},
		{
			name:    "nil limiter is ok",
			limiter: nil,
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"ok":true}`))
			}))
			t.Cleanup(srv.Close)

			req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
			h := bhttp.NewWithClient(srv.Client())

			err := h.DoWithOptions(req, &bhttp.Options{
				RateLimiter: tt.limiter,
			})

			if tt.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("expected nil error, got: %v", err)
			}
		})
	}
}

func TestPackage_Do(t *testing.T) {
	tests := []struct {
		name        string
		statusCode  int
		body        string
		wantErr     bool
		errContains []string
	}{
		{
			name:       "200 OK should pass",
			statusCode: http.StatusOK,
			body:       `{"ok":true}`,
			wantErr:    false,
		},
		{
			name:       "unexpected status should return error with body",
			statusCode: http.StatusBadRequest,
			body:       `{"error":"bad request","code":400}`,
			wantErr:    true,
			errContains: []string{
				"expected status code",
				`"error"`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.statusCode)
				_, _ = w.Write([]byte(tt.body))
			}))
			t.Cleanup(srv.Close)

			req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)

			err := bhttp.Do(req)

			if tt.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("expected nil error, got: %v", err)
			}
			if err != nil {
				for _, s := range tt.errContains {
					if !strings.Contains(err.Error(), s) {
						t.Fatalf("error %q does not contain %q", err.Error(), s)
					}
				}
			}
		})
	}
}

func TestPackage_DoWithOptions(t *testing.T) {
	tests := []struct {
		name        string
		statusCode  int
		body        string
		opts        *bhttp.Options
		wantErr     bool
		errContains []string
	}{
		{
			name:       "nil opts behaves like default (expects 200)",
			statusCode: http.StatusOK,
			body:       `{"ok":true}`,
			opts:       nil,
			wantErr:    false,
		},
		{
			name:       "custom expected status codes (201) should pass",
			statusCode: http.StatusCreated,
			body:       `{"ok":true}`,
			opts: &bhttp.Options{
				ExpectedStatusCodes: []int{http.StatusCreated},
			},
			wantErr: false,
		},
		{
			name:       "unexpected status with options should error",
			statusCode: http.StatusCreated,
			body:       `{"ok":true}`,
			opts: &bhttp.Options{
				ExpectedStatusCodes: []int{http.StatusOK},
			},
			wantErr: true,
			errContains: []string{
				"expected status code",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.statusCode)
				_, _ = w.Write([]byte(tt.body))
			}))
			t.Cleanup(srv.Close)

			req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)

			err := bhttp.DoWithOptions(req, tt.opts)

			if tt.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("expected nil error, got: %v", err)
			}
			if err != nil {
				for _, s := range tt.errContains {
					if !strings.Contains(err.Error(), s) {
						t.Fatalf("error %q does not contain %q", err.Error(), s)
					}
				}
			}
		})
	}
}

func TestPackage_DoAndUnwrap(t *testing.T) {
	type Resp struct {
		Message string `json:"message"`
	}

	tests := []struct {
		name        string
		statusCode  int
		body        string
		wantErr     bool
		wantMessage string
		errContains []string
	}{
		{
			name:        "unwrap success with default expected status",
			statusCode:  http.StatusOK,
			body:        `{"message":"hello"}`,
			wantErr:     false,
			wantMessage: "hello",
		},
		{
			name:       "invalid json returns error",
			statusCode: http.StatusOK,
			body:       `{"message":`,
			wantErr:    true,
			errContains: []string{
				"fail to unmarshal response body",
			},
		},
		{
			name:       "unexpected status returns error",
			statusCode: http.StatusBadRequest,
			body:       `{"error":"nope"}`,
			wantErr:    true,
			errContains: []string{
				"expected status code",
				`"error"`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.statusCode)
				_, _ = w.Write([]byte(tt.body))
			}))
			t.Cleanup(srv.Close)

			req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)

			out, err := bhttp.DoAndUnwrap[Resp](req)

			if tt.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("expected nil error, got: %v", err)
			}
			if err != nil {
				for _, s := range tt.errContains {
					if !strings.Contains(err.Error(), s) {
						t.Fatalf("error %q does not contain %q", err.Error(), s)
					}
				}
				return
			}

			if out == nil {
				t.Fatalf("expected non-nil response")
			}
			if out.Message != tt.wantMessage {
				t.Fatalf("Message = %q, want %q", out.Message, tt.wantMessage)
			}
		})
	}
}

func TestPackage_DoAndUnwrapWithOptions(t *testing.T) {
	type Resp struct {
		Message string `json:"message"`
	}

	tests := []struct {
		name        string
		statusCode  int
		body        string
		opts        *bhttp.Options
		wantErr     bool
		wantMessage string
		errContains []string
	}{
		{
			name:       "unwrap success with expected 201",
			statusCode: http.StatusCreated,
			body:       `{"message":"created"}`,
			opts: &bhttp.Options{
				ExpectedStatusCodes: []int{http.StatusCreated},
			},
			wantErr:     false,
			wantMessage: "created",
		},
		{
			name:       "nil opts uses default expected 200 and should error on 201",
			statusCode: http.StatusCreated,
			body:       `{"message":"created"}`,
			opts:       nil,
			wantErr:    true,
			errContains: []string{
				"expected status code",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.statusCode)
				_, _ = w.Write([]byte(tt.body))
			}))
			t.Cleanup(srv.Close)

			req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)

			out, err := bhttp.DoAndUnwrapWithOptions[Resp](req, tt.opts)

			if tt.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("expected nil error, got: %v", err)
			}
			if err != nil {
				for _, s := range tt.errContains {
					if !strings.Contains(err.Error(), s) {
						t.Fatalf("error %q does not contain %q", err.Error(), s)
					}
				}
				return
			}

			if out == nil {
				t.Fatalf("expected non-nil response")
			}
			if out.Message != tt.wantMessage {
				t.Fatalf("Message = %q, want %q", out.Message, tt.wantMessage)
			}
		})
	}

	t.Run("unwarp with invalid generic type", func(t *testing.T) {
		_, err := bhttp.DoAndUnwrapWithOptions[*Resp](&http.Request{}, nil)
		if err == nil {
			t.Fatalf("expected error, got nil")
		}
	})
}

func TestMethod_DoAndUnwrap(t *testing.T) {
	type Resp struct {
		Message string `json:"message"`
	}

	tests := []struct {
		name        string
		body        string
		dest        any
		wantErr     bool
		wantMessage string
		errContains []string
	}{
		{
			name:        "unwrap into pointer dest",
			body:        `{"message":"hello"}`,
			dest:        &Resp{},
			wantErr:     false,
			wantMessage: "hello",
		},
		{
			name:    "dest must be pointer",
			body:    `{"message":"hello"}`,
			dest:    Resp{}, // not a pointer
			wantErr: true,
			errContains: []string{
				"dest must be a non-nil pointer",
			},
		},
		{
			name:    "dest must not be nil pointer",
			body:    `{"message":"hello"}`,
			dest:    (*Resp)(nil),
			wantErr: true,
			errContains: []string{
				"dest must be a non-nil pointer",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(tt.body))
			}))
			t.Cleanup(srv.Close)

			req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)

			// Use instance, but with server client to avoid global http.DefaultClient assumptions.
			h := bhttp.NewWithClient(srv.Client())

			err := h.DoAndUnwrap(req, tt.dest)

			if tt.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("expected nil error, got: %v", err)
			}
			if err != nil {
				for _, s := range tt.errContains {
					if !strings.Contains(err.Error(), s) {
						t.Fatalf("error %q does not contain %q", err.Error(), s)
					}
				}
				return
			}

			// validate success
			out := tt.dest.(*Resp)
			if out.Message != tt.wantMessage {
				t.Fatalf("Message = %q, want %q", out.Message, tt.wantMessage)
			}
		})
	}
}

func TestBHTTP_Do_NilRequest(t *testing.T) {
	tests := []struct {
		name        string
		req         *http.Request
		wantErr     bool
		errContains []string
	}{
		{
			name:        "nil request should error",
			req:         nil,
			wantErr:     true,
			errContains: []string{"nil request"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := bhttp.New()
			err := h.Do(tt.req)

			if tt.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("expected nil error, got %v", err)
			}
			if err != nil {
				for _, s := range tt.errContains {
					if !strings.Contains(err.Error(), s) {
						t.Fatalf("error %q does not contain %q", err.Error(), s)
					}
				}
			}
		})
	}
}

func TestBHTTP_Do_NilHTTPClient_Unsafe(t *testing.T) {
	tests := []struct {
		name        string
		wantErr     bool
		errContains []string
	}{
		{
			name:        "force client=nil should error",
			wantErr:     true,
			errContains: []string{"nil http client"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := bhttp.New()

			// Force the unexported field bHTTP.client to nil (external test package).
			mustSetUnexportedPtrField(t, h, "client", nil)

			req, _ := http.NewRequest(http.MethodGet, "http://example.invalid", nil)
			err := h.Do(req)

			if tt.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if err != nil {
				for _, s := range tt.errContains {
					if !strings.Contains(err.Error(), s) {
						t.Fatalf("error %q does not contain %q", err.Error(), s)
					}
				}
			}
		})
	}
}

func TestBHTTP_DoWithOptions_RateLimiterWaitFail(t *testing.T) {
	tests := []struct {
		name        string
		cancelCtx   bool
		wantErr     bool
		errContains []string
	}{
		{
			name:        "cancelled context should fail rate limiter wait",
			cancelCtx:   true,
			wantErr:     true,
			errContains: []string{"rate limiter wait failed"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Server shouldn't be called because Wait() fails first.
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Fatalf("server should not be called when rate limiter wait fails")
			}))
			t.Cleanup(srv.Close)

			ctx := context.Background()
			if tt.cancelCtx {
				cctx, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cctx
			}

			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)

			h := bhttp.NewWithClient(srv.Client())
			err := h.DoWithOptions(req, &bhttp.Options{
				RateLimiter: rate.NewLimiter(rate.Every(1*time.Second), 1),
			})

			if tt.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if err != nil {
				for _, s := range tt.errContains {
					if !strings.Contains(err.Error(), s) {
						t.Fatalf("error %q does not contain %q", err.Error(), s)
					}
				}
			}
		})
	}
}

func TestBHTTP_Do_HTTPClientDoError(t *testing.T) {
	tests := []struct {
		name        string
		wantErr     bool
		errContains []string
	}{
		{
			name:        "http client Do returns error",
			wantErr:     true,
			errContains: []string{"transport boom"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &http.Client{
				Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
					return nil, errors.New("transport boom")
				}),
			}

			h := bhttp.NewWithClient(client)
			req, _ := http.NewRequest(http.MethodGet, "http://example.invalid", nil)

			err := h.Do(req)
			if tt.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if err != nil {
				for _, s := range tt.errContains {
					if !strings.Contains(err.Error(), s) {
						t.Fatalf("error %q does not contain %q", err.Error(), s)
					}
				}
			}
		})
	}
}

func TestBHTTP_Do_ResponseBodyReadError(t *testing.T) {
	tests := []struct {
		name        string
		wantErr     bool
		errContains []string
	}{
		{
			name:        "response body read error should bubble up",
			wantErr:     true,
			errContains: []string{"read boom"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &http.Client{
				Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: http.StatusOK,
						Body:       &errReadCloser{err: errors.New("read boom")},
						Header:     make(http.Header),
					}, nil
				}),
			}

			h := bhttp.NewWithClient(client)
			req, _ := http.NewRequest(http.MethodGet, "http://example.invalid", nil)

			err := h.Do(req)
			if tt.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if err != nil {
				for _, s := range tt.errContains {
					if !strings.Contains(err.Error(), s) {
						t.Fatalf("error %q does not contain %q", err.Error(), s)
					}
				}
			}
		})
	}
}

func TestBHTTP_Do_NonJSONErrorBodyKeptRaw(t *testing.T) {
	tests := []struct {
		name        string
		statusCode  int
		body        string
		wantErr     bool
		errContains []string
	}{
		{
			name:       "non-json body should appear raw in error",
			statusCode: http.StatusInternalServerError,
			body:       "plain error text",
			wantErr:    true,
			errContains: []string{
				"expected status code",
				"plain error text",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.statusCode)
				_, _ = w.Write([]byte(tt.body))
			}))
			t.Cleanup(srv.Close)

			req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
			h := bhttp.NewWithClient(srv.Client())

			err := h.Do(req)
			if tt.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if err != nil {
				for _, s := range tt.errContains {
					if !strings.Contains(err.Error(), s) {
						t.Fatalf("error %q does not contain %q", err.Error(), s)
					}
				}
			}
		})
	}
}

func TestBHTTP_DoWithOptions_RetryNilConfigPath(t *testing.T) {
	tests := []struct {
		name    string
		wantErr bool
	}{
		{name: "opts.Retry nil should not panic and should succeed", wantErr: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"ok":true}`))
			}))
			t.Cleanup(srv.Close)

			req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
			h := bhttp.NewWithClient(srv.Client())

			opts := &bhttp.Options{
				ExpectedStatusCodes: []int{http.StatusOK},
				Retry:               nil, // exercise opts.Retry == nil branch
			}

			err := h.DoWithOptions(req, opts)
			if tt.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("expected nil error, got %v", err)
			}
		})
	}
}

func TestBHTTP_DoWithOptions_NegativeAttemptsGuard(t *testing.T) {
	tests := []struct {
		name        string
		attempts    int
		wantHits    int32
		wantErr     bool
		errContains []string
	}{
		{
			name:     "negative attempts treated as 0 (only 1 try, no wrapped retries exhausted)",
			attempts: -5,
			wantHits: 1,
			wantErr:  true,
			errContains: []string{
				"expected status code", // from do()
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var hits int32

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&hits, 1)
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"error":"down"}`))
			}))
			t.Cleanup(srv.Close)

			req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
			h := bhttp.NewWithClient(srv.Client())

			opts := &bhttp.Options{
				ExpectedStatusCodes: []int{http.StatusOK},
				Retry: &bhttp.RetryConfig{
					Attempts:         tt.attempts,
					RetryStatusCodes: []int{http.StatusServiceUnavailable},
				},
			}

			err := h.DoWithOptions(req, opts)

			if tt.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if err != nil {
				// Because Attempts is clamped to 0, exec() should NOT wrap with "retries exhausted..."
				if strings.Contains(err.Error(), "retries exhausted") {
					t.Fatalf("did not expect wrapped retries exhausted error when attempts < 0; got: %v", err)
				}
				for _, s := range tt.errContains {
					if !strings.Contains(err.Error(), s) {
						t.Fatalf("error %q does not contain %q", err.Error(), s)
					}
				}
			}

			if got := atomic.LoadInt32(&hits); got != tt.wantHits {
				t.Fatalf("hits = %d, want %d", got, tt.wantHits)
			}
		})
	}
}

/******** helpers ********/

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type errReadCloser struct{ err error }

func (e *errReadCloser) Read([]byte) (int, error) { return 0, e.err }
func (e *errReadCloser) Close() error             { return nil }

// mustSetUnexportedPtrField sets an unexported pointer field on a concrete value.
// It accepts either an interface value (kind Interface) or a direct pointer (kind Ptr).
func mustSetUnexportedPtrField(t *testing.T, obj any, field string, ptr any) {
	t.Helper()

	v := reflect.ValueOf(obj)

	// Allow both interface and pointer inputs.
	if v.Kind() == reflect.Interface {
		v = v.Elem()
	}

	if v.Kind() != reflect.Pointer {
		t.Fatalf("expected pointer or interface-to-pointer, got kind %s", v.Kind())
	}
	if v.IsNil() {
		t.Fatalf("got nil pointer")
	}

	elem := v.Elem()
	f := elem.FieldByName(field)
	if !f.IsValid() {
		t.Fatalf("field %q not found on %T", field, v.Interface())
	}

	// Set via unsafe because field is unexported.
	f = reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem()

	if ptr == nil {
		f.Set(reflect.Zero(f.Type()))
		return
	}

	pv := reflect.ValueOf(ptr)
	if !pv.Type().AssignableTo(f.Type()) {
		t.Fatalf("ptr type %s not assignable to field type %s", pv.Type(), f.Type())
	}
	f.Set(pv)
}
