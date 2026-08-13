package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"time"

	"github.com/elyor04/go-hikvision-sdk/hikvision"

	"hik-cam-app/internal/video/h264"
	"hik-cam-app/internal/video/psdemux"
)

// maxCodecSniffFrames caps how many SDK frames are buffered while looking
// for an SPS NAL before giving up and assuming this project's default
// H.264 profile.
const maxCodecSniffFrames = 10

// sniffTimeout bounds how long to wait for the first usable video frame
// before surfacing a clear error instead of an indefinitely black view.
const sniffTimeout = 8 * time.Second

// streamMode identifies which of the two shapes HCNetSDK's RealPlay data can
// arrive in. The SDK wrapper's RealPlay hardcodes byProtoType=0 (private
// protocol), and which of these a given device actually sends back is not
// something this app controls.
type streamMode int

const (
	modeUnknown streamMode = iota
	// modePS: NET_DVR_SYSHEAD (1) + NET_DVR_STREAMDATA (2) [+
	// AUDIOSTREAMDATA (3)] together form a raw MPEG Program Stream -
	// Hikvision's private-protocol container, NOT a bare H.264/H.265
	// elementary stream. Demuxed natively by internal/video/psdemux. This is
	// what real hardware has actually been observed to send.
	modePS
	// modeES: NET_DVR_STD_VIDEODATA (4) is a genuine bare elementary stream
	// (no container). Kept as a defensive fallback - each SDK frame is
	// assumed to already be one complete Annex-B access unit.
	modeES
)

// wireHeaderSize is the fixed header this app's own wire format prepends to
// every access unit written to the HTTP stream: 1 flags byte (bit 0 =
// keyframe) + 4-byte big-endian payload length. Deliberately minimal - no
// container format, no ffmpeg - the frontend (see
// frontend/src/components/LiveView.tsx) reads this directly and hands the
// payload straight to WebCodecs' EncodedVideoChunk.
const wireHeaderSize = 5
const wireFlagKeyframe = 0x01

// StartStream begins live playback of the given 1-based channel on the
// connected device and returns the local HTTP URL to fetch the raw Annex-B
// access-unit stream from. The exact codec string VideoDecoder.configure
// needs isn't known yet at this point (it requires a buffered SPS NAL), so
// the frontend must wait for the "stream:ready" event - or call
// GetStreamInfo - before using this URL.
func (a *App) StartStream(channel int) (string, error) {
	cs := &a.cam

	cs.mu.Lock()
	dev := cs.device
	if dev == nil {
		cs.mu.Unlock()
		return "", fmt.Errorf("not connected")
	}
	if cs.streamCancel != nil {
		cs.mu.Unlock()
		return "", fmt.Errorf("stream already active")
	}
	ctx, cancel := context.WithCancel(a.ctx)
	bc := newBroadcaster()
	start := time.Now()
	cs.streamCancel = cancel
	cs.broadcast = bc
	cs.streamStartedAt = start
	addr := a.httpAddr
	cs.mu.Unlock()

	log.Printf("[stream] StartStream: channel=%d", channel)
	go a.runStreamPipeline(ctx, start, dev, int32(channel), bc)
	return fmt.Sprintf("http://%s/stream", addr), nil
}

// StopStream stops the active live playback. Safe to call when nothing is
// running.
func (a *App) StopStream() error {
	cs := &a.cam

	cs.mu.Lock()
	cancel, bc := cs.streamCancel, cs.broadcast
	cs.streamCancel, cs.broadcast = nil, nil
	// Cleared here too, not only in runStreamPipeline's deferred cleanup:
	// that cleanup clears it only when cs.broadcast still equals its own bc,
	// which the line above has just made false.
	cs.streamReady, cs.streamReadyOK = StreamReadyDTO{}, false
	cs.mu.Unlock()

	if cancel == nil && bc == nil {
		return nil
	}
	log.Printf("[stream] StopStream")
	if cancel != nil {
		cancel()
	}
	if bc != nil {
		// Disconnects every currently-connected viewer immediately,
		// independent of how quickly ctx cancellation stops the SDK
		// callback.
		bc.closeAll()
	}
	return nil
}

func (a *App) runStreamPipeline(ctx context.Context, start time.Time, dev *hikvision.Device, channel int32, bc *broadcaster) {
	cs := &a.cam
	defer func() {
		cs.mu.Lock()
		if cs.broadcast == bc {
			cs.broadcast, cs.streamCancel = nil, nil
			cs.streamReady, cs.streamReadyOK = StreamReadyDTO{}, false
		}
		cs.mu.Unlock()
		bc.closeAll()
		log.Printf("[stream] session ended after %v (dropped-frames total=%d)", elapsed(start), hikvision.DroppedFrameCount())
		a.emit(evtStreamStopped)
	}()

	// MainStream (not SubStream) - full resolution/bitrate encode.
	rp, err := dev.RealPlay(ctx, channel, hikvision.MainStream)
	if err != nil {
		// ctx.Err() != nil means an ordinary StopStream/shutdown cancelled
		// ctx while RealPlay was still starting up - not a real failure
		// worth surfacing to the user.
		if ctx.Err() == nil {
			log.Printf("[stream] RealPlay failed after %v: %v", elapsed(start), err)
			a.emit(evtStreamError, err.Error())
		}
		return
	}
	log.Printf("[stream] RealPlay started at t+%v", elapsed(start))
	// RealPlay's own ctx-linked goroutine closes rp (and rp.Frames()) when
	// ctx is cancelled, so no explicit Close is needed on the cancellation
	// path - this defer is a safety net for the early returns below.
	defer rp.Close()
	frames := rp.Frames()

	// Classify which shape this device's data arrives in (see streamMode)
	// before starting the demux pipeline, buffering leading frames so
	// nothing is lost. Bounded by sniffTimeout so a channel that never
	// produces video (wrong channel number, audio-only, ...) surfaces a
	// clear error instead of an indefinitely black view.
	sniffTimer := time.NewTimer(sniffTimeout)
	defer sniffTimer.Stop()

	mode := modeUnknown
	var buffered []hikvision.Frame
	firstFrameLogged := false
sniffLoop:
	for {
		select {
		case f, ok := <-frames:
			if !ok {
				break sniffLoop
			}
			if !firstFrameLogged {
				firstFrameLogged = true
				log.Printf("[stream] first frame from SDK at t+%v (type=%d bytes=%d)", elapsed(start), f.Type, len(f.Data))
			}
			if len(f.Data) == 0 {
				continue
			}
			switch f.Type {
			case hikvision.StreamSysHead, hikvision.StreamData, hikvision.StreamAudioData:
				mode = modePS
				buffered = append(buffered, f)
				// Keep buffering rather than breaking out on the first
				// frame: the very first frame off real hardware is
				// StreamSysHead, a protocol header carrying no video at all,
				// so one frame is nowhere near enough to find an SPS. See
				// sniffPSCodec.
				if _, ok := sniffPSCodec(buffered); ok {
					break sniffLoop
				}
				if len(buffered) >= maxCodecSniffFrames {
					break sniffLoop
				}
			case hikvision.StreamStdVideoData:
				mode = modeES
				buffered = append(buffered, f)
				if _, ok := h264.DetectCodec(f.Data); ok {
					break sniffLoop
				}
				if len(buffered) >= maxCodecSniffFrames {
					break sniffLoop
				}
			default:
				continue
			}
		case <-sniffTimer.C:
			break sniffLoop
		}
	}
	if mode == modeUnknown {
		// Same ctx.Err() reasoning as the RealPlay path above: a StopStream
		// while still waiting on the first frame closes `frames`, which
		// breaks this loop with mode still modeUnknown - not a real "the
		// camera sent nothing useful" failure.
		if ctx.Err() == nil {
			log.Printf("[stream] sniff failed after %v: no usable video data", elapsed(start))
			a.emit(evtStreamError, "no video data received from camera (check the channel number)")
		}
		return
	}
	log.Printf("[stream] sniff done at t+%v: mode=%v bufferedFrames=%d", elapsed(start), mode, len(buffered))

	ready := StreamReadyDTO{
		URL:   fmt.Sprintf("http://%s/stream", a.httpAddr),
		Codec: sniffCodecString(mode, buffered),
	}
	cs.mu.Lock()
	cs.streamReady, cs.streamReadyOK = ready, true
	cs.mu.Unlock()
	a.emit(evtStreamReady, ready)
	log.Printf("[stream] stream:ready emitted at t+%v: codec=%s", elapsed(start), ready.Codec)

	dst := &firstByteLogger{w: broadcastWriter{bc}, label: "wire-output->broadcast", start: start}
	if err := runDemuxPipeline(start, mode, buffered, frames, dst); err != nil && ctx.Err() == nil {
		log.Printf("[stream] demux exited with error at t+%v: %v", elapsed(start), err)
		a.emit(evtStreamError, fmt.Sprintf("demux: %v", err))
	}
}

// sniffPSCodec demuxes buffered's raw PS/PES frames (skipping the
// out-of-band StreamSysHead blob, the same convention runDemuxPipeline uses)
// through a throwaway psdemux instance and looks for an H.264 SPS in the
// resulting elementary-stream units.
//
// Safe to call repeatedly against the growing `buffered` slice from the
// sniff loop: psdemux.Feed is a pure function of the bytes handed to it in
// order, so re-demuxing from scratch each call redoes a little work
// (buffered is capped at maxCodecSniffFrames, and this is a one-time startup
// cost) but is otherwise harmless - runDemuxPipeline demuxes these same
// frames again afterwards with its own fresh instance and produces identical
// output.
func sniffPSCodec(buffered []hikvision.Frame) (string, bool) {
	dmx := psdemux.New()
	for _, f := range buffered {
		if f.Type == hikvision.StreamSysHead {
			continue
		}
		for _, u := range dmx.Feed(f.Data) {
			if _, _, _, ok := h264.ProfileLevel(u.Data); ok {
				return h264.CodecString(u.Data), true
			}
		}
	}
	if u := dmx.Flush(); u != nil {
		if _, _, _, ok := h264.ProfileLevel(u.Data); ok {
			return h264.CodecString(u.Data), true
		}
	}
	return "", false
}

// sniffCodecString derives the `avc1.PPCCLL` string VideoDecoderConfig.codec
// needs from the buffered sniff frames. modeES frames are already bare
// Annex-B access units and can be scanned directly; modePS frames are still
// raw, undemuxed PS/PES container bytes, so they go through sniffPSCodec
// first to recover real elementary-stream bytes. Scanning the raw container
// bytes directly would almost never find an SPS and would silently fall back
// to the hardcoded default profile for effectively every PS-mode camera.
func sniffCodecString(mode streamMode, buffered []hikvision.Frame) string {
	if mode == modePS {
		if codec, ok := sniffPSCodec(buffered); ok {
			return codec
		}
		return h264.CodecString(nil)
	}
	for _, f := range buffered {
		if _, _, _, ok := h264.ProfileLevel(f.Data); ok {
			return h264.CodecString(f.Data)
		}
	}
	return h264.CodecString(nil)
}

// streamStaleTimeout bounds how long runDemuxPipeline waits for the *next*
// frame once streaming is already under way before concluding the connection
// is dead (e.g. the network cable was pulled). This watchdog is not
// redundant with HCNetSDK's own exception callback (see app.go's
// handleSDKException): against real hardware, EXCEPTION_PREVIEW has been
// observed not to fire at all for minutes after an actual cable pull - the
// SDK's frame callback simply stops being invoked, with no error and no
// channel close. A camera at ~25fps sends a frame every ~40ms, so 5s is
// generous headroom while still being far faster than HCNetSDK's own 30s
// reconnect interval.
const streamStaleTimeout = 5 * time.Second

var errStreamStale = errors.New("no frame received before the stale-stream watchdog fired (camera unreachable?)")

// readFrame waits for the next frame or reports errStreamStale if none
// arrives within timeout. ok mirrors a channel receive's second value: false
// with err == nil means `frames` was closed, i.e. the stream ended normally
// (Close/ctx cancellation), not a stall.
func readFrame(frames <-chan hikvision.Frame, timeout time.Duration) (f hikvision.Frame, ok bool, err error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case f, ok = <-frames:
		return f, ok, nil
	case <-timer.C:
		return hikvision.Frame{}, false, errStreamStale
	}
}

// runDemuxPipeline turns the camera's raw byte stream into this app's wire
// format (see wireHeaderSize) written to dst: for modePS, frames are fed
// through internal/video/psdemux to reassemble access units out of
// HCNetSDK's private-protocol PS/PES container; for modeES, each SDK frame
// is already a bare elementary-stream access unit and is written directly.
// No decoding and no container remux happen here or anywhere in this
// process - decoding happens in the browser via WebCodecs.
func runDemuxPipeline(start time.Time, mode streamMode, buffered []hikvision.Frame, frames <-chan hikvision.Frame, dst io.Writer) error {
	var unitCount int
	var lastUnitAt, lastKeyframeAt time.Time
	logProgress := func(dataLen int, keyframe bool, writeDur time.Duration) {
		unitCount++
		gap := time.Duration(0)
		if !lastUnitAt.IsZero() {
			gap = time.Since(lastUnitAt)
		}
		lastUnitAt = time.Now()

		// Keyframes are ~10-20x a delta frame's size (a full intra-coded
		// picture vs. a diff) and arrive on the camera's fixed GOP interval,
		// so logging every one costs little - and their size/cadence/write
		// time are exactly what matters when diagnosing a playback hitch
		// that turns out to be keyframe-aligned.
		if keyframe {
			sinceLastKeyframe := time.Duration(0)
			if !lastKeyframeAt.IsZero() {
				sinceLastKeyframe = time.Since(lastKeyframeAt)
			}
			lastKeyframeAt = time.Now()
			log.Printf("[stream] keyframe unit %d: bytes=%d sinceLastKeyframe=%v writeDur=%v t+%v",
				unitCount, dataLen, sinceLastKeyframe.Round(time.Millisecond), writeDur.Round(time.Millisecond), elapsed(start))
		}
		if unitCount%150 == 0 {
			log.Printf("[stream] wrote %d access units at t+%v, gapSincePrev=%v writeDur=%v, frames-channel backlog=%d/%d, dropped-frames=%d",
				unitCount, elapsed(start), gap.Round(time.Millisecond), writeDur.Round(time.Millisecond),
				len(frames), cap(frames), hikvision.DroppedFrameCount())
		}
	}

	writeUnit := func(data []byte, keyframe bool) error {
		if len(data) == 0 {
			return nil
		}
		msg := make([]byte, wireHeaderSize+len(data))
		if keyframe {
			msg[0] = wireFlagKeyframe
		}
		binary.BigEndian.PutUint32(msg[1:5], uint32(len(data)))
		copy(msg[wireHeaderSize:], data)
		writeStart := time.Now()
		_, err := dst.Write(msg)
		writeDur := time.Since(writeStart)
		logProgress(len(data), keyframe, writeDur)
		return err
	}

	if mode == modeES {
		for _, f := range buffered {
			if err := writeUnit(f.Data, h264.IsKeyframe(f.Data)); err != nil {
				return err
			}
		}
		for {
			f, ok, err := readFrame(frames, streamStaleTimeout)
			if err != nil {
				return err
			}
			if !ok {
				return nil
			}
			if len(f.Data) == 0 {
				continue
			}
			if err := writeUnit(f.Data, h264.IsKeyframe(f.Data)); err != nil {
				return err
			}
		}
	}

	dmx := psdemux.New()
	feed := func(data []byte) error {
		for _, u := range dmx.Feed(data) {
			if err := writeUnit(u.Data, u.Keyframe); err != nil {
				return err
			}
		}
		return nil
	}

	for _, f := range buffered {
		if f.Type == hikvision.StreamSysHead {
			continue // out-of-band decoder-init blob, not part of the PS/PES byte stream - see internal/video/psdemux's package doc
		}
		if err := feed(f.Data); err != nil {
			return err
		}
	}
	for {
		f, ok, err := readFrame(frames, streamStaleTimeout)
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		if len(f.Data) == 0 || f.Type == hikvision.StreamSysHead {
			continue
		}
		if err := feed(f.Data); err != nil {
			return err
		}
	}
	if u := dmx.Flush(); u != nil {
		return writeUnit(u.Data, u.Keyframe)
	}
	return nil
}
