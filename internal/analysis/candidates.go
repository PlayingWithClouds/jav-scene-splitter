// Package analysis finds scene transitions (fades, flashes, dissolves) by
// reading where a video's bytes thin out, then decoding only those spots.
package analysis

import (
	"slices"
	"sort"

	"github.com/PlayingWithClouds/jav-scene-splitter/internal/media"
)

// Window is a stretch of video worth decoding densely.
type Window struct {
	Start float64
	End   float64
	// Reason is "black-keyframe" or "size-dip".
	Reason string
	// Strength orders windows: lower is a stronger signal (dip ratio; 0 for
	// black keyframes).
	Strength float64
}

// CandidateOptions tune which spots of the size profile get decoded.
type CandidateOptions struct {
	// BlackKeyframeRatio: a keyframe under this fraction of the median keyframe
	// size is taken to be black.
	BlackKeyframeRatio float64
	// Probes is how many of the strongest size dips get decoded.
	Probes int
	// MaxDipRatio ignores dips shallower than this fraction of the baseline.
	MaxDipRatio float64
}

const (
	// blackKeyframePadding is decoded on each side of a black keyframe.
	blackKeyframePadding = 1.5
	// dipPadding is added on each side of a dipping bin.
	dipPadding = 0.5
	// baselineSeconds is the radius of the rolling median for per-second bins.
	baselineSeconds = 60
	// neighbourBins is how far away the comparison segments sit for coarse
	// (per-segment) profiles.
	neighbourBins = 2
	// frameLevelDuration: bins shorter than this mean a per-frame profile.
	frameLevelDuration = 0.5
)

// FindCandidates turns a size profile into windows to decode: black keyframes
// (per-frame profiles only) plus the deepest dips in bytes per second.
func FindCandidates(profile []media.SizeBin, options CandidateOptions) []Window {
	var windows []Window
	frameLevel := isFrameLevel(profile)
	if frameLevel {
		windows = append(windows, blackKeyframeWindows(profile, options.BlackKeyframeRatio)...)
	}
	bins := profile
	if frameLevel {
		bins = perSecond(profile)
	}
	windows = append(windows, dipWindows(bins, frameLevel, options)...)
	return mergeWindows(windows)
}

// isFrameLevel reports whether the profile lists single frames rather than
// multi-second segments.
func isFrameLevel(profile []media.SizeBin) bool {
	if len(profile) < 2 {
		return false
	}
	span := profile[len(profile)-1].Start - profile[0].Start
	return span/float64(len(profile)) < frameLevelDuration
}

// blackKeyframeWindows marks every keyframe far smaller than the median one.
func blackKeyframeWindows(profile []media.SizeBin, ratio float64) []Window {
	var keyframeSizes []int64
	for _, bin := range profile {
		if bin.Keyframe {
			keyframeSizes = append(keyframeSizes, bin.Bytes)
		}
	}
	if len(keyframeSizes) == 0 {
		return nil
	}
	threshold := float64(median(keyframeSizes)) * ratio
	var windows []Window
	for _, bin := range profile {
		if !bin.Keyframe || float64(bin.Bytes) >= threshold {
			continue
		}
		windows = append(windows, Window{
			Start:  bin.Start - blackKeyframePadding,
			End:    bin.Start + blackKeyframePadding,
			Reason: "black-keyframe",
		})
	}
	return windows
}

// perSecond sums a per-frame profile into one-second bins.
func perSecond(profile []media.SizeBin) []media.SizeBin {
	last := profile[len(profile)-1].Start
	bins := make([]media.SizeBin, int(last)+1)
	for index := range bins {
		bins[index] = media.SizeBin{Start: float64(index), Duration: 1}
	}
	for _, frame := range profile {
		if frame.Start < 0 {
			continue
		}
		bins[int(frame.Start)].Bytes += frame.Bytes
	}
	return bins
}

// dipWindows scores each bin against its surroundings and keeps the deepest.
// Per-second bins compare with a rolling median (fades are short against a
// long baseline); segments compare with their near neighbours, since a fade
// only thins out part of a segment.
func dipWindows(bins []media.SizeBin, frameLevel bool, options CandidateOptions) []Window {
	ratios := make([]float64, len(bins))
	for index := range bins {
		if frameLevel {
			ratios[index] = ratioToRollingMedian(bins, index, baselineSeconds)
			continue
		}
		ratios[index] = ratioToNeighbours(bins, index, neighbourBins)
	}
	order := make([]int, len(bins))
	for index := range order {
		order[index] = index
	}
	sort.Slice(order, func(first, second int) bool { return ratios[order[first]] < ratios[order[second]] })

	var windows []Window
	for _, index := range order {
		if len(windows) == options.Probes || ratios[index] > options.MaxDipRatio {
			break
		}
		bin := bins[index]
		windows = append(windows, Window{
			Start:    bin.Start - dipPadding,
			End:      bin.Start + bin.Duration + dipPadding,
			Reason:   "size-dip",
			Strength: ratios[index],
		})
	}
	return windows
}

// ratioToRollingMedian divides a bin's bytes by the median of bins within radius.
func ratioToRollingMedian(bins []media.SizeBin, index int, radius int) float64 {
	from, to := max(0, index-radius), min(len(bins), index+radius+1)
	sizes := make([]int64, 0, to-from)
	for _, bin := range bins[from:to] {
		sizes = append(sizes, bin.Bytes)
	}
	baseline := median(sizes)
	if baseline == 0 {
		return 1
	}
	return float64(bins[index].Bytes) / float64(baseline)
}

// ratioToNeighbours divides a bin's bytes by the mean of the bins distance away.
func ratioToNeighbours(bins []media.SizeBin, index int, distance int) float64 {
	before := bins[max(0, index-distance)].Bytes
	after := bins[min(len(bins)-1, index+distance)].Bytes
	baseline := float64(before+after) / 2
	if baseline == 0 {
		return 1
	}
	return float64(bins[index].Bytes) / baseline
}

// mergeWindows unions overlapping windows, keeping the strongest reason.
func mergeWindows(windows []Window) []Window {
	sort.Slice(windows, func(first, second int) bool { return windows[first].Start < windows[second].Start })
	var merged []Window
	for _, window := range windows {
		if len(merged) == 0 || window.Start > merged[len(merged)-1].End {
			merged = append(merged, window)
			continue
		}
		last := &merged[len(merged)-1]
		last.End = max(last.End, window.End)
		if window.Strength < last.Strength || window.Reason == "black-keyframe" {
			last.Strength = min(last.Strength, window.Strength)
			last.Reason = strongerReason(last.Reason, window.Reason)
		}
	}
	return merged
}

// strongerReason prefers the black-keyframe signal over a plain dip.
func strongerReason(first string, second string) string {
	if first == "black-keyframe" || second == "black-keyframe" {
		return "black-keyframe"
	}
	return first
}

// median of a slice of sizes (the slice is not modified).
func median(sizes []int64) int64 {
	if len(sizes) == 0 {
		return 0
	}
	sorted := slices.Clone(sizes)
	slices.Sort(sorted)
	return sorted[len(sorted)/2]
}
