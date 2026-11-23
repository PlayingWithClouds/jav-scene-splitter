package analysis

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"

	"github.com/PlayingWithClouds/jav-scene-splitter/internal/decode"
	"github.com/PlayingWithClouds/jav-scene-splitter/internal/media"
)

// Boundary is a confirmed scene transition.
type Boundary struct {
	// Time is the boundary in seconds: the middle of the black (or white) for
	// fades and flashes, the middle of the blend for dissolves.
	Time float64
	// Transition is "fade" (through black), "flash" (through white) or
	// "dissolve" (gradual blend between two shots).
	Transition string
	// BlankFrom and BlankTo bound the blank stretch, for fades and flashes;
	// BlankTo is when the new scene's first picture appears.
	BlankFrom float64
	BlankTo   float64
	// Window is the candidate this was found in.
	Window Window
	// Detail describes the measurements behind the call, for tuning.
	Detail string
}

// Dissolve thresholds on mean absolute pixel difference (0..1) of thumbnails.
const (
	// dissolveStep: no single frame step inside a dissolve may exceed this
	// (a larger step is a hard cut).
	dissolveStep = 0.05
	// dissolveChange: the blend's two ends must differ at least this much.
	dissolveChange = 0.12
	// dissolveResidual: every in-between frame must lie this close to a
	// linear blend of the two ends (rules out camera pans and zooms).
	dissolveResidual  = 0.03
	minDissolveFrames = 5
	maxDissolveTime   = 2.5
	// minimumBlankFrames is how many consecutive blank frames make a fade.
	minimumBlankFrames = 2
	// maxSpanSeconds caps one dense decode.
	maxSpanSeconds = 20.0
)

// Inspector decodes candidate windows and classifies what happens in them.
type Inspector struct {
	video     media.Video
	keyframes []media.Keyframe
}

// NewInspector prepares dense decoding over the video's keyframes.
func NewInspector(video media.Video) *Inspector {
	return &Inspector{video: video, keyframes: video.Keyframes()}
}

// InspectAll inspects windows in parallel and returns the confirmed
// boundaries, in time order.
func (inspector *Inspector) InspectAll(ctx context.Context, windows []Window, workers int) []Boundary {
	results := make([]*Boundary, len(windows))
	jobs := make(chan int)
	var waitGroup sync.WaitGroup
	for range workers {
		waitGroup.Go(func() {
			for index := range jobs {
				boundary, found, err := inspector.Inspect(ctx, windows[index])
				if err != nil || !found {
					continue
				}
				results[index] = &boundary
			}
		})
	}
	for index := range windows {
		if ctx.Err() != nil {
			break
		}
		jobs <- index
	}
	close(jobs)
	waitGroup.Wait()

	var boundaries []Boundary
	for _, result := range results {
		if result != nil {
			boundaries = append(boundaries, *result)
		}
	}
	sort.Slice(boundaries, func(first, second int) bool { return boundaries[first].Time < boundaries[second].Time })
	return boundaries
}

// Inspect decodes every frame of a window (widened to keyframes) and looks for
// a fade, flash or dissolve. A fade that runs past the end of the decoded span
// is followed into the next keyframe interval once.
func (inspector *Inspector) Inspect(ctx context.Context, window Window) (Boundary, bool, error) {
	from, to := inspector.keyframeRange(window.Start, window.End)
	if from == to {
		return Boundary{}, false, errors.New("window covers no keyframe interval")
	}
	frames, times, err := inspector.decodeRange(ctx, from, to)
	if err != nil {
		return Boundary{}, false, err
	}
	blankStart, blankEnd := longestBlankRun(frames)
	if blankEnd-blankStart >= minimumBlankFrames && blankEnd == len(frames) && to+1 < len(inspector.keyframes) {
		moreFrames, moreTimes, err := inspector.decodeRange(ctx, to, to+1)
		if err == nil {
			frames, times = append(frames, moreFrames...), append(times, moreTimes...)
			blankStart, blankEnd = longestBlankRun(frames)
		}
	}

	if blankEnd-blankStart >= minimumBlankFrames {
		blankFrom, blankTo := times[blankStart], times[min(blankEnd, len(times)-1)]
		return Boundary{
			Transition: blankKind(frames[blankStart].features),
			BlankFrom:  blankFrom,
			BlankTo:    blankTo,
			Time:       (blankFrom + blankTo) / 2,
			Window:     window,
		}, true, nil
	}
	dissolve, found := findDissolve(frames, times)
	if found {
		detail := fmt.Sprintf("change %.3f over %d frames (%.2fs), worst residual %.3f, largest step %.3f",
			dissolve.change, dissolve.frames, dissolve.duration, dissolve.residual, dissolve.largestStep)
		return Boundary{Transition: "dissolve", Time: dissolve.time, Window: window, Detail: detail}, true, nil
	}
	return Boundary{}, false, nil
}

// keyframeRange returns keyframe indices [from, to) whose interval covers
// [start, end], capped to maxSpanSeconds.
func (inspector *Inspector) keyframeRange(start float64, end float64) (int, int) {
	from := sort.Search(len(inspector.keyframes), func(index int) bool { return inspector.keyframes[index].Time > start }) - 1
	from = max(from, 0)
	to := sort.Search(len(inspector.keyframes), func(index int) bool { return inspector.keyframes[index].Time >= end })
	to = min(max(to, from+1), len(inspector.keyframes)-1)
	for to > from+1 && inspector.keyframes[to].Time-inspector.keyframes[from].Time > maxSpanSeconds {
		to--
	}
	return from, to
}

// thumbnail is one decoded frame with its features.
type thumbnail struct {
	pixels   decode.Frame
	features Features
}

// decodeRange fetches and decodes all frames from keyframe from up to (not
// including) keyframe to.
func (inspector *Inspector) decodeRange(ctx context.Context, from int, to int) ([]thumbnail, []float64, error) {
	span, err := inspector.video.Span(ctx, inspector.keyframes[from], inspector.keyframes[to])
	if err != nil {
		return nil, nil, err
	}
	decoded, err := decode.Run(ctx, inspector.video.Codec(), span.Units)
	if err != nil {
		return nil, nil, err
	}
	count := min(len(decoded), len(span.Times))
	frames := make([]thumbnail, count)
	for index := range frames {
		frames[index] = thumbnail{pixels: decoded[index], features: Compute(decoded[index])}
	}
	return frames, span.Times[:count], nil
}

// longestBlankRun returns [start, end) of the longest run of blank frames.
func longestBlankRun(frames []thumbnail) (int, int) {
	bestStart, bestEnd := 0, 0
	runStart := -1
	for index := 0; index <= len(frames); index++ {
		blank := index < len(frames) && frames[index].features.Blank()
		if blank && runStart < 0 {
			runStart = index
		}
		if blank || runStart < 0 {
			continue
		}
		if index-runStart > bestEnd-bestStart {
			bestStart, bestEnd = runStart, index
		}
		runStart = -1
	}
	return bestStart, bestEnd
}

// blankKind names the transition a blank frame belongs to.
func blankKind(features Features) string {
	if features.White() {
		return "flash"
	}
	return "fade"
}

// dissolveMatch describes the strongest blend found in a window.
type dissolveMatch struct {
	time        float64
	change      float64
	frames      int
	duration    float64
	residual    float64
	largestStep float64
}

// findDissolve looks for the strongest gradual blend: a stretch of frames with
// no hard step, ends that differ a lot, and every frame in between close to a
// linear mix of the ends. Its time is the blend's midpoint.
func findDissolve(frames []thumbnail, times []float64) (dissolveMatch, bool) {
	steps := make([]float64, len(frames))
	for index := 1; index < len(frames); index++ {
		steps[index] = pixelDifference(frames[index-1].pixels, frames[index].pixels)
	}
	var best dissolveMatch
	for first := range frames {
		largestStep := 0.0
		for last := first + 1; last < len(frames); last++ {
			if steps[last] > dissolveStep || times[last]-times[first] > maxDissolveTime {
				break
			}
			largestStep = max(largestStep, steps[last])
			if last-first < minDissolveFrames {
				continue
			}
			change := pixelDifference(frames[first].pixels, frames[last].pixels)
			if change < dissolveChange || change <= best.change {
				continue
			}
			residual := worstBlendResidual(frames[first : last+1])
			if residual > dissolveResidual {
				continue
			}
			best = dissolveMatch{
				time: times[(first+last)/2], change: change, frames: last - first + 1,
				duration: times[last] - times[first], residual: residual, largestStep: largestStep,
			}
		}
	}
	return best, best.change > 0
}

// worstBlendResidual is the largest misfit of any inner frame against the best
// mix of the two ends.
func worstBlendResidual(frames []thumbnail) float64 {
	start, end := frames[0].pixels, frames[len(frames)-1].pixels
	worst := 0.0
	for _, frame := range frames[1 : len(frames)-1] {
		worst = max(worst, blendResidual(start, end, frame.pixels))
	}
	return worst
}

// blendResidual fits frame ≈ start + alpha·(end − start) by least squares and
// returns the mean absolute error of that fit, in 0..1.
func blendResidual(start decode.Frame, end decode.Frame, frame decode.Frame) float64 {
	var numerator, denominator float64
	for index := range frame {
		direction := float64(end[index]) - float64(start[index])
		numerator += (float64(frame[index]) - float64(start[index])) * direction
		denominator += direction * direction
	}
	alpha := 0.0
	if denominator > 0 {
		alpha = math.Max(0, math.Min(1, numerator/denominator))
	}
	var residual float64
	for index := range frame {
		predicted := float64(start[index]) + alpha*(float64(end[index])-float64(start[index]))
		residual += math.Abs(float64(frame[index]) - predicted)
	}
	return residual / float64(len(frame)) / 255
}

// pixelDifference is the mean absolute difference of two thumbnails, in 0..1.
func pixelDifference(first decode.Frame, second decode.Frame) float64 {
	var total float64
	for index := range first {
		total += math.Abs(float64(first[index]) - float64(second[index]))
	}
	return total / float64(len(first)) / 255
}
