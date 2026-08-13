package h264

import "testing"

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestIterateAnnexB_FourByteStartCodeDoesNotLeakTrailingZero is the regression test for the bug
// reported 2026-08-10: a NAL boundary computed as "next start code's position minus 3" assumed the
// next start code was always the 3-byte 00 00 01 form. When it was actually the 4-byte 00 00 00 01
// form, the extra leading zero byte belonged to the next start code, not this NAL's payload, but
// used to get included in the returned slice anyway.
func TestIterateAnnexB_FourByteStartCodeDoesNotLeakTrailingZero(t *testing.T) {
	data := []byte{
		0x00, 0x00, 0x01, // 3-byte start code
		0x67, 0xAA, 0xBB, // NAL 0 payload (SPS, nal_unit_type 7)
		0x00, 0x00, 0x00, 0x01, // 4-byte start code
		0x41, 0xCC, // NAL 1 payload (nal_unit_type 1)
	}
	nals := IterateAnnexB(data)
	if len(nals) != 2 {
		t.Fatalf("expected 2 NALs, got %d: %v", len(nals), nals)
	}
	if !bytesEqual(nals[0], []byte{0x67, 0xAA, 0xBB}) {
		t.Errorf("NAL 0: expected [67 AA BB], got %X -- trailing zero byte from the next NAL's 4-byte start code leaked in", nals[0])
	}
	if !bytesEqual(nals[1], []byte{0x41, 0xCC}) {
		t.Errorf("NAL 1: expected [41 CC], got %X", nals[1])
	}
}

// TestIterateAnnexB_ThreeByteStartCodesUnaffected guards the common case the fix must not disturb.
func TestIterateAnnexB_ThreeByteStartCodesUnaffected(t *testing.T) {
	data := []byte{
		0x00, 0x00, 0x01, 0x67, 0xAA, 0xBB,
		0x00, 0x00, 0x01, 0x41, 0xCC,
	}
	nals := IterateAnnexB(data)
	if len(nals) != 2 {
		t.Fatalf("expected 2 NALs, got %d: %v", len(nals), nals)
	}
	if !bytesEqual(nals[0], []byte{0x67, 0xAA, 0xBB}) {
		t.Errorf("NAL 0: expected [67 AA BB], got %X", nals[0])
	}
	if !bytesEqual(nals[1], []byte{0x41, 0xCC}) {
		t.Errorf("NAL 1: expected [41 CC], got %X", nals[1])
	}
}

func TestProfileLevel_FindsSPSAfterFourByteStartCode(t *testing.T) {
	data := []byte{
		0x00, 0x00, 0x00, 0x01, // 4-byte start code
		0x67, 0x42, 0xC0, 0x1E, 0xFF, // SPS: profile=0x42 constraints=0xC0 level=0x1E
	}
	p, c, l, ok := ProfileLevel(data)
	if !ok {
		t.Fatal("expected ProfileLevel to find the SPS")
	}
	if p != 0x42 || c != 0xC0 || l != 0x1E {
		t.Errorf("got profile=%02X constraints=%02X level=%02X, want 42 C0 1E", p, c, l)
	}
	if got := CodecString(data); got != "avc1.42C01E" {
		t.Errorf("CodecString = %q, want avc1.42C01E", got)
	}
}
