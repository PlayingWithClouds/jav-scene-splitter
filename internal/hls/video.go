package hls

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"sync"

	"github.com/PlayingWithClouds/jav-scene-splitter/internal/media"
	"github.com/PlayingWithClouds/jav-scene-splitter/internal/source"
)

// headBytes is how much of a segment's start is fetched to find its opening
// keyframe; doubled until a whole keyframe is in hand.
const headBytes = 48 * 1024

// Video adapts an HLS variant to media.Video. Each segment is assumed to open
// with a keyframe, which is how HLS is cut in practice.
type Video struct {
	client      *source.Client
	segments    []segment
	codec       string
	variantURL  string
	variantInfo string

	mutex         sync.Mutex
	parameterSets [][]byte
}

// Open loads a playlist, choosing the lowest-bandwidth variant of a master
// playlist (thumbnails are 64x36, so resolution is wasted), and probes the
// first segment for the codec.
func Open(ctx context.Context, client *source.Client, playlistURL string, body []byte) (*Video, error) {
	base, err := url.Parse(playlistURL)
	if err != nil {
		return nil, err
	}
	variants, err := parseMaster(body, base)
	if err != nil {
		return nil, err
	}
	video := &Video{client: client, variantURL: playlistURL, variantInfo: "media playlist"}
	if len(variants) > 0 {
		lowest := slices.MinFunc(variants, func(first, second variant) int {
			return int(first.bandwidth - second.bandwidth)
		})
		video.variantURL = lowest.url
		video.variantInfo = fmt.Sprintf("variant %d of %d, %d kbit/s", slices.Index(variants, lowest)+1, len(variants), lowest.bandwidth/1000)
		body, err = client.Get(ctx, lowest.url)
		if err != nil {
			return nil, fmt.Errorf("variant playlist: %w", err)
		}
		base, _ = url.Parse(lowest.url)
	}
	video.segments, err = parseMedia(body, base)
	if err != nil {
		return nil, err
	}
	if len(video.segments) == 0 {
		return nil, errors.New("playlist has no segments")
	}
	_, err = video.KeyframeUnit(ctx, media.Keyframe{Position: 0})
	if err != nil {
		return nil, fmt.Errorf("probing first segment: %w", err)
	}
	return video, nil
}

func (video *Video) Codec() string       { return video.codec }
func (video *Video) BytesFetched() int64 { return video.client.BytesRead() }
func (video *Video) Close() error        { return nil }

// Duration is the sum of segment durations.
func (video *Video) Duration() float64 {
	last := video.segments[len(video.segments)-1]
	return last.start + last.duration
}

// Describe summarises the chosen variant.
func (video *Video) Describe() string {
	return fmt.Sprintf("HLS %s (%s), %d segments", video.codec, video.variantInfo, len(video.segments))
}

// Keyframes lists one keyframe per segment, at the segment's start.
func (video *Video) Keyframes() []media.Keyframe {
	keyframes := make([]media.Keyframe, len(video.segments))
	for index, current := range video.segments {
		keyframes[index] = media.Keyframe{Time: current.start, Position: index}
	}
	return keyframes
}

// SizeProfile returns each segment's byte size. Byte-range playlists already
// carry them; otherwise each segment costs one HEAD request.
func (video *Video) SizeProfile(ctx context.Context, workers int) ([]media.SizeBin, error) {
	bins := make([]media.SizeBin, len(video.segments))
	jobs := make(chan int)
	var firstError error
	var errorOnce sync.Once
	var waitGroup sync.WaitGroup
	for range workers {
		waitGroup.Go(func() {
			for index := range jobs {
				current := video.segments[index]
				size := current.rangeLength
				if size < 0 {
					var err error
					size, err = video.client.ContentLength(ctx, current.url)
					if err != nil {
						errorOnce.Do(func() { firstError = err })
						continue
					}
				}
				bins[index] = media.SizeBin{Start: current.start, Duration: current.duration, Bytes: size}
			}
		})
	}
	for index := range video.segments {
		if ctx.Err() != nil {
			break
		}
		jobs <- index
	}
	close(jobs)
	waitGroup.Wait()
	if firstError != nil {
		return nil, firstError
	}
	return bins, ctx.Err()
}

// KeyframeUnit fetches the head of a segment, growing the request until the
// first keyframe access unit is complete.
func (video *Video) KeyframeUnit(ctx context.Context, keyframe media.Keyframe) ([]byte, error) {
	current := video.segments[keyframe.Position]
	for length := int64(headBytes); ; length *= 2 {
		data, wholeSegment, err := video.fetchHead(ctx, current, length)
		if err != nil {
			return nil, err
		}
		stream, err := demuxVideo(data, wholeSegment)
		if err != nil {
			return nil, err
		}
		video.rememberCodec(stream.codec)
		unit, found := video.firstKeyframe(stream)
		if found {
			return unit, nil
		}
		if wholeSegment {
			return nil, fmt.Errorf("segment %d has no keyframe", keyframe.Position)
		}
	}
}

// fetchHead fetches up to length bytes from the start of a segment and reports
// whether that covered the whole segment.
func (video *Video) fetchHead(ctx context.Context, current segment, length int64) ([]byte, bool, error) {
	if current.rangeLength >= 0 {
		length = min(length, current.rangeLength)
	}
	data, err := video.client.GetRange(ctx, current.url, current.rangeOffset, length)
	if err != nil {
		return nil, false, err
	}
	wholeSegment := int64(len(data)) < length || length == current.rangeLength
	return data, wholeSegment, nil
}

// firstKeyframe returns the first keyframe unit, with parameter sets prefixed
// when the unit lacks them, and records any parameter sets it sees.
func (video *Video) firstKeyframe(stream demuxed) ([]byte, bool) {
	for _, packet := range stream.packets {
		sets := collectParameterSets(stream.codec, packet.data)
		if len(sets) > 0 {
			video.mutex.Lock()
			video.parameterSets = sets
			video.mutex.Unlock()
		}
		if !isKeyframe(stream.codec, packet.data) {
			continue
		}
		video.mutex.Lock()
		known := video.parameterSets
		video.mutex.Unlock()
		return withParameterSets(stream.codec, packet.data, known), true
	}
	return nil, false
}

// rememberCodec stores the codec the first time a segment reveals it.
func (video *Video) rememberCodec(codec string) {
	video.mutex.Lock()
	defer video.mutex.Unlock()
	if video.codec == "" {
		video.codec = codec
	}
}

// Span fetches whole segments [from, to) and returns every video frame from
// the first keyframe on, timed by segment start plus PTS offset.
func (video *Video) Span(ctx context.Context, from media.Keyframe, to media.Keyframe) (media.Span, error) {
	var span media.Span
	for index := from.Position; index < to.Position; index++ {
		current := video.segments[index]
		data, err := video.fetchWhole(ctx, current)
		if err != nil {
			return media.Span{}, err
		}
		stream, err := demuxVideo(data, true)
		if err != nil {
			return media.Span{}, err
		}
		appendSegment(&span, stream, current.start)
	}
	trimToFirstKeyframe(&span, video.codec)
	if len(span.Units) == 0 {
		return media.Span{}, errors.New("no keyframe in span")
	}
	video.mutex.Lock()
	span.Units[0] = withParameterSets(video.codec, span.Units[0], video.parameterSets)
	video.mutex.Unlock()
	sort.Float64s(span.Times)
	return span, nil
}

// fetchWhole downloads a segment (or its byte range) entirely.
func (video *Video) fetchWhole(ctx context.Context, current segment) ([]byte, error) {
	if current.rangeLength >= 0 {
		return video.client.GetRange(ctx, current.url, current.rangeOffset, current.rangeLength)
	}
	return video.client.Get(ctx, current.url)
}

// appendSegment adds a segment's units, timing each frame relative to the
// segment's earliest PTS so playlist time and frame time line up.
func appendSegment(span *media.Span, stream demuxed, segmentStart float64) {
	earliest := int64(-1)
	for _, packet := range stream.packets {
		if packet.hasPTS && (earliest < 0 || packet.pts < earliest) {
			earliest = packet.pts
		}
	}
	for _, packet := range stream.packets {
		span.Units = append(span.Units, packet.data)
		time := segmentStart
		if packet.hasPTS && earliest >= 0 {
			time += float64(packet.pts-earliest) / 90000
		}
		span.Times = append(span.Times, time)
	}
}

// trimToFirstKeyframe drops leading units a decoder could not start from.
func trimToFirstKeyframe(span *media.Span, codec string) {
	for index, unit := range span.Units {
		if isKeyframe(codec, unit) {
			span.Units = span.Units[index:]
			span.Times = span.Times[index:]
			return
		}
	}
	span.Units, span.Times = nil, nil
}
