package bhttp

import (
	"golang.org/x/time/rate"
)

type Options struct {
	// ExpectedStatusCodes defines which HTTP status codes are considered successful.
	// If empty/nil, defaults to []int{http.StatusOK}.
	ExpectedStatusCodes []int

	// Retry configures retry behavior based on response status codes.
	// If nil, it is treated as &RetryConfig{} (no retries by default).
	Retry *RetryConfig

	// RateLimiter, if set, will wait before EACH attempt (including retries) using req.Context().
	// This is useful to cap outgoing QPS across calls.
	// If nil, no rate limiting is applied.
	RateLimiter *rate.Limiter
}

type RetryConfig struct {
	// Attempts is the number of retries AFTER the first attempt.
	// Total tries = 1 + Attempts.
	//
	// Example:
	//   Attempts = 0 => 1 try total (no retries)
	//   Attempts = 2 => up to 3 tries total
	Attempts int

	// RetryStatusCodes lists HTTP status codes that should trigger a retry.
	// Only response-status-based retries are supported by your current code
	// (network errors are returned immediately and are not retried).
	//
	// Example common retry codes: 429, 500, 502, 503, 504.
	RetryStatusCodes []int
}
