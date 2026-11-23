// Package decode turns Annex-B access units into tiny RGB thumbnails by piping
// them through ffmpeg as a raw elementary stream.
package decode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
)

// Thumbnail dimensions. Small enough that decoding cost is dominated by the
// codec itself; large enough for histograms and a coarse spatial grid.
const (
	Width  = 64
	Height = 36
)

// FrameBytes is the size of one RGB24 thumbnail.
const FrameBytes = Width * Height * 3

// Frame is one decoded RGB24 thumbnail, row-major.
type Frame []byte

// Keyframes decodes independent, self-contained keyframes in one ffmpeg run.
// The result has one frame per unit, in input order; a unit that fails to
// decode yields a nil frame.
func Keyframes(ctx context.Context, codec string, units [][]byte) ([]Frame, error) {
	frames, err := runFFmpeg(ctx, codec, bytes.Join(units, nil))
	if err != nil {
		return nil, err
	}
	if len(frames) == len(units) {
		return frames, nil
	}
	return keyframesOneByOne(ctx, codec, units), nil
}

// keyframesOneByOne is the slow fallback when a batch decodes to the wrong
// frame count (a corrupt keyframe would otherwise shift every later frame).
func keyframesOneByOne(ctx context.Context, codec string, units [][]byte) []Frame {
	frames := make([]Frame, len(units))
	for index, unit := range units {
		decoded, err := runFFmpeg(ctx, codec, unit)
		if err != nil || len(decoded) == 0 {
			continue
		}
		frames[index] = decoded[0]
	}
	return frames
}

// Run decodes a contiguous run of access units that starts at a keyframe.
// Frames come back in presentation order, as ffmpeg emits them.
func Run(ctx context.Context, codec string, units [][]byte) ([]Frame, error) {
	return runFFmpeg(ctx, codec, bytes.Join(units, nil))
}

// runFFmpeg decodes an Annex-B stream and returns every frame it produces.
func runFFmpeg(ctx context.Context, codec string, stream []byte) ([]Frame, error) {
	scale := "scale=" + strconv.Itoa(Width) + ":" + strconv.Itoa(Height) + ":flags=area"
	command := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error", "-nostdin",
		"-f", codec, "-i", "pipe:0",
		"-an", "-sn", "-vf", scale,
		"-fps_mode", "passthrough",
		"-pix_fmt", "rgb24", "-f", "rawvideo", "pipe:1",
	)
	command.Stdin = bytes.NewReader(stream)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	err = command.Start()
	if err != nil {
		return nil, fmt.Errorf("start ffmpeg: %w", err)
	}
	frames, readError := readFrames(stdout)
	waitError := command.Wait()
	if readError != nil {
		return nil, readError
	}
	if waitError != nil && len(frames) == 0 {
		return nil, fmt.Errorf("ffmpeg: %w: %s", waitError, stderr.String())
	}
	return frames, nil
}

// readFrames splits ffmpeg's rawvideo output into fixed-size frames.
func readFrames(output io.Reader) ([]Frame, error) {
	var frames []Frame
	for {
		frame := make(Frame, FrameBytes)
		_, err := io.ReadFull(output, frame)
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return frames, nil
		}
		if err != nil {
			return nil, err
		}
		frames = append(frames, frame)
	}
}
