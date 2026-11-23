package mp4

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/PlayingWithClouds/jav-scene-splitter/internal/source"
)

// Sample is one coded video frame in the file.
type Sample struct {
	Offset int64
	Size   int64
	// PTS is the presentation time in seconds, edit list applied.
	PTS  float64
	Sync bool
}

// VideoTrack is everything needed to fetch and decode the video.
type VideoTrack struct {
	// Codec is the ffmpeg raw demuxer name: "h264" or "hevc".
	Codec string
	// ParameterSets are SPS/PPS (and VPS for HEVC) NAL units, without start codes.
	ParameterSets [][]byte
	// NALLengthSize is the byte width of each NAL length prefix in samples.
	NALLengthSize int
	Width         int
	Height        int
	Duration      float64
	// Samples are in decode order.
	Samples []Sample
	// SyncIndices are indices into Samples of the keyframes, ascending.
	SyncIndices []int
}

const headerProbeSize = 16

// ReadIndex locates the moov box by hopping over top-level box headers, reads
// it in one request, and builds the video track's sample index.
func ReadIndex(ctx context.Context, video source.Source) (*VideoTrack, error) {
	moov, err := readMoov(ctx, video)
	if err != nil {
		return nil, err
	}
	moovChildren, err := parseBoxes(moov)
	if err != nil {
		return nil, fmt.Errorf("moov: %w", err)
	}
	for _, candidate := range moovChildren {
		if candidate.kind != "trak" {
			continue
		}
		track, err := parseVideoTrak(candidate.payload)
		if errors.Is(err, errNotVideo) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if len(track.Samples) == 0 && child(moovChildren, "mvex") != nil {
			return nil, errors.New("fragmented MP4 is not supported yet")
		}
		return track, nil
	}
	return nil, errors.New("no H.264/HEVC video track found")
}

// readMoov walks the top-level boxes (ftyp, mdat, moov, ...) reading only
// their headers until it finds moov, wherever it sits in the file.
func readMoov(ctx context.Context, video source.Source) ([]byte, error) {
	var offset int64
	for offset < video.Size() {
		probeLength := min(headerProbeSize, video.Size()-offset)
		headerBytes, err := video.ReadRange(ctx, offset, probeLength)
		if err != nil {
			return nil, err
		}
		header, err := parseBoxHeader(headerBytes)
		if err != nil {
			return nil, fmt.Errorf("top-level box at %d: %w", offset, err)
		}
		if header.runsToEnd {
			header.totalSize = video.Size() - offset
		}
		if header.kind == "moov" {
			payload, err := video.ReadRange(ctx, offset+header.headerSize, header.totalSize-header.headerSize)
			if err != nil {
				return nil, err
			}
			return payload, nil
		}
		offset += header.totalSize
	}
	return nil, errors.New("no moov box: not an MP4, or truncated")
}

var errNotVideo = errors.New("not a video track")

// parseVideoTrak builds a VideoTrack from one trak box, or errNotVideo.
func parseVideoTrak(trak []byte) (*VideoTrack, error) {
	trakChildren, err := parseBoxes(trak)
	if err != nil {
		return nil, err
	}
	handler, err := childPath(trakChildren, "mdia", "hdlr")
	if err != nil {
		return nil, err
	}
	if handlerType(handler.payload) != "vide" {
		return nil, errNotVideo
	}
	mdhd, err := childPath(trakChildren, "mdia", "mdhd")
	if err != nil {
		return nil, err
	}
	timescale := mediaTimescale(mdhd.payload)
	if timescale == 0 {
		return nil, errors.New("mdhd timescale is zero")
	}
	stbl, err := childPath(trakChildren, "mdia", "minf", "stbl")
	if err != nil {
		return nil, err
	}
	stblChildren, err := parseBoxes(stbl.payload)
	if err != nil {
		return nil, err
	}
	track := &VideoTrack{}
	err = parseSampleDescription(stblChildren, track)
	if err != nil {
		return nil, err
	}
	editOffset := editListMediaTime(trakChildren)
	err = buildSamples(stblChildren, timescale, editOffset, track)
	if err != nil {
		return nil, err
	}
	return track, nil
}

// handlerType reads hdlr's handler_type four-character code.
func handlerType(payload []byte) string {
	cursor := &reader{data: payload}
	cursor.fullBoxVersion()
	cursor.skip(4)
	return string(cursor.take(4))
}

// mediaTimescale reads mdhd's timescale, which differs by box version.
func mediaTimescale(payload []byte) uint32 {
	cursor := &reader{data: payload}
	if cursor.fullBoxVersion() == 1 {
		cursor.skip(16)
	} else {
		cursor.skip(8)
	}
	return cursor.u32()
}

// editListMediaTime returns the media time the first non-empty edit starts at,
// in media timescale units; presentation times are shifted back by it.
func editListMediaTime(trakChildren []box) int64 {
	elst, err := childPath(trakChildren, "edts", "elst")
	if err != nil {
		return 0
	}
	cursor := &reader{data: elst.payload}
	version := cursor.fullBoxVersion()
	entryCount := cursor.u32()
	for range entryCount {
		var mediaTime int64
		if version == 1 {
			cursor.skip(8)
			mediaTime = int64(cursor.u64())
		} else {
			cursor.skip(4)
			mediaTime = int64(int32(cursor.u32()))
		}
		cursor.skip(4)
		if cursor.err != nil {
			return 0
		}
		if mediaTime >= 0 {
			return mediaTime
		}
	}
	return 0
}

// sortedSyncIndices lists keyframe sample indices in ascending order.
func sortedSyncIndices(samples []Sample) []int {
	var indices []int
	for index, sample := range samples {
		if sample.Sync {
			indices = append(indices, index)
		}
	}
	sort.Ints(indices)
	return indices
}
