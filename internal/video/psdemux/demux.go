// Package psdemux incrementally parses HCNetSDK's private-protocol
// (byProtoType=0) live stream data - verified against a real device
// (iDS-TCM203-A) by hex-dumping its NET_DVR_STREAMDATA callback buffers -
// to be genuine, if minimal, MPEG Program Stream / PES framing per
// ITU-T H.222.0 | ISO/IEC 13818-1: a pack_header (start code 0xBA),
// typically one program_stream_map (0xBC) declaring the PES stream_id ->
// codec mapping, and PES packets (0xE0-0xEF for video).
//
// One important, non-obvious, empirically-verified property of this
// device's packetizer: PES packet boundaries do NOT align with NAL/access
// unit boundaries. A large NAL (a keyframe's IDR slice easily exceeds the
// ~5.1KB payload this camera puts in one PES packet) is split across
// several consecutive video PES packets - only the first carries a fresh
// Annex-B start code, the rest are raw continuation bytes. So this package
// reassembles access units itself: it watches for the next PES payload
// that begins with a fresh Annex-B start code introducing a VCL slice NAL
// (H.264 type 1 or 5) and treats that as "the previous access unit is
// complete, this begins the next one" - the same kind of inference RTP
// H.264 depacketization (RFC 6184) makes from marker bits, just derived
// here from the elementary stream's own emulation-prevented start codes
// (which, per the H.264 spec's emulation-prevention byte, cannot occur
// coincidentally inside real NAL payload data - so this is exact, not a
// heuristic).
//
// This is deliberately NOT a general MPEG-PS demuxer: no multi-program
// support, no PS_program_stream_directory, no trick-mode/copy-protection
// PES extension fields, no attempt to interpret PTS/DTS. It extracts only
// video access units (stream_id 0xE0-0xEF) and discards everything else
// (pack/system headers, the PSM, audio/private PES) - none of which this
// camera's private protocol has been observed to need. Fed straight to a
// browser's WebCodecs VideoDecoder, no ffmpeg/container remux required.
package psdemux

import (
	"encoding/binary"

	"hik-cam-app/internal/video/h264"
)

// maxRawBufferBytes bounds buf's growth if the input never produces a
// recognized PS-level start code (corrupt stream, wrong channel, etc.).
// maxPendingBytes bounds the in-progress access unit's growth if a new VCL
// NAL start is never seen (stream stalls mid-frame). Both generous above
// what's actually been observed (single access units up to ~50KB across
// ~10 PES packets for a 1080p substream IDR - mainstream IDRs run larger
// given its higher resolution/bitrate, but stay well under these caps) so
// real data is never at risk of being truncated by either cap.
const (
	maxRawBufferBytes = 4 << 20 // 4MiB
	maxPendingBytes   = 8 << 20 // 8MiB
)

// Unit is one complete, reassembled H.264 access unit, ready to hand to a
// WebCodecs EncodedVideoChunk with no further framing. H.264 only: appendPayload
// finds access unit boundaries using H.264 VCL NAL types, so an H.265 stream is
// misframed here rather than rejected - see internal/video/h264's package doc.
type Unit struct {
	// Data is Annex-B (0x000001/0x00000001 start codes), owned by the
	// caller - safe to retain past the next Feed call.
	Data []byte
	// Keyframe is true if Data contains an IDR slice (H.264 NAL type 5).
	Keyframe bool
}

// Demuxer holds the incremental parse state for one live stream session.
// Not safe for concurrent use.
type Demuxer struct {
	buf []byte // raw incoming bytes not yet parsed into PS-level elements

	pending       []byte // access unit currently being reassembled
	pendingHasVCL bool   // whether pending already contains a VCL slice NAL
}

// New returns a Demuxer ready to Feed.
func New() *Demuxer {
	return &Demuxer{}
}

// Feed appends data (one NET_DVR_STREAMDATA/STREAMAUDIODATA callback
// buffer) to the demuxer's state and returns any access units completed
// as a result - typically zero or one per call, since completing an
// access unit requires seeing the start of the *next* one (see package
// doc), which usually arrives in a later Feed call. data is not retained
// past this call.
func (d *Demuxer) Feed(data []byte) []Unit {
	d.buf = append(d.buf, data...)

	var units []Unit
	for len(d.buf) > 0 {
		consumed, payload, hasPayload := parseOne(d.buf)
		if consumed == 0 {
			break // not enough buffered data yet to make progress
		}
		d.buf = d.buf[consumed:]
		if hasPayload {
			if u := d.appendPayload(payload); u != nil {
				units = append(units, *u)
			}
		}
	}

	if len(d.buf) > maxRawBufferBytes {
		// Pathological: no recognizable start code across a huge span.
		// Drop the oldest half and let resync (see parseOne's default
		// case) find the next real boundary in what remains.
		d.buf = append([]byte(nil), d.buf[len(d.buf)/2:]...)
	}
	if len(d.pending) > maxPendingBytes {
		// Pathological: never saw the start of a following access unit.
		// Drop what we have rather than growing forever; the stream
		// will resync cleanly at the next real VCL NAL start.
		d.pending = nil
		d.pendingHasVCL = false
	}
	return units
}

// Flush returns whatever access unit is still being reassembled, if any -
// call at stream end (RealPlay's Frames channel closing) to not silently
// drop the final frame, which otherwise only completes once the *next*
// one starts.
func (d *Demuxer) Flush() *Unit {
	if len(d.pending) == 0 || !d.pendingHasVCL {
		return nil
	}
	u := &Unit{Data: d.pending, Keyframe: h264.IsKeyframe(d.pending)}
	d.pending = nil
	d.pendingHasVCL = false
	return u
}

// appendPayload feeds one extracted video-PES payload (raw bytes; may or
// may not begin with a fresh Annex-B start code - see package doc) into
// the access unit being reassembled, flushing the previous one first if
// this payload marks the start of a new one.
//
// The boundary signal is "a fresh NAL starts here AND pending already has
// a VCL slice" - not "a fresh VCL slice starts here". The camera resends
// SPS/PPS ahead of every keyframe, not just once at session start; those
// aren't VCL NALs, but their arrival after a slice was already accumulated
// still means the previous access unit is complete and a new one (SPS,
// PPS, then its IDR slice) is beginning - exactly the same conclusion an
// SEI arriving in that position would imply. A fresh NAL of any type
// arriving before any VCL has been seen yet (SPS/PPS at session start, or
// immediately after the previous flush) is instead the leading material of
// the access unit still being built, so it's appended without flushing.
func (d *Demuxer) appendPayload(payload []byte) (flushed *Unit) {
	nalType, isFreshNAL := leadingNALType(payload)
	if isFreshNAL {
		if d.pendingHasVCL {
			flushed = &Unit{Data: d.pending, Keyframe: h264.IsKeyframe(d.pending)}
			d.pending = nil
			d.pendingHasVCL = false
		}
		if nalType == 1 || nalType == 5 {
			d.pendingHasVCL = true
		}
	}
	d.pending = append(d.pending, payload...)
	return flushed
}

// leadingNALType reports the H.264 nal_unit_type of the NAL immediately
// following a fresh Annex-B start code at the very front of payload, and
// whether payload begins with a start code at all (false for the raw
// continuation chunks that make up the bulk of a large NAL - see package
// doc; those never begin with a start code by construction, since Annex-B
// emulation prevention guarantees 0x000001 cannot occur coincidentally
// inside real NAL payload bytes).
func leadingNALType(payload []byte) (nalType byte, ok bool) {
	var nalStart int
	switch {
	case len(payload) >= 4 && payload[0] == 0 && payload[1] == 0 && payload[2] == 0 && payload[3] == 1:
		nalStart = 4
	case len(payload) >= 3 && payload[0] == 0 && payload[1] == 0 && payload[2] == 1:
		nalStart = 3
	default:
		return 0, false
	}
	if nalStart >= len(payload) {
		return 0, false
	}
	return payload[nalStart] & 0x1F, true
}

// parseOne tries to consume exactly one PS-level element from the front of
// buf. Returns consumed=0 if buf doesn't yet hold enough bytes to know how
// much to consume (caller should wait for more data via the next Feed).
// hasPayload is true only when the consumed element was a video PES packet
// whose elementary-stream bytes were successfully extracted into payload -
// note this is a raw PES payload chunk, not necessarily a complete access
// unit (see appendPayload).
func parseOne(buf []byte) (consumed int, payload []byte, hasPayload bool) {
	if len(buf) < 3 {
		return 0, nil, false
	}
	if !(buf[0] == 0 && buf[1] == 0 && buf[2] == 1) {
		// Not positioned on a start code - either leading noise (first
		// Feed of a session) or we've lost sync. Scan forward for the
		// next 0x000001 and drop everything before it. Cheap in the
		// common case: after any successful parse we land exactly on
		// the next real start code, so this branch is rarely taken.
		idx := indexStartCode(buf[1:])
		if idx < 0 {
			// No start code anywhere in the buffered tail. Keep the
			// last 2 bytes (a start code could straddle the next Feed)
			// and drop the rest.
			if len(buf) > 2 {
				return len(buf) - 2, nil, false
			}
			return 0, nil, false
		}
		return idx + 1, nil, false
	}
	if len(buf) < 4 {
		return 0, nil, false // need the marker byte
	}

	marker := buf[3]
	switch {
	case marker == 0xB9: // MPEG_program_end_code - no payload
		return 4, nil, false

	case marker == 0xBA: // pack_start_code: pack_header
		if len(buf) < 14 {
			return 0, nil, false
		}
		stuffingLen := int(buf[13] & 0x07)
		total := 14 + stuffingLen
		if len(buf) < total {
			return 0, nil, false
		}
		return total, nil, false

	case marker == 0xBB || marker == 0xBC:
		// system_header_start_code / program_stream_map: both are
		// [4-byte start code][16-bit length][length bytes].
		return skipLengthPrefixed(buf)

	case marker >= 0xE0 && marker <= 0xEF:
		// Video PES packet - extract its ES payload.
		return parseVideoPES(buf)

	case marker == 0xBD || marker == 0xBF || (marker >= 0xC0 && marker <= 0xDF):
		// private_stream_1, private_stream_2, or audio PES - same
		// [start code][16-bit length][data] envelope; we don't need
		// the contents, so just skip the whole packet by length.
		return skipLengthPrefixed(buf)

	default:
		// Not a start code we recognize at PS level - almost certainly
		// a coincidental 0x000001 inside a NAL/PES payload we've
		// already accounted for elsewhere. Advance one byte and let
		// the outer loop re-scan from there.
		return 1, nil, false
	}
}

// skipLengthPrefixed consumes a [4-byte start code][16-bit big-endian
// length][length bytes] element without interpreting its contents -
// covers system_header, program_stream_map, and any PES packet whose
// payload this demuxer doesn't need (audio, private streams).
func skipLengthPrefixed(buf []byte) (consumed int, payload []byte, hasPayload bool) {
	if len(buf) < 6 {
		return 0, nil, false
	}
	length := int(binary.BigEndian.Uint16(buf[4:6]))
	if length == 0 {
		return skipUnboundedPES(buf, false)
	}
	total := 6 + length
	if len(buf) < total {
		return 0, nil, false
	}
	return total, nil, false
}

// parseVideoPES consumes one video PES packet (stream_id 0xE0-0xEF) and
// extracts its elementary-stream payload per the standard PES header
// layout:
//
//	[0:3]  packet_start_code_prefix (00 00 01)
//	[3]    stream_id
//	[4:6]  PES_packet_length (big-endian; bytes following this field)
//	[6]    '10' + scrambling/priority/alignment/copyright flags
//	[7]    PTS_DTS_flags + ESCR/ES_rate/trick-mode/copy-info/CRC/ext flags
//	[8]    PES_header_data_length (N) - bytes of optional fields+stuffing
//	[9:9+N] optional fields (PTS/DTS/etc. if flagged) + stuffing - skipped
//	[9+N:]  elementary stream payload, up to 6+PES_packet_length
//
// The extracted payload is a raw chunk of the elementary stream, NOT
// necessarily a complete access unit - see appendPayload.
func parseVideoPES(buf []byte) (consumed int, payload []byte, hasPayload bool) {
	if len(buf) < 9 {
		return 0, nil, false
	}
	length := int(binary.BigEndian.Uint16(buf[4:6]))
	if length == 0 {
		return skipUnboundedPES(buf, true)
	}
	total := 6 + length
	if len(buf) < total {
		return 0, nil, false
	}

	headerDataLen := int(buf[8])
	payloadStart := 9 + headerDataLen
	if payloadStart > total {
		// Malformed header_data_length - skip the packet without
		// emitting rather than slicing out of range.
		return total, nil, false
	}
	p := buf[payloadStart:total]
	if len(p) == 0 {
		return total, nil, false
	}
	return total, append([]byte(nil), p...), true
}

// skipUnboundedPES handles the rare (never observed against real hardware
// in this project - see the package doc) PES_packet_length==0 case, which
// per spec means "length not known when the header was written; extends to
// the next start code." Falls back to scanning forward for the next
// recognizable PS-level marker; if none is buffered yet, waits for more
// data. wantPayload controls whether the skipped bytes should be returned
// (true for video PES, false for the non-video callers of
// skipLengthPrefixed, which don't need the contents). This is inherently
// best-effort (a coincidental start-code-like pattern could false-trigger
// early), acceptable only because there is no evidence this device ever
// takes this path - the fixed-length case (verified against real capture
// data) is what actually runs in practice.
func skipUnboundedPES(buf []byte, wantPayload bool) (consumed int, payload []byte, hasPayload bool) {
	const fixedHeader = 9
	if len(buf) < fixedHeader {
		return 0, nil, false
	}
	headerDataLen := int(buf[8])
	payloadStart := fixedHeader + headerDataLen
	if len(buf) < payloadStart {
		return 0, nil, false
	}
	rest := buf[payloadStart:]
	idx := indexNextMarker(rest)
	if idx < 0 {
		if len(buf) > maxRawBufferBytes/2 {
			// Give up waiting - treat everything buffered so far as
			// the payload rather than stalling the stream forever.
			if !wantPayload || len(rest) == 0 {
				return len(buf), nil, false
			}
			return len(buf), append([]byte(nil), rest...), true
		}
		return 0, nil, false
	}
	total := payloadStart + idx
	if !wantPayload || idx == 0 {
		return total, nil, false
	}
	return total, append([]byte(nil), rest[:idx]...), true
}

// indexStartCode returns the index of the first 0x000001 start-code prefix
// in buf, or -1 if none is present.
func indexStartCode(buf []byte) int {
	for i := 0; i+2 < len(buf); i++ {
		if buf[i] == 0 && buf[i+1] == 0 && buf[i+2] == 1 {
			return i
		}
	}
	return -1
}

// indexNextMarker returns the index of the next start code in buf whose
// marker byte looks like a genuine PS-level unit (pack/system
// header/PSM/PES range) rather than data - used only by the unbounded-PES
// fallback (see skipUnboundedPES).
func indexNextMarker(buf []byte) int {
	for i := 0; i+3 < len(buf); i++ {
		if buf[i] != 0 || buf[i+1] != 0 || buf[i+2] != 1 {
			continue
		}
		m := buf[i+3]
		if m == 0xB9 || m == 0xBA || m == 0xBB || m == 0xBC || m == 0xBD || m == 0xBF || m >= 0xC0 {
			return i
		}
	}
	return -1
}
