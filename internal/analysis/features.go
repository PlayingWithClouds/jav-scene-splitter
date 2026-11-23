// Package analysis finds scene boundaries from cheap per-frame image
// statistics: colour histograms, a coarse brightness grid, and black/white
// frame detection. No learned models.
package analysis

import (
	"math"

	"github.com/PlayingWithClouds/jav-scene-splitter/internal/decode"
)

const (
	histogramBins = 16
	gridColumns   = 8
	gridRows      = 4
)

// Features summarise one thumbnail.
type Features struct {
	// Luma is mean brightness in [0, 1]; LumaDeviation its standard deviation.
	Luma          float64
	LumaDeviation float64
	// Histogram holds a normalised 16-bin histogram per RGB channel.
	Histogram [3 * histogramBins]float64
	// Grid is mean brightness of an 8x4 grid of cells, in [0, 1].
	Grid [gridColumns * gridRows]float64
}

// Black reports a near-uniform dark frame, as seen during fades between scenes.
func (features Features) Black() bool {
	return features.Luma < 0.07 && features.LumaDeviation < 0.05
}

// White reports a near-uniform bright frame (flash or white title card).
func (features Features) White() bool {
	return features.Luma > 0.92 && features.LumaDeviation < 0.05
}

// Blank reports a frame that carries no picture content.
func (features Features) Blank() bool {
	return features.Black() || features.White()
}

// Compute derives Features from an RGB24 thumbnail.
func Compute(frame decode.Frame) Features {
	var features Features
	var lumaSum, lumaSquareSum float64
	var gridSums [gridColumns * gridRows]float64
	pixelCount := float64(decode.Width * decode.Height)
	cellPixels := pixelCount / float64(gridColumns*gridRows)

	for pixel := range decode.Width * decode.Height {
		red, green, blue := frame[pixel*3], frame[pixel*3+1], frame[pixel*3+2]
		features.Histogram[int(red)*histogramBins/256]++
		features.Histogram[histogramBins+int(green)*histogramBins/256]++
		features.Histogram[2*histogramBins+int(blue)*histogramBins/256]++

		luma := (0.299*float64(red) + 0.587*float64(green) + 0.114*float64(blue)) / 255
		lumaSum += luma
		lumaSquareSum += luma * luma
		gridSums[gridCell(pixel)] += luma
	}

	for bin := range features.Histogram {
		features.Histogram[bin] /= pixelCount
	}
	for cell := range gridSums {
		features.Grid[cell] = gridSums[cell] / cellPixels
	}
	features.Luma = lumaSum / pixelCount
	variance := lumaSquareSum/pixelCount - features.Luma*features.Luma
	features.LumaDeviation = math.Sqrt(max(variance, 0))
	return features
}

// gridCell maps a pixel index to its cell in the coarse brightness grid.
func gridCell(pixel int) int {
	column := (pixel % decode.Width) * gridColumns / decode.Width
	row := (pixel / decode.Width) * gridRows / decode.Height
	return row*gridColumns + column
}

// Distance is a [0, 1] dissimilarity between two frames: half colour
// distribution, half spatial layout of brightness.
func Distance(first Features, second Features) float64 {
	return 0.5*histogramDistance(first.Histogram, second.Histogram) + 0.5*gridDistance(first, second)
}

// histogramDistance is the total variation distance averaged over channels.
func histogramDistance(first [3 * histogramBins]float64, second [3 * histogramBins]float64) float64 {
	var total float64
	for bin := range first {
		total += math.Abs(first[bin] - second[bin])
	}
	return total / 6
}

// gridDistance is the mean absolute brightness change per cell, scaled so a
// typical hard cut lands near 1.
func gridDistance(first Features, second Features) float64 {
	var total float64
	for cell := range first.Grid {
		total += math.Abs(first.Grid[cell] - second.Grid[cell])
	}
	return math.Min(1, 3*total/float64(len(first.Grid)))
}
