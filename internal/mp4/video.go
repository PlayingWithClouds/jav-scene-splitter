package mp4

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"

	"github.com/PlayingWithClouds/jav-scene-splitter/internal/media"
	"github.com/PlayingWithClouds/jav-scene-splitter/internal/source"
)

// Video adapts an indexed MP4 file to media.Video.
type Video struct {
	bytes source.Source
	track *VideoTrack
}

// Open indexes an MP4 from any byte source.
func Open(ctx context.Context, bytes source.Source) (*Video, error) {
	track, err := ReadIndex(ctx, bytes)
	if err != nil {
		return nil, err
	}
	return &Video{bytes: bytes, track: track}, nil
}

func (video *Video) Codec() string       { return video.track.Codec }
func (video *Video) Duration() float64   { return video.track.Duration }
func (video *Video) BytesFetched() int64 { return video.bytes.BytesRead() }
func (video *Video) Close() error        { return video.bytes.Close() }

// Describe summarises codec, size and keyframe count.
func (video *Video) Describe() string {
	return fmt.Sprintf("MP4 %s %dx%d, %d keyframes, %.1f MB file",
		video.track.Codec, video.track.Width, video.track.Height,
		len(video.track.SyncIndices), float64(video.bytes.Size())/1e6)
}

// Keyframes lists every sync sample.
func (video *Video) Keyframes() []media.Keyframe {
	keyframes := make([]media.Keyframe, 0, len(video.track.SyncIndices))
	for _, sampleIndex := range video.track.SyncIndices {
		keyframes = append(keyframes, media.Keyframe{Time: video.track.Samples[sampleIndex].PTS, Position: sampleIndex})
	}
	return keyframes
}

// SizeProfile returns every frame's size in presentation order. It costs
// nothing: the sizes come from the sample table already in memory.
func (video *Video) SizeProfile(_ context.Context, _ int) ([]media.SizeBin, error) {
	samples := append([]Sample(nil), video.track.Samples...)
	sort.Slice(samples, func(first, second int) bool { return samples[first].PTS < samples[second].PTS })
	bins := make([]media.SizeBin, len(samples))
	for index, sample := range samples {
		duration := 0.0
		if index+1 < len(samples) {
			duration = samples[index+1].PTS - sample.PTS
		}
		bins[index] = media.SizeBin{Start: sample.PTS, Duration: duration, Bytes: sample.Size, Keyframe: sample.Sync}
	}
	return bins, nil
}

// KeyframeUnit fetches one sync sample and prefixes the parameter sets.
func (video *Video) KeyframeUnit(ctx context.Context, keyframe media.Keyframe) ([]byte, error) {
	sample := video.track.Samples[keyframe.Position]
	data, err := video.bytes.ReadRange(ctx, sample.Offset, sample.Size)
	if err != nil {
		return nil, err
	}
	var unit bytes.Buffer
	video.writeParameterSets(&unit)
	err = writeAnnexB(&unit, data, video.track.NALLengthSize)
	if err != nil {
		return nil, err
	}
	return unit.Bytes(), nil
}

// Span fetches all samples between two keyframes with one range request.
func (video *Video) Span(ctx context.Context, from media.Keyframe, to media.Keyframe) (media.Span, error) {
	samples := video.track.Samples[from.Position:to.Position]
	if len(samples) == 0 {
		return media.Span{}, errors.New("empty span")
	}
	startOffset, endOffset := samples[0].Offset, samples[0].Offset+samples[0].Size
	for _, sample := range samples {
		startOffset = min(startOffset, sample.Offset)
		endOffset = max(endOffset, sample.Offset+sample.Size)
	}
	data, err := video.bytes.ReadRange(ctx, startOffset, endOffset-startOffset)
	if err != nil {
		return media.Span{}, err
	}

	span := media.Span{Units: make([][]byte, len(samples)), Times: make([]float64, len(samples))}
	for index, sample := range samples {
		var unit bytes.Buffer
		if index == 0 {
			video.writeParameterSets(&unit)
		}
		relative := sample.Offset - startOffset
		err = writeAnnexB(&unit, data[relative:relative+sample.Size], video.track.NALLengthSize)
		if err != nil {
			return media.Span{}, err
		}
		span.Units[index] = unit.Bytes()
		span.Times[index] = sample.PTS
	}
	sort.Float64s(span.Times)
	return span, nil
}

var startCode = []byte{0, 0, 0, 1}

// writeParameterSets writes SPS/PPS (and VPS) with start codes.
func (video *Video) writeParameterSets(stream *bytes.Buffer) {
	for _, parameterSet := range video.track.ParameterSets {
		stream.Write(startCode)
		stream.Write(parameterSet)
	}
}

// writeAnnexB converts one length-prefixed MP4 sample into start-code NALs.
func writeAnnexB(stream *bytes.Buffer, sample []byte, lengthSize int) error {
	for len(sample) > 0 {
		if len(sample) < lengthSize {
			return errors.New("sample ends inside a NAL length prefix")
		}
		length := nalLength(sample[:lengthSize])
		sample = sample[lengthSize:]
		if length > len(sample) {
			return fmt.Errorf("NAL length %d exceeds remaining %d bytes", length, len(sample))
		}
		stream.Write(startCode)
		stream.Write(sample[:length])
		sample = sample[length:]
	}
	return nil
}

// nalLength reads a 1-, 2- or 4-byte big-endian NAL length prefix.
func nalLength(prefix []byte) int {
	switch len(prefix) {
	case 1:
		return int(prefix[0])
	case 2:
		return int(binary.BigEndian.Uint16(prefix))
	}
	return int(binary.BigEndian.Uint32(prefix))
}
