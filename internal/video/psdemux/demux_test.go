package psdemux

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"os"
	"testing"
)

// loadFixtures reads testdata/frames.b64 - 80 consecutive NET_DVR_STREAMDATA
// callback buffers captured from a live iDS-TCM203-A camera (see this
// repo's git history for the capture diagnostic), in arrival order. Index 0
// is the StreamSysHead (never fed to the demuxer - see package doc); the
// rest span a pack_header+PSM, an SPS/PPS-only PES, and multiple full
// access units - some spanning many PES packets (a keyframe's IDR slice),
// some spanning few (P-frames) - exactly the mixed shape the demuxer has to
// handle correctly.
func loadFixtures(t *testing.T) [][]byte {
	t.Helper()
	f, err := os.Open("testdata/frames.b64")
	if err != nil {
		t.Fatalf("open fixtures: %v", err)
	}
	defer f.Close()

	var frames [][]byte
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(line)
		if err != nil {
			t.Fatalf("decode fixture line: %v", err)
		}
		frames = append(frames, raw)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan fixtures: %v", err)
	}
	if len(frames) < 40 {
		t.Fatalf("expected at least 40 fixture frames, got %d", len(frames))
	}
	return frames
}

// feedAll drives a fresh Demuxer with every fixture frame except the
// leading StreamSysHead, then Flush()es the trailing in-progress unit -
// mirrors exactly how stream.go will drive it against the live SDK.
func feedAll(frames [][]byte) []Unit {
	d := New()
	var units []Unit
	for _, f := range frames[1:] {
		units = append(units, d.Feed(f)...)
	}
	if u := d.Flush(); u != nil {
		units = append(units, *u)
	}
	return units
}

func TestFeedRealCapture_ReassemblesMultiPacketAccessUnits(t *testing.T) {
	frames := loadFixtures(t)
	units := feedAll(frames)

	if len(units) < 5 {
		t.Fatalf("got %d access units from 80 raw frames, want several", len(units))
	}

	var sawKeyframe, sawMultiPacketUnit bool
	for i, u := range units {
		if len(u.Data) == 0 {
			t.Errorf("unit %d is empty", i)
		}
		if !bytes.HasPrefix(u.Data, []byte{0, 0, 0, 1}) && !bytes.HasPrefix(u.Data, []byte{0, 0, 1}) {
			t.Errorf("unit %d does not start with an Annex-B start code: %x", i, u.Data[:min(8, len(u.Data))])
		}
		if u.Keyframe {
			sawKeyframe = true
		}
		// A raw PES payload chunk from this camera is capped around
		// 5.1KB (see package doc); any unit bigger than that must have
		// been reassembled from multiple PES packets.
		if len(u.Data) > 6000 {
			sawMultiPacketUnit = true
		}
	}
	if !sawKeyframe {
		t.Error("no unit was flagged as a keyframe across the whole capture - expected at least one IDR")
	}
	if !sawMultiPacketUnit {
		t.Error("no unit exceeded one PES packet's worth of payload - expected at least one multi-packet (keyframe) access unit given the capture includes a run of 5108-byte continuation buffers")
	}

	// Every byte from every video PES packet across the whole capture
	// must end up in exactly one unit, in order - reassembly must not
	// drop, duplicate, or reorder data. Independently recompute the
	// expected concatenation by re-parsing PES packets directly, without
	// going through appendPayload's access-unit grouping at all, and
	// compare byte-for-byte (not just total length, which alone wouldn't
	// catch reordering/corruption).
	var gotConcat []byte
	for _, u := range units {
		gotConcat = append(gotConcat, u.Data...)
	}
	want := concatVideoPESPayloads(t, frames[1:])
	if !bytes.Equal(gotConcat, want) {
		t.Errorf("reassembled units concatenated (%d bytes) != ground-truth PES payload concatenation (%d bytes) - bytes lost, duplicated, or reordered", len(gotConcat), len(want))
	}
}

// concatVideoPESPayloads independently re-parses every video PES packet in
// frames (concatenated) and returns their extracted payloads concatenated
// in order, as ground truth to check reassembly preserved exact byte
// content and ordering, not just total length.
func concatVideoPESPayloads(t *testing.T, frames [][]byte) []byte {
	t.Helper()
	var all []byte
	for _, f := range frames {
		all = append(all, f...)
	}
	var out []byte
	for len(all) > 0 {
		consumed, payload, hasPayload := parseOne(all)
		if consumed == 0 {
			break
		}
		if hasPayload {
			out = append(out, payload...)
		}
		all = all[consumed:]
	}
	return out
}

// TestFeedRealCapture_ByteAtATime proves the incremental parser is truly
// alignment-independent: feeding one byte at a time (worst-case
// fragmentation - never happens via the real SDK callback, but must not
// break the parser) must extract byte-identical units to feeding whole
// buffers at once.
func TestFeedRealCapture_ByteAtATime(t *testing.T) {
	frames := loadFixtures(t)
	var all []byte
	for _, f := range frames[1:] {
		all = append(all, f...)
	}

	whole := New()
	wantUnits := whole.Feed(all)
	if u := whole.Flush(); u != nil {
		wantUnits = append(wantUnits, *u)
	}

	fragmented := New()
	var gotUnits []Unit
	for i := 0; i < len(all); i++ {
		gotUnits = append(gotUnits, fragmented.Feed(all[i:i+1])...)
	}
	if u := fragmented.Flush(); u != nil {
		gotUnits = append(gotUnits, *u)
	}

	if len(gotUnits) != len(wantUnits) {
		t.Fatalf("byte-at-a-time produced %d units, whole-buffer produced %d", len(gotUnits), len(wantUnits))
	}
	for i := range wantUnits {
		if !bytes.Equal(gotUnits[i].Data, wantUnits[i].Data) {
			t.Errorf("unit %d differs between byte-at-a-time and whole-buffer feeding", i)
		}
		if gotUnits[i].Keyframe != wantUnits[i].Keyframe {
			t.Errorf("unit %d Keyframe mismatch: byte-at-a-time=%v whole-buffer=%v", i, gotUnits[i].Keyframe, wantUnits[i].Keyframe)
		}
	}
}

// TestFeedRealCapture_SplitAcrossFeedBoundary proves a PES packet whose
// bytes straddle two separate Feed() calls (simulating chunking that
// doesn't align to callback buffers) is still parsed correctly.
func TestFeedRealCapture_SplitAcrossFeedBoundary(t *testing.T) {
	frames := loadFixtures(t)
	// frames[3] is the first fixture after the pack+PSM/SPS-PPS pair -
	// a full 5108-byte continuation-carrying PES packet.
	idrFrame := frames[3]

	whole := New()
	wantUnits := whole.Feed(idrFrame)

	split := New()
	mid := len(idrFrame) / 2
	gotUnits := split.Feed(idrFrame[:mid])
	if len(gotUnits) != 0 {
		t.Fatalf("expected no complete unit from a half-fed PES packet, got %d", len(gotUnits))
	}
	gotUnits = append(gotUnits, split.Feed(idrFrame[mid:])...)

	if len(gotUnits) != len(wantUnits) {
		t.Fatalf("split feed produced %d units, whole feed produced %d", len(gotUnits), len(wantUnits))
	}
}

// TestFeedRealCapture_LeadingNoise proves the resync path (see parseOne's
// "not positioned on a start code" branch) correctly discards bytes ahead
// of the first real start code instead of misparsing them - simulates
// joining a session where the very first Feed doesn't start cleanly at a
// pack_header (e.g. StreamSysHead bytes accidentally included).
func TestFeedRealCapture_LeadingNoise(t *testing.T) {
	frames := loadFixtures(t)
	noisy := append(append([]byte(nil), frames[0]...), frames[1]...)

	withNoise := New()
	var gotUnits []Unit
	gotUnits = append(gotUnits, withNoise.Feed(noisy)...)
	for _, f := range frames[2:] {
		gotUnits = append(gotUnits, withNoise.Feed(f)...)
	}
	if u := withNoise.Flush(); u != nil {
		gotUnits = append(gotUnits, *u)
	}

	clean := New()
	var wantUnits []Unit
	for _, f := range frames[1:] {
		wantUnits = append(wantUnits, clean.Feed(f)...)
	}
	if u := clean.Flush(); u != nil {
		wantUnits = append(wantUnits, *u)
	}

	if len(gotUnits) != len(wantUnits) {
		t.Fatalf("with leading noise: got %d units, want %d", len(gotUnits), len(wantUnits))
	}
	for i := range wantUnits {
		if !bytes.Equal(gotUnits[i].Data, wantUnits[i].Data) {
			t.Errorf("unit %d differs with leading noise present", i)
		}
	}
}

// TestFlush_EmptyDemuxer ensures Flush on a Demuxer that never saw a VCL
// NAL (e.g. Stop called before any frame completed) returns nil, not a
// bogus empty/SPS-only unit.
func TestFlush_EmptyDemuxer(t *testing.T) {
	d := New()
	if u := d.Flush(); u != nil {
		t.Errorf("Flush on empty demuxer = %+v, want nil", u)
	}

	frames := loadFixtures(t)
	d2 := New()
	d2.Feed(frames[1]) // pack_header+PSM only - no VCL seen yet
	if u := d2.Flush(); u != nil {
		t.Errorf("Flush after only a non-VCL PES = %+v, want nil", u)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
