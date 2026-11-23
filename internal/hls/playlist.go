// Package hls reads HLS streams (MPEG-TS segments) as a seekable video:
// one keyframe per segment, fetched by range from the segment's head.
package hls

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// segment is one media segment of the chosen variant.
type segment struct {
	url      string
	start    float64
	duration float64
	// rangeOffset/rangeLength come from EXT-X-BYTERANGE; rangeLength < 0 means
	// the whole resource.
	rangeOffset int64
	rangeLength int64
}

// variant is one rendition listed in a master playlist.
type variant struct {
	url       string
	bandwidth int64
}

// IsPlaylist reports whether a body looks like an M3U8 playlist.
func IsPlaylist(body []byte) bool {
	return bytes.HasPrefix(bytes.TrimSpace(body), []byte("#EXTM3U"))
}

// parseMaster lists the variants of a master playlist (empty for a media playlist).
func parseMaster(body []byte, base *url.URL) ([]variant, error) {
	var variants []variant
	pendingBandwidth := int64(-1)
	scanner := bufio.NewScanner(bytes.NewReader(body))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
			pendingBandwidth = attributeInt(line, "BANDWIDTH")
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") || pendingBandwidth < 0 {
			continue
		}
		resolved, err := base.Parse(line)
		if err != nil {
			return nil, err
		}
		variants = append(variants, variant{url: resolved.String(), bandwidth: pendingBandwidth})
		pendingBandwidth = -1
	}
	return variants, scanner.Err()
}

// parseMedia lists the segments of a media playlist with their start times.
func parseMedia(body []byte, base *url.URL) ([]segment, error) {
	var segments []segment
	var clock float64
	pendingDuration := -1.0
	pendingRange := [2]int64{0, -1}
	var nextRangeOffset int64
	scanner := bufio.NewScanner(bytes.NewReader(body))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case strings.HasPrefix(line, "#EXTINF:"):
			duration, _, _ := strings.Cut(strings.TrimPrefix(line, "#EXTINF:"), ",")
			parsed, err := strconv.ParseFloat(strings.TrimSpace(duration), 64)
			if err != nil {
				return nil, fmt.Errorf("bad EXTINF %q", line)
			}
			pendingDuration = parsed
		case strings.HasPrefix(line, "#EXT-X-BYTERANGE:"):
			pendingRange = parseByteRange(strings.TrimPrefix(line, "#EXT-X-BYTERANGE:"), nextRangeOffset)
		case strings.HasPrefix(line, "#EXT-X-KEY:") && !strings.Contains(line, "METHOD=NONE"):
			return nil, errors.New("encrypted HLS (EXT-X-KEY) is not supported")
		case strings.HasPrefix(line, "#EXT-X-MAP:"):
			return nil, errors.New("fMP4 HLS (EXT-X-MAP) is not supported yet")
		case line == "" || strings.HasPrefix(line, "#"):
		case pendingDuration >= 0:
			resolved, err := base.Parse(line)
			if err != nil {
				return nil, err
			}
			segments = append(segments, segment{
				url: resolved.String(), start: clock, duration: pendingDuration,
				rangeOffset: pendingRange[0], rangeLength: pendingRange[1],
			})
			clock += pendingDuration
			nextRangeOffset = pendingRange[0] + max(pendingRange[1], 0)
			pendingDuration = -1
			pendingRange = [2]int64{0, -1}
		}
	}
	return segments, scanner.Err()
}

// parseByteRange reads "length[@offset]"; a missing offset continues from the
// previous segment's end.
func parseByteRange(value string, continueFrom int64) [2]int64 {
	lengthText, offsetText, hasOffset := strings.Cut(value, "@")
	length, _ := strconv.ParseInt(lengthText, 10, 64)
	offset := continueFrom
	if hasOffset {
		offset, _ = strconv.ParseInt(offsetText, 10, 64)
	}
	return [2]int64{offset, length}
}

// attributeInt reads NAME=123 from an attribute list, or -1.
func attributeInt(line string, name string) int64 {
	_, rest, found := strings.Cut(line, name+"=")
	if !found {
		return -1
	}
	value, _, _ := strings.Cut(rest, ",")
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return -1
	}
	return parsed
}
