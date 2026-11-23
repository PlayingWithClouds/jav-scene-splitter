package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// DefaultUserAgent is sent unless the caller sets one: CDNs behind Cloudflare
// block Go's default agent outright.
const DefaultUserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"

// Client does GETs and Range GETs with fixed headers, retries and a byte count.
type Client struct {
	http      *http.Client
	headers   http.Header
	bytesRead atomicCounter
}

// NewClient returns a Client that sends headers on every request.
func NewClient(headers http.Header) *Client {
	if headers == nil {
		headers = http.Header{}
	}
	if headers.Get("User-Agent") == "" {
		headers.Set("User-Agent", DefaultUserAgent)
	}
	return &Client{http: &http.Client{Timeout: 60 * time.Second}, headers: headers}
}

// BytesRead is how many body bytes this client has downloaded.
func (client *Client) BytesRead() int64 { return client.bytesRead.load() }

// Get downloads a whole resource.
func (client *Client) Get(ctx context.Context, url string) ([]byte, error) {
	return client.withRetries(ctx, func() ([]byte, error) {
		response, err := client.send(ctx, url, "")
		if err != nil {
			return nil, err
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GET %s: %s", url, response.Status)
		}
		return io.ReadAll(response.Body)
	})
}

// GetRange downloads up to length bytes at offset. A server that ignores Range
// and answers 200 is tolerated when offset is 0: the body is cut at length.
// Fewer bytes come back when the resource is shorter.
func (client *Client) GetRange(ctx context.Context, url string, offset int64, length int64) ([]byte, error) {
	return client.withRetries(ctx, func() ([]byte, error) {
		response, err := client.send(ctx, url, fmt.Sprintf("bytes=%d-%d", offset, offset+length-1))
		if err != nil {
			return nil, err
		}
		defer response.Body.Close()
		partial := response.StatusCode == http.StatusPartialContent
		wholeFromStart := response.StatusCode == http.StatusOK && offset == 0
		if !partial && !wholeFromStart {
			return nil, fmt.Errorf("range GET %s: %s", url, response.Status)
		}
		return io.ReadAll(io.LimitReader(response.Body, length))
	})
}

// ContentLength learns a resource's size without downloading it: a HEAD, or
// a one-byte Range GET when the server answers HEAD without a length.
func (client *Client) ContentLength(ctx context.Context, url string) (int64, error) {
	var length int64
	_, err := client.withRetries(ctx, func() ([]byte, error) {
		request, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
		if err != nil {
			return nil, err
		}
		client.applyHeaders(request)
		response, err := client.http.Do(request)
		if err != nil {
			return nil, err
		}
		response.Body.Close()
		if response.StatusCode == http.StatusOK && response.ContentLength >= 0 {
			length = response.ContentLength
			return nil, nil
		}
		ranged, err := client.send(ctx, url, "bytes=0-0")
		if err != nil {
			return nil, err
		}
		ranged.Body.Close()
		if ranged.StatusCode != http.StatusPartialContent {
			return nil, fmt.Errorf("size of %s: %s", url, ranged.Status)
		}
		length, err = totalSizeFromContentRange(ranged.Header.Get("Content-Range"))
		return nil, err
	})
	return length, err
}

// applyHeaders copies the client's fixed headers onto a request.
func (client *Client) applyHeaders(request *http.Request) {
	for name, values := range client.headers {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
}

// send issues one GET with the client's headers and an optional Range.
func (client *Client) send(ctx context.Context, url string, byteRange string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	client.applyHeaders(request)
	if byteRange != "" {
		request.Header.Set("Range", byteRange)
	}
	return client.http.Do(request)
}

// withRetries runs fetch up to httpAttempts times with quadratic backoff and
// counts the bytes of the successful attempt.
func (client *Client) withRetries(ctx context.Context, fetch func() ([]byte, error)) ([]byte, error) {
	var lastError error
	for attempt := range httpAttempts {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt*attempt) * 250 * time.Millisecond)
		}
		body, err := fetch()
		if err == nil {
			client.bytesRead.add(int64(len(body)))
			return body, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		lastError = err
	}
	return nil, fmt.Errorf("after %d attempts: %w", httpAttempts, lastError)
}

var errNoRangeSupport = errors.New("server ignores Range requests; cannot seek remotely")
