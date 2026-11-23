package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/PlayingWithClouds/jav-scene-splitter/internal/analysis"
	"github.com/PlayingWithClouds/jav-scene-splitter/internal/media"
)

// Result is the JSON written for one video.
type Result struct {
	Source       string  `json:"source"`
	Duration     float64 `json:"duration"`
	Scenes       []Scene `json:"scenes"`
	BytesFetched int64   `json:"bytes_fetched"`
	Elapsed      float64 `json:"elapsed_seconds"`
}

// Scene is one span of the video. Start/End are seconds; the Timestamp
// fields repeat them as H:MM:SS.mmm for reading.
type Scene struct {
	Index          int     `json:"index"`
	Start          float64 `json:"start"`
	End            float64 `json:"end"`
	StartTimestamp string  `json:"start_timestamp"`
	EndTimestamp   string  `json:"end_timestamp"`
	// Transition into this scene: "fade", "flash", "dissolve", or "" for the first.
	Transition string  `json:"transition,omitempty"`
	BlankFrom  float64 `json:"blank_from,omitempty"`
	BlankTo    float64 `json:"blank_to,omitempty"`
	// FoundBy is the size signal that pointed at it: "black-keyframe" or "size-dip".
	FoundBy string `json:"found_by,omitempty"`
}

// buildResult turns boundaries into consecutive scenes covering the video.
func buildResult(location string, video media.Video, boundaries []analysis.Boundary, elapsed time.Duration) Result {
	result := Result{
		Source:       location,
		Duration:     roundMillis(video.Duration()),
		BytesFetched: video.BytesFetched(),
		Elapsed:      math.Round(elapsed.Seconds()*10) / 10,
	}
	start := 0.0
	previous := analysis.Boundary{}
	for index := range len(boundaries) + 1 {
		end := video.Duration()
		if index < len(boundaries) {
			end = boundaries[index].Time
		}
		scene := Scene{
			Index:          index + 1,
			Start:          roundMillis(start),
			End:            roundMillis(end),
			StartTimestamp: formatTimestamp(start),
			EndTimestamp:   formatTimestamp(end),
		}
		if index > 0 {
			scene.Transition = previous.Transition
			scene.BlankFrom = roundMillis(previous.BlankFrom)
			scene.BlankTo = roundMillis(previous.BlankTo)
			scene.FoundBy = previous.Window.Reason
		}
		result.Scenes = append(result.Scenes, scene)
		if index < len(boundaries) {
			start, previous = end, boundaries[index]
		}
	}
	return result
}

// writeResult writes indented JSON to path, or stdout when path is empty.
func writeResult(path string, result Result) error {
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	if path == "" {
		_, err = os.Stdout.Write(encoded)
		return err
	}
	return os.WriteFile(path, encoded, 0o644)
}

// writeChapters writes ffmetadata chapters, usable with
// `ffmpeg -i in.mp4 -i chapters.txt -map_metadata 1 -codec copy out.mp4`.
func writeChapters(path string, result Result) error {
	var builder strings.Builder
	builder.WriteString(";FFMETADATA1\n")
	for _, scene := range result.Scenes {
		fmt.Fprintf(&builder, "\n[CHAPTER]\nTIMEBASE=1/1000\nSTART=%d\nEND=%d\ntitle=Scene %d\n",
			int64(scene.Start*1000), int64(scene.End*1000), scene.Index)
	}
	return os.WriteFile(path, []byte(builder.String()), 0o644)
}

// writeSizesCSV dumps the size profile: start, duration, bytes per bin.
func writeSizesCSV(path string, profile []media.SizeBin) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	writer := csv.NewWriter(file)
	writer.Write([]string{"start", "duration", "bytes"})
	for _, bin := range profile {
		writer.Write([]string{
			strconv.FormatFloat(bin.Start, 'f', 3, 64),
			strconv.FormatFloat(bin.Duration, 'f', 3, 64),
			strconv.FormatInt(bin.Bytes, 10),
		})
	}
	writer.Flush()
	return writer.Error()
}

// formatTimestamp renders seconds as H:MM:SS.mmm.
func formatTimestamp(seconds float64) string {
	totalMillis := int64(math.Round(seconds * 1000))
	hours := totalMillis / 3_600_000
	minutes := totalMillis / 60_000 % 60
	wholeSeconds := totalMillis / 1000 % 60
	millis := totalMillis % 1000
	return fmt.Sprintf("%d:%02d:%02d.%03d", hours, minutes, wholeSeconds, millis)
}

// roundMillis rounds seconds to millisecond precision for the JSON.
func roundMillis(seconds float64) float64 {
	return math.Round(seconds*1000) / 1000
}
