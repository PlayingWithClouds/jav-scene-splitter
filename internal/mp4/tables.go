package mp4

import (
	"errors"
	"fmt"
)

// visualSampleEntryFields is the fixed part of a VisualSampleEntry before its
// child boxes (avcC/hvcC, pasp, ...).
const visualSampleEntryFields = 78

// parseSampleDescription reads the codec, dimensions and parameter sets from stsd.
func parseSampleDescription(stblChildren []box, track *VideoTrack) error {
	stsd := child(stblChildren, "stsd")
	if stsd == nil {
		return errors.New("missing stsd box")
	}
	if len(stsd.payload) < 8 {
		return errTruncated
	}
	entries, err := parseBoxes(stsd.payload[8:])
	if err != nil || len(entries) == 0 {
		return fmt.Errorf("stsd entries: %w", err)
	}
	entry := entries[0]
	if len(entry.payload) < visualSampleEntryFields {
		return errTruncated
	}
	cursor := &reader{data: entry.payload}
	cursor.skip(24)
	track.Width = int(cursor.u16())
	track.Height = int(cursor.u16())
	configBoxes, err := parseBoxes(entry.payload[visualSampleEntryFields:])
	if err != nil {
		return fmt.Errorf("%s config: %w", entry.kind, err)
	}
	switch entry.kind {
	case "avc1", "avc3":
		track.Codec = "h264"
		return parseAVCConfig(child(configBoxes, "avcC"), track)
	case "hvc1", "hev1":
		track.Codec = "hevc"
		return parseHEVCConfig(child(configBoxes, "hvcC"), track)
	}
	return fmt.Errorf("unsupported video codec %q (need H.264 or HEVC)", entry.kind)
}

// parseAVCConfig reads the NAL length size and SPS/PPS from an avcC box.
func parseAVCConfig(avcC *box, track *VideoTrack) error {
	if avcC == nil {
		return errors.New("missing avcC box")
	}
	cursor := &reader{data: avcC.payload}
	cursor.skip(4)
	track.NALLengthSize = int(cursor.u8()&0x03) + 1
	sequenceSetCount := int(cursor.u8() & 0x1f)
	track.ParameterSets = append(track.ParameterSets, readLengthPrefixedNALs(cursor, sequenceSetCount)...)
	pictureSetCount := int(cursor.u8())
	track.ParameterSets = append(track.ParameterSets, readLengthPrefixedNALs(cursor, pictureSetCount)...)
	return cursor.err
}

// parseHEVCConfig reads the NAL length size and VPS/SPS/PPS from an hvcC box.
func parseHEVCConfig(hvcC *box, track *VideoTrack) error {
	if hvcC == nil {
		return errors.New("missing hvcC box")
	}
	cursor := &reader{data: hvcC.payload}
	cursor.skip(21)
	track.NALLengthSize = int(cursor.u8()&0x03) + 1
	arrayCount := int(cursor.u8())
	for range arrayCount {
		cursor.skip(1)
		nalCount := int(cursor.u16())
		track.ParameterSets = append(track.ParameterSets, readLengthPrefixedNALs(cursor, nalCount)...)
	}
	return cursor.err
}

// readLengthPrefixedNALs reads count NAL units each preceded by a 16-bit length.
func readLengthPrefixedNALs(cursor *reader, count int) [][]byte {
	var nals [][]byte
	for range count {
		length := int(cursor.u16())
		nal := cursor.take(length)
		if nal == nil {
			return nals
		}
		nals = append(nals, nal)
	}
	return nals
}

// buildSamples combines stts/ctts/stss/stsz/stsc/stco into a flat sample list.
func buildSamples(stblChildren []box, timescale uint32, editOffset int64, track *VideoTrack) error {
	sizes, err := sampleSizes(stblChildren)
	if err != nil {
		return err
	}
	offsets, err := sampleOffsets(stblChildren, len(sizes))
	if err != nil {
		return err
	}
	decodeTimes, err := decodeTimestamps(stblChildren, len(sizes))
	if err != nil {
		return err
	}
	compositionOffsets := compositionTimeOffsets(stblChildren, len(sizes))
	syncSet := syncSamples(stblChildren)

	samples := make([]Sample, len(sizes))
	var lastPTS float64
	for index := range samples {
		presentation := decodeTimes[index] + compositionOffsets[index] - editOffset
		samples[index] = Sample{
			Offset: offsets[index],
			Size:   sizes[index],
			PTS:    float64(presentation) / float64(timescale),
			Sync:   syncSet == nil || syncSet[index],
		}
		lastPTS = max(lastPTS, samples[index].PTS)
	}
	track.Samples = samples
	track.SyncIndices = sortedSyncIndices(samples)
	track.Duration = lastPTS
	return nil
}

// sampleSizes reads stsz (or the compact stz2 is rejected as unsupported).
func sampleSizes(stblChildren []box) ([]int64, error) {
	stsz := child(stblChildren, "stsz")
	if stsz == nil {
		return nil, errors.New("missing stsz box (stz2 unsupported)")
	}
	cursor := &reader{data: stsz.payload}
	cursor.fullBoxVersion()
	constantSize := cursor.u32()
	count := int(cursor.u32())
	sizes := make([]int64, count)
	for index := range sizes {
		if constantSize != 0 {
			sizes[index] = int64(constantSize)
			continue
		}
		sizes[index] = int64(cursor.u32())
	}
	return sizes, cursor.err
}

// sampleOffsets maps each sample to its absolute file offset via stsc + stco/co64.
func sampleOffsets(stblChildren []box, sampleCount int) ([]int64, error) {
	chunkOffsets, err := chunkOffsetTable(stblChildren)
	if err != nil {
		return nil, err
	}
	stsc := child(stblChildren, "stsc")
	if stsc == nil {
		return nil, errors.New("missing stsc box")
	}
	type run struct{ firstChunk, samplesPerChunk int }
	cursor := &reader{data: stsc.payload}
	cursor.fullBoxVersion()
	runCount := int(cursor.u32())
	runs := make([]run, runCount)
	for index := range runs {
		runs[index] = run{firstChunk: int(cursor.u32()), samplesPerChunk: int(cursor.u32())}
		cursor.skip(4)
	}
	if cursor.err != nil {
		return nil, fmt.Errorf("stsc: %w", cursor.err)
	}
	sizes, err := sampleSizes(stblChildren)
	if err != nil {
		return nil, err
	}

	offsets := make([]int64, 0, sampleCount)
	for runIndex, current := range runs {
		lastChunk := len(chunkOffsets)
		if runIndex+1 < len(runs) {
			lastChunk = runs[runIndex+1].firstChunk - 1
		}
		for chunk := current.firstChunk; chunk <= lastChunk && chunk-1 < len(chunkOffsets); chunk++ {
			position := chunkOffsets[chunk-1]
			for range current.samplesPerChunk {
				if len(offsets) == sampleCount {
					return offsets, nil
				}
				offsets = append(offsets, position)
				position += sizes[len(offsets)-1]
			}
		}
	}
	if len(offsets) != sampleCount {
		return nil, fmt.Errorf("chunk table covers %d of %d samples", len(offsets), sampleCount)
	}
	return offsets, nil
}

// chunkOffsetTable reads stco (32-bit) or co64 (64-bit) chunk offsets.
func chunkOffsetTable(stblChildren []box) ([]int64, error) {
	wide := false
	table := child(stblChildren, "stco")
	if table == nil {
		table = child(stblChildren, "co64")
		wide = true
	}
	if table == nil {
		return nil, errors.New("missing stco/co64 box")
	}
	cursor := &reader{data: table.payload}
	cursor.fullBoxVersion()
	offsets := make([]int64, cursor.u32())
	for index := range offsets {
		if wide {
			offsets[index] = int64(cursor.u64())
			continue
		}
		offsets[index] = int64(cursor.u32())
	}
	return offsets, cursor.err
}

// decodeTimestamps expands stts runs into one decode time per sample.
func decodeTimestamps(stblChildren []box, sampleCount int) ([]int64, error) {
	stts := child(stblChildren, "stts")
	if stts == nil {
		return nil, errors.New("missing stts box")
	}
	cursor := &reader{data: stts.payload}
	cursor.fullBoxVersion()
	runCount := int(cursor.u32())
	times := make([]int64, 0, sampleCount)
	var clock int64
	for range runCount {
		count := int(cursor.u32())
		delta := int64(cursor.u32())
		for range count {
			if len(times) == sampleCount {
				break
			}
			times = append(times, clock)
			clock += delta
		}
	}
	if cursor.err != nil || len(times) != sampleCount {
		return nil, fmt.Errorf("stts covers %d of %d samples", len(times), sampleCount)
	}
	return times, nil
}

// compositionTimeOffsets expands ctts runs; all zero when the box is absent.
func compositionTimeOffsets(stblChildren []box, sampleCount int) []int64 {
	offsets := make([]int64, sampleCount)
	ctts := child(stblChildren, "ctts")
	if ctts == nil {
		return offsets
	}
	cursor := &reader{data: ctts.payload}
	cursor.fullBoxVersion()
	runCount := int(cursor.u32())
	position := 0
	for range runCount {
		count := int(cursor.u32())
		offset := int64(int32(cursor.u32()))
		for range count {
			if position == sampleCount || cursor.err != nil {
				return offsets
			}
			offsets[position] = offset
			position++
		}
	}
	return offsets
}

// syncSamples returns the set of keyframe indices from stss, or nil when every
// sample is a keyframe (no stss box).
func syncSamples(stblChildren []box) map[int]bool {
	stss := child(stblChildren, "stss")
	if stss == nil {
		return nil
	}
	cursor := &reader{data: stss.payload}
	cursor.fullBoxVersion()
	count := int(cursor.u32())
	set := make(map[int]bool, count)
	for range count {
		sampleNumber := int(cursor.u32())
		if cursor.err != nil {
			break
		}
		set[sampleNumber-1] = true
	}
	return set
}
