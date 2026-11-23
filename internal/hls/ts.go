package hls

import (
	"bytes"
	"errors"
)

const packetSize = 188

// Stream types from the PMT that we can decode.
const (
	streamTypeH264 = 0x1b
	streamTypeHEVC = 0x24
)

// pes is one video PES packet: normally exactly one access unit.
type pes struct {
	// pts is in 90 kHz ticks; hasPTS is false when the header omits it.
	pts    int64
	hasPTS bool
	data   []byte
}

// demuxed is the video elementary stream found in a chunk of transport stream.
type demuxed struct {
	codec string
	// packets are complete PES packets in stream (decode) order.
	packets []pes
}

var errNoVideo = errors.New("no H.264/HEVC video stream in transport stream")

// demuxVideo extracts video PES packets from MPEG-TS bytes. When the chunk is
// only the head of a segment, the last PES is cut off and is dropped unless
// complete is true (the chunk is a whole segment).
func demuxVideo(data []byte, complete bool) (demuxed, error) {
	start := syncOffset(data)
	if start < 0 {
		return demuxed{}, errors.New("no MPEG-TS sync found")
	}
	state := demuxState{programMapPID: -1, videoPID: -1}
	for offset := start; offset+packetSize <= len(data); offset += packetSize {
		packet := data[offset : offset+packetSize]
		if packet[0] != 0x47 {
			continue
		}
		state.handlePacket(packet)
	}
	if state.videoPID < 0 {
		return demuxed{}, errNoVideo
	}
	if complete && state.current != nil {
		state.finish()
	}
	return demuxed{codec: state.codec, packets: state.packets}, nil
}

// syncOffset finds the first position where 0x47 repeats every 188 bytes.
func syncOffset(data []byte) int {
	for offset := 0; offset < min(len(data), packetSize); offset++ {
		if data[offset] != 0x47 {
			continue
		}
		if offset+packetSize >= len(data) || data[offset+packetSize] == 0x47 {
			return offset
		}
	}
	return -1
}

type demuxState struct {
	programMapPID int
	videoPID      int
	codec         string
	current       *bytes.Buffer
	packets       []pes
}

// handlePacket routes one TS packet to PAT, PMT or video PES assembly.
func (state *demuxState) handlePacket(packet []byte) {
	pid := int(packet[1]&0x1f)<<8 | int(packet[2])
	unitStart := packet[1]&0x40 != 0
	payload := packetPayload(packet)
	if payload == nil {
		return
	}
	switch {
	case pid == 0 && unitStart:
		state.programMapPID = parseProgramAssociation(payload)
	case pid == state.programMapPID && unitStart && state.videoPID < 0:
		state.videoPID, state.codec = parseProgramMap(payload)
	case pid == state.videoPID:
		state.appendVideo(payload, unitStart)
	}
}

// appendVideo accumulates PES payload, closing the previous PES at each start.
func (state *demuxState) appendVideo(payload []byte, unitStart bool) {
	if unitStart {
		if state.current != nil {
			state.finish()
		}
		state.current = &bytes.Buffer{}
	}
	if state.current == nil {
		return
	}
	state.current.Write(payload)
}

// finish parses the PES being assembled and stores it.
func (state *demuxState) finish() {
	packet, ok := parsePES(state.current.Bytes())
	state.current = nil
	if ok {
		state.packets = append(state.packets, packet)
	}
}

// packetPayload returns the payload after any adaptation field, or nil.
func packetPayload(packet []byte) []byte {
	control := (packet[3] >> 4) & 0x3
	if control == 0 || control == 2 {
		return nil
	}
	start := 4
	if control == 3 {
		start += 1 + int(packet[4])
	}
	if start >= packetSize {
		return nil
	}
	return packet[start:]
}

// parseProgramAssociation returns the first program's PMT PID, or -1.
func parseProgramAssociation(payload []byte) int {
	section := skipPointer(payload)
	if len(section) < 12 {
		return -1
	}
	sectionLength := int(section[1]&0x0f)<<8 | int(section[2])
	end := min(3+sectionLength-4, len(section))
	for entry := 8; entry+4 <= end; entry += 4 {
		programNumber := int(section[entry])<<8 | int(section[entry+1])
		if programNumber == 0 {
			continue
		}
		return int(section[entry+2]&0x1f)<<8 | int(section[entry+3])
	}
	return -1
}

// parseProgramMap returns the first H.264/HEVC stream's PID and codec name.
func parseProgramMap(payload []byte) (int, string) {
	section := skipPointer(payload)
	if len(section) < 12 {
		return -1, ""
	}
	sectionLength := int(section[1]&0x0f)<<8 | int(section[2])
	end := min(3+sectionLength-4, len(section))
	programInfoLength := int(section[10]&0x0f)<<8 | int(section[11])
	for entry := 12 + programInfoLength; entry+5 <= end; {
		streamType := section[entry]
		pid := int(section[entry+1]&0x1f)<<8 | int(section[entry+2])
		infoLength := int(section[entry+3]&0x0f)<<8 | int(section[entry+4])
		switch streamType {
		case streamTypeH264:
			return pid, "h264"
		case streamTypeHEVC:
			return pid, "hevc"
		}
		entry += 5 + infoLength
	}
	return -1, ""
}

// skipPointer drops the PSI pointer field and any filler before the table.
func skipPointer(payload []byte) []byte {
	if len(payload) == 0 {
		return nil
	}
	start := 1 + int(payload[0])
	if start >= len(payload) {
		return nil
	}
	return payload[start:]
}

// parsePES strips the PES header and reads the PTS.
func parsePES(data []byte) (pes, bool) {
	if len(data) < 9 || data[0] != 0 || data[1] != 0 || data[2] != 1 {
		return pes{}, false
	}
	headerLength := int(data[8])
	if 9+headerLength > len(data) {
		return pes{}, false
	}
	packet := pes{data: data[9+headerLength:]}
	if data[7]&0x80 != 0 && headerLength >= 5 {
		packet.pts = readTimestamp(data[9:14])
		packet.hasPTS = true
	}
	return packet, true
}

// readTimestamp decodes the 33-bit PTS packed into five bytes.
func readTimestamp(field []byte) int64 {
	return int64(field[0]>>1&0x07)<<30 |
		int64(field[1])<<22 | int64(field[2]>>1)<<15 |
		int64(field[3])<<7 | int64(field[4]>>1)
}
