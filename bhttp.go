package bhttp

import (
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/time/rate"
	"io"
	"net/http"
	"reflect"
	"slices"
)

type bHTTP struct {
	client *http.Client
}

// BHTTP is a small HTTP helper interface that wraps an underlying *http.Client and
// provides convenience methods for:
//   - validating expected response status codes,
//   - retrying based on response status codes,
//   - optionally unmarshaling JSON responses into a destination struct.
//
// Notes:
//   - Retry is currently status-code based only (http.Client.Do errors are returned immediately).
//   - For the final attempt, the implementation may disable RetryStatusCodes so that a previously
//     "retryable" status code becomes a returned error (useful to surface the response body).
//   - If you retry requests with a non-empty body (POST/PUT), ensure the request body is replayable
//     (e.g. req.GetBody is set, or you rebuild the request per attempt).
type BHTTP interface {
	// Client returns the underlying *http.Client used by this instance.
	// Callers may use it to customize transport/timeouts or to perform advanced requests directly.
	Client() *http.Client

	// Do execute the HTTP request using default behavior.
	//
	// Defaults:
	//   - ExpectedStatusCodes: []int{http.StatusOK}
	//   - Retry: disabled (no retries)
	//   - RateLimiter: none
	//
	// Returns an error if the request fails or the response status code is not expected.
	Do(req *http.Request) error

	// DoWithOptions executes the HTTP request with the provided options.
	//
	// It validates the response status code against opts.ExpectedStatusCodes (default 200).
	// If opts.Retry is configured, it will retry when the response status code matches
	// opts.Retry.RetryStatusCodes, up to 1+opts.Retry.Attempts total tries.
	//
	// Returns an error if the request fails, retries are exhausted, or the final response status
	// code is not expected.
	DoWithOptions(req *http.Request, opts *Options) error

	// DoAndUnwrap executes the request using default behavior and unmarshal the JSON response body
	// into dest.
	//
	// Requirements:
	//   - dest must be a non-nil pointer.
	//
	// Defaults:
	//   - ExpectedStatusCodes: []int{http.StatusOK}
	//   - Retry: disabled (no retries)
	//   - RateLimiter: none
	//
	// Returns an error if the request fails, the response status code is not expected, or the
	// response body cannot be unmarshaled into dest.
	DoAndUnwrap(req *http.Request, dest any) error

	// DoAndUnwrapWithOptions executes the request with the provided options and unmarshal the JSON
	// response body into dest.
	//
	// Requirements:
	//   - dest must be a non-nil pointer.
	//
	// Behavior:
	//   - status code validation uses opts.ExpectedStatusCodes (default 200)
	//   - retry behavior uses opts.Retry.RetryStatusCodes and opts.Retry.Attempts (if provided)
	//   - rate limiting uses opts.RateLimiter (if provided)
	//
	// Returns an error if the request fails, retries are exhausted, the final response status
	// code is not expected, or the response body cannot be unmarshalled into dest.
	DoAndUnwrapWithOptions(req *http.Request, dest any, opts *Options) error
}

// New constructs a BHTTP instance using http.DefaultClient.
//
// Use NewWithClient if you need a custom *http.Client (timeouts, transport, proxy, etc).
func New() BHTTP {
	return NewWithClient(http.DefaultClient)
}

// NewWithClient constructs a BHTTP instance using the provided *http.Client.
//
// If client is nil, http.DefaultClient is used.
func NewWithClient(client *http.Client) BHTTP {
	if client == nil {
		client = http.DefaultClient
	}
	return &bHTTP{client}
}

// Do execute an HTTP request using the package default client (http.DefaultClient)
// and default options.
//
// Defaults:
//   - ExpectedStatusCodes: []int{http.StatusOK}
//   - Retry: disabled (no retries)
//   - RateLimiter: none
//
// Returns an error if the request fails or the response status code is not expected.
func Do(req *http.Request) error {
	return DoWithOptions(req, nil)
}

// DoWithOptions executes an HTTP request using the package default client (http.DefaultClient)
// and the provided options.
//
// If opts is nil, default options are used (same as Do).
// See Options and RetryConfig for details on status code validation, retry behavior, and rate limiting.
//
// Returns an error if the request fails, retries are exhausted, or the final response status
// code is not expected.
func DoWithOptions(req *http.Request, opts *Options) error {
	return New().DoWithOptions(req, opts)
}

// DoAndUnwrap executes an HTTP request using the package default client (http.DefaultClient)
// and default options, then unmarshal the JSON response body into a value of type T.
//
// Defaults:
//   - ExpectedStatusCodes: []int{http.StatusOK}
//   - Retry: disabled (no retries)
//   - RateLimiter: none
//
// Returns a pointer to the decoded value, or an error if the request fails, the response status
// code is not expected, or the response body cannot be unmarshalled into T.
func DoAndUnwrap[T any](req *http.Request) (*T, error) {
	return DoAndUnwrapWithOptions[T](req, nil)
}

// DoAndUnwrapWithOptions executes an HTTP request using the package default client (http.DefaultClient)
// and the provided options, then unmarshal the JSON response body into a value of type T.
//
// If opts is nil, default options are used.
// See Options and RetryConfig for details on status code validation, retry behavior, and rate limiting.
//
// Returns a pointer to the decoded value, or an error if the request fails, retries are exhausted,
// the final response status code is not expected, or the response body cannot be unmarshalled into T.
func DoAndUnwrapWithOptions[T any](req *http.Request, opts *Options) (*T, error) {
	var t T
	if err := New().DoAndUnwrapWithOptions(req, &t, opts); err != nil {
		return nil, err
	}
	return &t, nil
}

func (c *bHTTP) Client() *http.Client {
	return c.client
}

func (c *bHTTP) Do(req *http.Request) error {
	return c.exec(req, nil, false, nil)
}

func (c *bHTTP) DoWithOptions(req *http.Request, opts *Options) error {
	return c.exec(req, nil, false, opts)
}

func (c *bHTTP) DoAndUnwrap(req *http.Request, dest any) error {
	return c.exec(req, dest, true, nil)
}

func (c *bHTTP) DoAndUnwrapWithOptions(req *http.Request, dest any, opts *Options) error {
	return c.exec(req, dest, true, opts)
}

func (c *bHTTP) exec(req *http.Request, dest any, validateDest bool, opts *Options) error {
	if validateDest {
		rv := reflect.ValueOf(dest)
		if rv.Kind() != reflect.Pointer || rv.IsNil() {
			return fmt.Errorf("dest must be a non-nil pointer. retrieved dest type: %T", dest)
		}
	}
	if opts == nil {
		opts = new(Options)
	}
	if opts.Retry == nil {
		opts.Retry = new(RetryConfig)
	}

	// guard negative values
	if opts.Retry.Attempts < 0 {
		opts.Retry.Attempts = 0
	}

	totalTries := 1 + opts.Retry.Attempts

	for try := 1; try <= totalTries; try++ {
		retryCodes := opts.Retry.RetryStatusCodes
		// last try: disable retry classification so we surface the real error + body
		if try == totalTries {
			retryCodes = nil
		}

		shouldRetry, err := do(
			c.client,
			opts.RateLimiter,
			req,
			dest,
			opts.ExpectedStatusCodes,
			retryCodes,
		)
		if err != nil {
			if opts.Retry.Attempts > 0 {
				return fmt.Errorf("retries exhausted after %d attempt(s): %w", opts.Retry.Attempts, err)
			}
			return err
		}

		if !shouldRetry {
			break
		}
	}

	return nil
}

func do(httpClient *http.Client, rateLimiter *rate.Limiter, req *http.Request, dest any, expectedStatusCodes []int, shouldRetryStatusCodes []int) (bool, error) {
	if httpClient == nil {
		return false, errors.New("nil http client")
	}
	if req == nil {
		return false, errors.New("nil request")
	}
	if len(expectedStatusCodes) == 0 {
		expectedStatusCodes = []int{http.StatusOK}
	}

	reqCtx := req.Context()
	if rateLimiter != nil && reqCtx != nil {
		if err := rateLimiter.Wait(reqCtx); err != nil {
			return false, fmt.Errorf("rate limiter wait failed: %w", err)
		}
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, err
	}

	if slices.Contains(shouldRetryStatusCodes, resp.StatusCode) {
		return true, nil
	}

	errRespBody := string(body)
	var raw any
	if uerr := json.Unmarshal(body, &raw); uerr == nil {
		if pretty, merr := json.MarshalIndent(raw, "", "\t"); merr == nil {
			errRespBody = string(pretty)
		}
	}

	if !slices.Contains(expectedStatusCodes, resp.StatusCode) {
		return false, fmt.Errorf("expected status code(s) %+v but got %d. body: %s", expectedStatusCodes, resp.StatusCode, errRespBody)
	}

	if dest == nil {
		return false, nil
	}

	if err = json.Unmarshal(body, dest); err != nil {
		return false, fmt.Errorf("fail to unmarshal response body into dest. err: %w. body: %s", err, errRespBody)
	}

	return false, nil
}
