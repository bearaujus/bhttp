# BHTTP - Minimal HTTP Client Helper for Go (Automatic JSON Unwrapping, Simple Retries, Rate Limiting)

[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Go Report Card](https://goreportcard.com/badge/github.com/bearaujus/bhttp)](https://goreportcard.com/report/github.com/bearaujus/bhttp)

BHTTP is a lightweight Go library that wraps `net/http`. This library is able to validate expected status codes, 
retry on specific status codes, optionally rate limit requests, and decode JSON responses into a destination struct.

## Installation

To install BHTTP, run:

```sh
go get github.com/bearaujus/bhttp
```

## Import

```go
import "github.com/bearaujus/bhttp"
```

## Features

- Validate response status codes (defaults to `200` OK).
- Retry on specific response status codes (e.g., `429`, `500`, `502`, `503`, `504`).
- Optional rate limiting using `golang.org/x/time/rate`.
- Decode JSON responses into a struct (DoAndUnwrap).
- Helpful error messages including response body (pretty-printed if JSON).

## Usage

### 1. Simple "send + unwrap"

```go
type HttpBinGetResponse struct {
    URL     string            `json:"url"`
    Headers map[string]string `json:"headers"`
}

func main() {
    req, _ := http.NewRequest(http.MethodGet, "https://httpbin.org/get", nil)

    // BHTTP will automatically unmarshal the JSON response body into HttpBinGetResponse struct.
    resp, err := bhttp.DoAndUnwrap[HttpBinGetResponse](req)
    if err != nil {
        panic(err)
    }

    fmt.Printf("[%T] %+v\n", resp, resp)
}
```

```text
[*main.HttpBinGetResponse] &{URL:https://httpbin.org/get Headers:map[Accept-Encoding:gzip Host:httpbin.org User-Agent:Go-http-client/2.0]}
```

### 2. Use Options to validate expected status codes.
```go
func main() {
    req, _ := http.NewRequest(http.MethodGet, "https://httpbin.org/get", nil)

    opts := &bhttp.Options{
        // Only treat these status codes as success (defaults to 200 if omitted). 
        ExpectedStatusCodes: []int{http.StatusOK},
    }
    
    var out HttpBinGetResponse
    if err := bhttp.New().DoAndUnwrapWithOptions(req, &out, opts); err != nil {
        panic(err)
    }

    fmt.Printf("[%T] %+v\n", resp, resp)
}
```

```text
[main.HttpBinGetResponse] {URL:https://httpbin.org/get Headers:map[Accept-Encoding:gzip Host:httpbin.org User-Agent:Go-http-client/2.0]}
```

### 3. Use Options + Retry to retry on specific status codes (e.g., 429 / 5xx).
```go
func main() {
    // Using httpstat.us to simulate a retryable status.
    req, _ := http.NewRequest(http.MethodGet, "https://httpstat.us/503", nil)
    
    opts := &bhttp.Options{
        ExpectedStatusCodes: []int{http.StatusOK},
        Retry: &bhttp.RetryConfig{
            // Number of retries AFTER the first attempt (total tries = 1 + Attempts).
            Attempts: 2,
            
            // Retry only when the response status code matches one of these.
            RetryStatusCodes: []int{http.StatusTooManyRequests, http.StatusServiceUnavailable},
        },
    }
    
    // Note: On the final attempt, BHTTP will stop treating these as retryable so you get
    // a real error with the response body if it still fails.
    if err := bhttp.DoWithOptions(req, opts); err != nil {
        panic(err)
    }
}
```

```text
panic: retries exhausted after 2 attempt(s): Get "https://httpstat.us/503": EOF
```

## TODO
- Add support for back-off retry, jitter & similar kind of components for retry mechanism

## License

This project is licensed under the MIT License - see the [LICENSE](https://github.com/bearaujus/bhttp/blob/master/LICENSE) file for details.
