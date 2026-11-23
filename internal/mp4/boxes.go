// Package mp4 reads just enough of an MP4 file to know where every video
// sample lives: the moov box's sample tables and the codec's parameter sets.
// It never touches mdat, so a remote file costs a handful of small requests.
package mp4

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// box is one parsed box: its four-character type and payload (header stripped).
type box struct {
	kind    string
	payload []byte
}

var errTruncated = errors.New("truncated box")

// parseBoxes splits a byte slice into consecutive boxes.
func parseBoxes(data []byte) ([]box, error) {
	var boxes []box
	for len(data) > 0 {
		header, err := parseBoxHeader(data)
		if err != nil {
			return nil, err
		}
		if header.totalSize > int64(len(data)) {
			return nil, fmt.Errorf("%s: %w", header.kind, errTruncated)
		}
		boxes = append(boxes, box{kind: header.kind, payload: data[header.headerSize:header.totalSize]})
		data = data[header.totalSize:]
	}
	return boxes, nil
}

type boxHeader struct {
	kind       string
	headerSize int64
	totalSize  int64
	// runsToEnd marks a size-0 box, which extends to the end of the file.
	runsToEnd bool
}

// parseBoxHeader reads a box header, handling 64-bit sizes. A size of zero
// (box runs to end of data) is resolved by the caller where it matters.
func parseBoxHeader(data []byte) (boxHeader, error) {
	if len(data) < 8 {
		return boxHeader{}, errTruncated
	}
	size := int64(binary.BigEndian.Uint32(data[0:4]))
	header := boxHeader{kind: string(data[4:8]), headerSize: 8, totalSize: size}
	if size == 1 {
		if len(data) < 16 {
			return boxHeader{}, errTruncated
		}
		header.headerSize = 16
		header.totalSize = int64(binary.BigEndian.Uint64(data[8:16]))
	}
	if size == 0 {
		header.totalSize = int64(len(data))
		header.runsToEnd = true
	}
	if header.totalSize < header.headerSize {
		return boxHeader{}, fmt.Errorf("box %q has invalid size %d", header.kind, header.totalSize)
	}
	return header, nil
}

// child returns the first child box of the given type, or nil.
func child(boxes []box, kind string) *box {
	for index := range boxes {
		if boxes[index].kind == kind {
			return &boxes[index]
		}
	}
	return nil
}

// childPath walks nested container boxes, e.g. childPath(trak, "mdia", "minf", "stbl").
func childPath(parent []box, kinds ...string) (*box, error) {
	current := parent
	var found *box
	for _, kind := range kinds {
		found = child(current, kind)
		if found == nil {
			return nil, fmt.Errorf("missing %s box", kind)
		}
		children, err := parseBoxes(found.payload)
		if err != nil && kind != kinds[len(kinds)-1] {
			return nil, err
		}
		current = children
	}
	return found, nil
}

// reader is a bounds-checked big-endian cursor over a box payload.
type reader struct {
	data   []byte
	offset int
	err    error
}

func (cursor *reader) take(count int) []byte {
	if cursor.err != nil {
		return nil
	}
	if cursor.offset+count > len(cursor.data) {
		cursor.err = errTruncated
		return nil
	}
	slice := cursor.data[cursor.offset : cursor.offset+count]
	cursor.offset += count
	return slice
}

func (cursor *reader) skip(count int) { cursor.take(count) }

func (cursor *reader) u8() uint8 {
	bytes := cursor.take(1)
	if bytes == nil {
		return 0
	}
	return bytes[0]
}

func (cursor *reader) u16() uint16 {
	bytes := cursor.take(2)
	if bytes == nil {
		return 0
	}
	return binary.BigEndian.Uint16(bytes)
}

func (cursor *reader) u32() uint32 {
	bytes := cursor.take(4)
	if bytes == nil {
		return 0
	}
	return binary.BigEndian.Uint32(bytes)
}

func (cursor *reader) u64() uint64 {
	bytes := cursor.take(8)
	if bytes == nil {
		return 0
	}
	return binary.BigEndian.Uint64(bytes)
}

// fullBoxVersion reads the version byte and skips the 24-bit flags.
func (cursor *reader) fullBoxVersion() uint8 {
	version := cursor.u8()
	cursor.skip(3)
	return version
}
