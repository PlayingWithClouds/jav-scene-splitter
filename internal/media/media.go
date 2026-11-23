// Package media is the seam between container formats (MP4, HLS) and the
// analysis: anything that can list keyframe positions, fetch one keyframe, and
// fetch a dense run of frames between two keyframes.
package media

import "context"

// Keyframe is a seekable position: a keyframe the video can be decoded from.
type Keyframe struct {
	Time float64
	// Position is container-specific (sample index for MP4, segment index for HLS).
	Position int
}

// Span is a dense run of coded frames, ready for a decoder.
type Span struct {
	// Units are Annex-B access units in decode order, starting with a keyframe
	// that carries (or is preceded by) its parameter sets.
	Units [][]byte
	// Times are the presentation times of those frames, ascending.
	Times []float64
}

// SizeBin is a stretch of video and how many bytes it takes up. Black and
// near-static pictures compress to very little, so dips in bytes per second
// point at fades without decoding anything.
type SizeBin struct {
	Start    float64
	Duration float64
	Bytes    int64
	// Keyframe marks a single keyframe (per-frame profiles only). A black
	// keyframe is a fraction of a normal one's size.
	Keyframe bool
}

// Video is a seekable video, local or remote.
type Video interface {
	// SizeProfile lists byte sizes over time: per frame for MP4 (free, from the
	// index), per segment for HLS (one HEAD request each, no body).
	SizeProfile(ctx context.Context, workers int) ([]SizeBin, error)
	// Codec is the ffmpeg raw demuxer name: "h264" or "hevc".
	Codec() string
	Duration() float64
	// Keyframes lists every seekable keyframe, ascending by time.
	Keyframes() []Keyframe
	// KeyframeUnit fetches one keyframe as a self-contained Annex-B access unit
	// (parameter sets included), decodable on its own.
	KeyframeUnit(ctx context.Context, keyframe Keyframe) ([]byte, error)
	// Span fetches every frame from one keyframe up to (not including) another.
	Span(ctx context.Context, from Keyframe, to Keyframe) (Span, error)
	// Describe is a one-line summary for logs.
	Describe() string
	// BytesFetched is how many bytes have been read so far.
	BytesFetched() int64
	Close() error
}
