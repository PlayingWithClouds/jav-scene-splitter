// Package source gives random access to a video's bytes, whether the video is
// a local file or a URL on a server that answers HTTP Range requests.
package source

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
)

// Source is a sized, random-access view of a video's bytes.
type Source interface {
	// ReadRange returns exactly length bytes starting at offset.
	ReadRange(ctx context.Context, offset int64, length int64) ([]byte, error)
	// Size is the total length of the video in bytes.
	Size() int64
	// BytesRead is how many bytes have been fetched so far.
	BytesRead() int64
	Close() error
}

// IsURL reports whether location is an http(s) URL rather than a path.
func IsURL(location string) bool {
	return strings.HasPrefix(location, "http://") || strings.HasPrefix(location, "https://")
}

// Open returns a Source for a local path or an http(s) URL.
func Open(ctx context.Context, location string, client *Client) (Source, error) {
	if IsURL(location) {
		return openHTTP(ctx, location, client)
	}
	return openFile(location)
}

type fileSource struct {
	file      *os.File
	size      int64
	bytesRead atomicCounter
}

// openFile opens a local video file.
func openFile(path string) (*fileSource, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	return &fileSource{file: file, size: info.Size()}, nil
}

// ReadRange reads length bytes at offset from the file.
func (source *fileSource) ReadRange(_ context.Context, offset int64, length int64) ([]byte, error) {
	buffer := make([]byte, length)
	_, err := source.file.ReadAt(buffer, offset)
	if err != nil {
		return nil, fmt.Errorf("read %d bytes at %d: %w", length, offset, err)
	}
	source.bytesRead.add(length)
	return buffer, nil
}

func (source *fileSource) Size() int64      { return source.size }
func (source *fileSource) BytesRead() int64 { return source.bytesRead.load() }
func (source *fileSource) Close() error     { return source.file.Close() }

type httpSource struct {
	client *Client
	url    string
	size   int64
}

const httpAttempts = 4

// openHTTP learns the file size from a one-byte range request, which also
// confirms the server supports ranges at all.
func openHTTP(ctx context.Context, url string, client *Client) (*httpSource, error) {
	response, err := client.send(ctx, url, "bytes=0-0")
	if err != nil {
		return nil, err
	}
	response.Body.Close()
	if response.StatusCode == http.StatusOK {
		return nil, errNoRangeSupport
	}
	if response.StatusCode != http.StatusPartialContent {
		return nil, fmt.Errorf("GET %s: %s", url, response.Status)
	}
	size, err := totalSizeFromContentRange(response.Header.Get("Content-Range"))
	if err != nil {
		return nil, err
	}
	return &httpSource{client: client, url: url, size: size}, nil
}

// ReadRange fetches exactly length bytes at offset.
func (source *httpSource) ReadRange(ctx context.Context, offset int64, length int64) ([]byte, error) {
	body, err := source.client.GetRange(ctx, source.url, offset, length)
	if err != nil {
		return nil, err
	}
	if int64(len(body)) != length {
		return nil, fmt.Errorf("range %d+%d: got %d bytes", offset, length, len(body))
	}
	return body, nil
}

func (source *httpSource) Size() int64      { return source.size }
func (source *httpSource) BytesRead() int64 { return source.client.BytesRead() }
func (source *httpSource) Close() error     { return nil }

// totalSizeFromContentRange parses the total from "bytes 0-0/12345".
func totalSizeFromContentRange(header string) (int64, error) {
	_, total, found := strings.Cut(header, "/")
	if !found || total == "*" {
		return 0, fmt.Errorf("cannot learn file size from Content-Range %q", header)
	}
	return strconv.ParseInt(total, 10, 64)
}
