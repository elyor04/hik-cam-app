package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/elyor04/go-hikvision-sdk/hikvision"

	"hik-cam-app/internal/video/h264"
	"hik-cam-app/internal/video/psdemux"
	"hik-cam-app/internal/wire"
)

// maxCodecSniffFrames caps how many SDK frames are buffered while looking for an
// SPS NAL before giving up and assuming this project's default H.264 profile.
const maxCodecSniffFrames = 10

// sniffTimeout bounds how long to wait for the first usable video frame before
// surfacing a clear error instead of an indefinitely black view.
const sniffTimeout = 8 * time.Second

// splitFrame adapts hikvision.Frame to what wire.Pipeline needs: the payload
// bytes, and whether this is an out-of-band NET_DVR_SYSHEAD blob rather than
// part of the PS/PES byte stream. This one function is the entire SDK-shaped
// surface of the demux pipeline, which is what lets the pipeline itself live in
// internal/wire and be tested without the SDK.
func splitFrame(f hikvision.Frame) ([]byte, bool) {
	return f.Data, f.Type == hikvision.StreamSysHead
}

// StartStream begins live playback of the given 1-based channel on the connected
// device.
//
// It deliberately does not return the stream URL. The URL alone is not usable:
// VideoDecoder.configure also needs a codec string, which isn't known until an
// SPS NAL has been buffered, several hundred milliseconds later. Callers must
// wait for the "stream:ready" event, or call GetStreamInfo, which carry both.
func (a *App) StartStream(channel int) error {
	cs := &a.cam

	cs.mu.Lock()
	dev := cs.device
	if dev == nil {
		cs.mu.Unlock()
		return fmt.Errorf("not connected")
	}
	if cs.streamCancel != nil {
		cs.mu.Unlock()
		return fmt.Errorf("stream already active")
	}
	ctx, cancel := context.WithCancel(a.ctx)
	bc := wire.NewBroadcaster()
	start := time.Now()
	cs.streamCancel = cancel
	cs.broadcast = bc
	cs.streamStartedAt = start
	cs.mu.Unlock()

	log.Printf("[stream] StartStream: channel=%d", channel)
	go a.runStreamPipeline(ctx, start, dev, int32(channel), bc)
	return nil
}

// StopStream stops the active live playback. Safe to call when nothing is
// running.
func (a *App) StopStream() error {
	cs := &a.cam

	cs.mu.Lock()
	cancel, bc := cs.streamCancel, cs.broadcast
	cs.streamCancel, cs.broadcast = nil, nil
	// Cleared here too, not only in runStreamPipeline's deferred cleanup: that
	// cleanup clears it only when cs.broadcast still equals its own bc, which
	// the line above has just made false.
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
		// Disconnects every currently-connected viewer immediately, independent
		// of how quickly ctx cancellation stops the SDK callback.
		bc.CloseAll()
	}
	return nil
}

// IsStreamActive reports whether a live-view pipeline is currently running.
//
// This is deliberately broader than GetStreamInfo, which only succeeds once the
// codec sniff has finished. Between StartStream and "stream:ready" - up to
// sniffTimeout, so as much as 8 seconds - a pipeline is genuinely running but
// has nothing to report yet. A frontend that mounted in that window and asked
// only GetStreamInfo used to conclude no stream existed, offer a Start button,
// and then fail with "stream already active" when the user pressed it. Same
// reasoning as IsANPRActive and IsConnected: the Go side owns this state and
// outlives any given mount, so it has to be askable.
func (a *App) IsStreamActive() bool {
	a.cam.mu.Lock()
	defer a.cam.mu.Unlock()
	return a.cam.streamCancel != nil
}

func (a *App) runStreamPipeline(ctx context.Context, start time.Time, dev *hikvision.Device, channel int32, bc *wire.Broadcaster) {
	cs := &a.cam
	defer func() {
		cs.mu.Lock()
		if cs.broadcast == bc {
			cs.broadcast, cs.streamCancel = nil, nil
			cs.streamReady, cs.streamReadyOK = StreamReadyDTO{}, false
		}
		cs.mu.Unlock()
		bc.CloseAll()
		log.Printf("[stream] session ended after %v (dropped-frames total=%d)", elapsed(start), hikvision.DroppedFrameCount())
		a.emit(evtStreamStopped)
	}()

	// MainStream (not SubStream) - full resolution/bitrate encode.
	rp, err := dev.RealPlay(ctx, channel, hikvision.MainStream)
	if err != nil {
		// ctx.Err() != nil means an ordinary StopStream/shutdown cancelled ctx
		// while RealPlay was still starting up - not a real failure worth
		// surfacing to the user.
		if ctx.Err() == nil {
			log.Printf("[stream] RealPlay failed after %v: %v", elapsed(start), err)
			a.emit(evtStreamError, err.Error())
		}
		return
	}
	log.Printf("[stream] RealPlay started at t+%v", elapsed(start))
	// RealPlay's own ctx-linked goroutine closes rp (and rp.Frames()) when ctx is
	// cancelled, so no explicit Close is needed on the cancellation path - this
	// defer is a safety net for the early returns below.
	defer rp.Close()
	frames := rp.Frames()

	mode, buffered, ok := a.sniffStream(ctx, start, frames)
	if !ok {
		return
	}

	// Reject a codec this build cannot decode, rather than misframing it. Every
	// stage downstream of here is H.264-only (see internal/video/h264's package
	// doc): psdemux reassembles access units by H.264 VCL NAL types, and the
	// codec string handed to VideoDecoder.configure is always avc1.*. Without
	// this check an H.265 camera - which many current Hikvision models default
	// to - produced garbage units configured as H.264, surfacing minutes later
	// as "decoding is failing repeatedly", which points nowhere near the cause.
	family, codec := sniffCodec(mode, buffered)
	if family != "" && family != "h264" {
		log.Printf("[stream] unsupported codec %q detected at t+%v", family, elapsed(start))
		a.emit(evtStreamError, fmt.Sprintf(
			"camera is sending %s; this build decodes H.264 only - change the camera's video encoding to H.264",
			strings.ToUpper(family)))
		return
	}

	ready := StreamReadyDTO{
		URL:   a.streamURL(),
		Codec: codec,
	}
	cs.mu.Lock()
	cs.streamReady, cs.streamReadyOK = ready, true
	cs.mu.Unlock()
	a.emit(evtStreamReady, ready)
	log.Printf("[stream] stream:ready emitted at t+%v: codec=%s", elapsed(start), ready.Codec)

	dst := &firstByteLogger{w: wire.Writer{B: bc}, label: "wire-output->broadcast", start: start}
	pipeline := wire.Pipeline[hikvision.Frame]{
		Mode:   mode,
		Split:  splitFrame,
		OnUnit: unitLogger(start, frames),
	}
	if err := pipeline.Run(buffered, frames, dst); err != nil && ctx.Err() == nil {
		log.Printf("[stream] demux exited with error at t+%v: %v", elapsed(start), err)
		a.emit(evtStreamError, fmt.Sprintf("demux: %v", err))
	}
}

// sniffStream classifies which shape this device's data arrives in (see
// wire.Mode) before starting the demux pipeline, buffering leading frames so
// nothing is lost. Bounded by sniffTimeout so a channel that never produces
// video (wrong channel number, audio-only, ...) surfaces a clear error instead
// of an indefinitely black view. ok is false when the caller should give up.
func (a *App) sniffStream(ctx context.Context, start time.Time, frames <-chan hikvision.Frame) (mode wire.Mode, buffered []hikvision.Frame, ok bool) {
	sniffTimer := time.NewTimer(sniffTimeout)
	defer sniffTimer.Stop()

	mode = wire.ModeUnknown
	firstFrameLogged := false
sniffLoop:
	for {
		select {
		case f, more := <-frames:
			if !more {
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
				mode = wire.ModePS
			case hikvision.StreamStdVideoData:
				mode = wire.ModeES
			default:
				continue
			}

			buffered = append(buffered, f)
			// Keep buffering rather than breaking out on the first frame: the
			// very first frame off real hardware is StreamSysHead, a protocol
			// header carrying no video at all, so one frame is nowhere near
			// enough to find an SPS. See sniffCodec.
			//
			// Stopping on any recognized family, not just H.264, is what lets an
			// H.265 camera be rejected promptly instead of buffering out the
			// full window first and only then failing to classify.
			if family, _ := sniffCodec(mode, buffered); family != "" {
				break sniffLoop
			}
			if len(buffered) >= maxCodecSniffFrames {
				break sniffLoop
			}
		case <-sniffTimer.C:
			break sniffLoop
		}
	}

	if mode == wire.ModeUnknown {
		// Same ctx.Err() reasoning as the RealPlay path: a StopStream while
		// still waiting on the first frame closes `frames`, which breaks this
		// loop with mode still unknown - not a real "the camera sent nothing
		// useful" failure.
		if ctx.Err() == nil {
			log.Printf("[stream] sniff failed after %v: no usable video data", elapsed(start))
			a.emit(evtStreamError, "no video data received from camera (check the channel number)")
		}
		return wire.ModeUnknown, nil, false
	}
	log.Printf("[stream] sniff done at t+%v: mode=%v bufferedFrames=%d", elapsed(start), mode, len(buffered))
	return mode, buffered, true
}

// unitLogger builds the per-access-unit logging wire.Pipeline calls back into.
// It lives here rather than in the pipeline because the numbers worth logging
// alongside each unit - the SDK's process-wide dropped-frame count, and the
// backlog on the SDK's own delivery channel - are only visible from this side.
func unitLogger(start time.Time, frames <-chan hikvision.Frame) func(wire.UnitStat) {
	return func(s wire.UnitStat) {
		// Keyframes are ~10-20x a delta frame's size (a full intra-coded picture
		// vs. a diff) and arrive on the camera's fixed GOP interval, so logging
		// every one costs little - and their size/cadence/write time are exactly
		// what matters when diagnosing a playback hitch that turns out to be
		// keyframe-aligned.
		if s.Keyframe {
			log.Printf("[stream] keyframe unit %d: bytes=%d sinceLastKeyframe=%v writeDur=%v t+%v",
				s.Count, s.Bytes, s.SincePrevKeyframe.Round(time.Millisecond), s.WriteDur.Round(time.Millisecond), elapsed(start))
		}
		if s.Count%150 == 0 {
			log.Printf("[stream] wrote %d access units at t+%v, gapSincePrev=%v writeDur=%v, frames-channel backlog=%d/%d, dropped-frames=%d",
				s.Count, elapsed(start), s.SincePrevUnit.Round(time.Millisecond), s.WriteDur.Round(time.Millisecond),
				len(frames), cap(frames), hikvision.DroppedFrameCount())
		}
	}
}

// sniffCodec inspects the buffered sniff frames for a sequence parameter set and
// reports both which codec family it belongs to and the exact `avc1.PPCCLL`
// string VideoDecoderConfig.codec needs.
//
// ModeES frames are already bare Annex-B access units and can be scanned
// directly; ModePS frames are still raw, undemuxed PS/PES container bytes, so
// they go through psdemux first to recover real elementary-stream bytes.
// Scanning the raw container bytes directly would almost never find an SPS and
// would silently fall back to the hardcoded default profile for effectively
// every PS-mode camera.
//
// family is "" when no SPS turned up inside the sniff window, in which case
// codec is this project's known-Baseline default and the caller proceeds: a
// stream that could not be classified yet is not the same as one known to be
// undecodable.
//
// Safe to call repeatedly against the growing `buffered` slice from the sniff
// loop: psdemux.Feed is a pure function of the bytes handed to it in order, so
// re-demuxing from scratch each call redoes a little work (buffered is capped at
// maxCodecSniffFrames, and this is a one-time startup cost) but is otherwise
// harmless - the pipeline demuxes these same frames again afterwards with its
// own fresh instance and produces identical output.
func sniffCodec(mode wire.Mode, buffered []hikvision.Frame) (family, codec string) {
	// inspect classifies one elementary-stream access unit. A non-H.264 family
	// gets the default codec string it will never use - the caller rejects the
	// stream on family alone.
	inspect := func(unit []byte) (string, string, bool) {
		fam, ok := h264.DetectCodec(unit)
		if !ok {
			return "", "", false
		}
		if fam != "h264" {
			return fam, h264.CodecString(nil), true
		}
		return fam, h264.CodecString(unit), true
	}

	if mode != wire.ModePS {
		for _, f := range buffered {
			if fam, c, ok := inspect(f.Data); ok {
				return fam, c
			}
		}
		return "", h264.CodecString(nil)
	}

	dmx := psdemux.New()
	for _, f := range buffered {
		if f.Type == hikvision.StreamSysHead {
			continue // out-of-band decoder-init blob, not part of the PS/PES byte stream
		}
		for _, u := range dmx.Feed(f.Data) {
			if fam, c, ok := inspect(u.Data); ok {
				return fam, c
			}
		}
	}
	if u := dmx.Flush(); u != nil {
		if fam, c, ok := inspect(u.Data); ok {
			return fam, c
		}
	}
	return "", h264.CodecString(nil)
}
