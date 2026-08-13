// Package h264 provides small, dependency-free helpers for inspecting
// Annex-B H.264/H.265 NAL unit streams - codec/profile sniffing and
// keyframe detection. No decoding; byte-level inspection only.
package h264

import "fmt"

// IterateAnnexB splits an Annex-B byte stream (0x000001 start codes) into
// individual NAL unit payloads (start code stripped).
func IterateAnnexB(data []byte) [][]byte {
	var starts []int
	for i := 0; i+2 < len(data); i++ {
		if data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 {
			starts = append(starts, i+3)
		}
	}
	var nals [][]byte
	for k, s := range starts {
		end := len(data)
		if k+1 < len(starts) {
			end = starts[k+1] - 3
			// starts[k+1]-3 is where the next start code's literal "00 00 01" bytes begin. If the
			// byte(s) immediately before that are also 0x00, the next NAL's on-wire start code was
			// actually 4-byte (00 00 00 01) or had extra zero_byte padding -- that padding belongs
			// to the next start code, not this NAL's payload, so strip it back to the real boundary.
			for end > s && data[end-1] == 0 {
				end--
			}
			if end < s {
				end = s
			}
		}
		if s < end {
			nals = append(nals, data[s:end])
		}
	}
	return nals
}

// NALType returns an H.264 NAL unit's nal_unit_type (low 5 bits of the
// first byte). Only meaningful for H.264; H.265 uses a different layout
// (see IsHEVCKeyframe).
func NALType(nal []byte) byte {
	if len(nal) == 0 {
		return 0
	}
	return nal[0] & 0x1F
}

// DetectCodec inspects an Annex-B NAL stream for an SPS unit to determine
// whether the elementary stream is H.264 or H.265.
func DetectCodec(data []byte) (codec string, ok bool) {
	for _, nal := range IterateAnnexB(data) {
		if len(nal) == 0 {
			continue
		}
		if nal[0]&0x1F == 7 { // H.264 SPS
			return "h264", true
		}
		if (nal[0]>>1)&0x3F == 33 { // H.265 SPS
			return "hevc", true
		}
	}
	return "", false
}

// ProfileLevel reads profile_idc/constraint-flags/level_idc directly - the
// first 3 bytes of seq_parameter_set_rbsp() per ITU-T H.264 §7.3.2.1.1,
// immediately after the 1-byte NAL header - no further RBSP/exp-golomb
// parsing needed.
func ProfileLevel(data []byte) (profileIDC, constraintFlags, levelIDC byte, ok bool) {
	for _, nal := range IterateAnnexB(data) {
		if len(nal) >= 4 && nal[0]&0x1F == 7 {
			return nal[1], nal[2], nal[3], true
		}
	}
	return 0, 0, 0, false
}

// CodecString derives the exact `avc1.PPCCLL` string WebCodecs'
// VideoDecoderConfig.codec (and MSE's addSourceBuffer, historically) needs,
// by scanning data for an H.264 SPS. Falls back to this project's
// known-Baseline-profile camera default if no SPS is present in data.
func CodecString(data []byte) string {
	if p, c, l, ok := ProfileLevel(data); ok {
		return fmt.Sprintf("avc1.%02X%02X%02X", p, c, l)
	}
	return "avc1.42C01E" // fallback: this camera's known Baseline profile
}

// IsKeyframe reports whether data (one Annex-B access unit) contains an
// H.264 IDR slice (nal_unit_type 5) - used to flag a WebCodecs
// EncodedVideoChunk as "key" vs "delta".
func IsKeyframe(data []byte) bool {
	for _, nal := range IterateAnnexB(data) {
		if NALType(nal) == 5 {
			return true
		}
	}
	return false
}
