package hls

import "bytes"

// nalUnits splits an Annex-B buffer at its start codes.
func nalUnits(stream []byte) [][]byte {
	var units [][]byte
	position := 0
	for {
		start := nextStartCode(stream, position)
		if start < 0 {
			return units
		}
		payloadStart := start + 3
		next := nextStartCode(stream, payloadStart)
		end := len(stream)
		if next >= 0 {
			end = next
			if end > payloadStart && stream[end-1] == 0 {
				end--
			}
		}
		if end > payloadStart {
			units = append(units, stream[payloadStart:end])
		}
		if next < 0 {
			return units
		}
		position = next
	}
}

// nextStartCode finds the next 00 00 01 at or after from, or -1.
func nextStartCode(stream []byte, from int) int {
	if from >= len(stream) {
		return -1
	}
	index := bytes.Index(stream[from:], []byte{0, 0, 1})
	if index < 0 {
		return -1
	}
	return from + index
}

// nalKind classifies a NAL unit header for the codec.
type nalKind int

const (
	nalOther nalKind = iota
	nalKeyframe
	nalParameterSet
)

// classifyNAL tells keyframe slices and parameter sets apart from the rest.
func classifyNAL(codec string, nal []byte) nalKind {
	if len(nal) == 0 {
		return nalOther
	}
	if codec == "hevc" {
		nalType := (nal[0] >> 1) & 0x3f
		if nalType >= 16 && nalType <= 21 {
			return nalKeyframe
		}
		if nalType >= 32 && nalType <= 34 {
			return nalParameterSet
		}
		return nalOther
	}
	nalType := nal[0] & 0x1f
	if nalType == 5 {
		return nalKeyframe
	}
	if nalType == 7 || nalType == 8 {
		return nalParameterSet
	}
	return nalOther
}

// isKeyframe reports whether an access unit contains a keyframe slice.
func isKeyframe(codec string, unit []byte) bool {
	for _, nal := range nalUnits(unit) {
		if classifyNAL(codec, nal) == nalKeyframe {
			return true
		}
	}
	return false
}

// withParameterSets prefixes the last-seen parameter sets to a keyframe unit
// that does not carry its own.
func withParameterSets(codec string, unit []byte, parameterSets [][]byte) []byte {
	for _, nal := range nalUnits(unit) {
		if classifyNAL(codec, nal) == nalParameterSet {
			return unit
		}
	}
	var prefixed bytes.Buffer
	for _, parameterSet := range parameterSets {
		prefixed.Write([]byte{0, 0, 0, 1})
		prefixed.Write(parameterSet)
	}
	prefixed.Write(unit)
	return prefixed.Bytes()
}

// collectParameterSets returns the parameter set NALs within a unit.
func collectParameterSets(codec string, unit []byte) [][]byte {
	var sets [][]byte
	for _, nal := range nalUnits(unit) {
		if classifyNAL(codec, nal) == nalParameterSet {
			sets = append(sets, nal)
		}
	}
	return sets
}
