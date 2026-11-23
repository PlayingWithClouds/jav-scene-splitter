package main

import (
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/PlayingWithClouds/jav-scene-splitter/internal/analysis"
)

// truthTolerance is how far (seconds) a detected boundary may sit from a known
// one and still count as found.
const truthTolerance = 5.0

// parseTimestamps reads a comma-separated list of H:MM:SS, MM:SS or seconds.
func parseTimestamps(list string) ([]float64, error) {
	var times []float64
	for field := range strings.SplitSeq(list, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		seconds, err := parseTimestamp(field)
		if err != nil {
			return nil, err
		}
		times = append(times, seconds)
	}
	sort.Float64s(times)
	return times, nil
}

// parseTimestamp converts H:MM:SS(.mmm), MM:SS or plain seconds.
func parseTimestamp(text string) (float64, error) {
	var seconds float64
	for part := range strings.SplitSeq(text, ":") {
		value, err := strconv.ParseFloat(part, 64)
		if err != nil {
			return 0, fmt.Errorf("bad timestamp %q", text)
		}
		seconds = seconds*60 + value
	}
	return seconds, nil
}

// reportAgainstTruth prints, for every known boundary, the matching detection
// or why it was missed, then lists detections that match nothing.
func reportAgainstTruth(boundaries []analysis.Boundary, truth []float64, windows []analysis.Window) {
	matched := make([]bool, len(boundaries))
	found := 0
	fmt.Fprintln(os.Stderr, "\ntruth        detected      error    transition  found-by")
	for _, known := range truth {
		nearest := nearestBoundary(boundaries, known)
		if nearest < 0 || math.Abs(boundaries[nearest].Time-known) > truthTolerance {
			fmt.Fprintf(os.Stderr, "%-12s missed: %s\n", formatTimestamp(known), missReason(windows, known))
			continue
		}
		matched[nearest] = true
		found++
		boundary := boundaries[nearest]
		fmt.Fprintf(os.Stderr, "%-12s %-13s %+6.1fs  %-11s %s  %s\n", formatTimestamp(known),
			formatTimestamp(boundary.Time), boundary.Time-known, boundary.Transition, boundary.Window.Reason, blankDetail(boundary, known))
	}
	for index, boundary := range boundaries {
		if !matched[index] {
			fmt.Fprintf(os.Stderr, "false positive at %s (%s, %s) %s\n", formatTimestamp(boundary.Time), boundary.Transition, boundary.Window.Reason, boundary.Detail)
		}
	}
	fmt.Fprintf(os.Stderr, "found %d/%d, %d false positives, %d windows decoded\n", found, len(truth), len(boundaries)-found, len(windows))
}

// blankDetail shows where a fade's black starts relative to the known time,
// or the dissolve measurements.
func blankDetail(boundary analysis.Boundary, known float64) string {
	if boundary.Transition == "dissolve" {
		return boundary.Detail
	}
	return fmt.Sprintf("black from %s (%+.1fs)", formatTimestamp(boundary.BlankFrom), boundary.BlankFrom-known)
}

// missReason says whether a missed boundary was never a candidate, or was
// decoded without a transition being confirmed.
func missReason(windows []analysis.Window, known float64) string {
	for _, window := range windows {
		if known >= window.Start-truthTolerance && known <= window.End+truthTolerance {
			return fmt.Sprintf("decoded (%s, ratio %.2f) but no transition confirmed", window.Reason, window.Strength)
		}
	}
	return "never a candidate"
}

// nearestBoundary returns the index of the boundary closest to time, or -1.
func nearestBoundary(boundaries []analysis.Boundary, time float64) int {
	nearest := -1
	for index, boundary := range boundaries {
		if nearest < 0 || math.Abs(boundary.Time-time) < math.Abs(boundaries[nearest].Time-time) {
			nearest = index
		}
	}
	return nearest
}
