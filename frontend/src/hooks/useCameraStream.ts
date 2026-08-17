import {RefObject, useCallback, useEffect, useRef, useState} from 'react';
import {GetStreamInfo, IsStreamActive, LogFrontend, StartStream, StopStream} from '../../wailsjs/go/main/App';
import {EventsOn} from '../../wailsjs/runtime';
import {EVT, StreamReadyDTO} from '../types';

// Wire format written by internal/wire's Encode: repeated
// [1 byte flags (bit0 = keyframe)][4-byte big-endian payload length][payload]
// messages, each payload one complete Annex-B H.264 access unit - no container,
// no ffmpeg, no MSE involved. Decoding happens here, once, via WebCodecs
// directly. This parser and the Go encoder must change together; neither side
// can validate the other.
const WIRE_HEADER_SIZE = 5;
const WIRE_FLAG_KEYFRAME = 0x01;

const STATS_INTERVAL_MS = 1000;

// Consecutive decoder.decode() throws with no successful output in between
// before the failure is surfaced to the user. A single throw is expected and
// self-healing (a delta chunk fed before the decoder has seen a keyframe, which
// the stream's next keyframe fixes), but a sustained run means something is
// genuinely wrong and would otherwise be invisible outside a dev console.
const DECODE_FAILURE_THRESHOLD = 10;

// Diagnostic log lines allowed per second before the rest of that second is
// coalesced into a single suppressed-count line on the next stats tick.
//
// Every logT call is a Wails IPC round-trip, and the two most frequent callers
// fire on render gaps and network chunk gaps - conditions that, during a real
// stall, repeat continuously. Unbounded, the diagnostic adds main-thread work to
// an already-stalled main thread, making the stall it is reporting on worse.
const LOG_LINES_PER_SEC = 10;

/**
 * Strips the stream URL's random path token out of a diagnostic line.
 *
 * That token is not decoration: the loopback listener is reachable by any page
 * in any browser on this machine, and the CORS header has to stay "*" because
 * the frontend's origin differs between `wails dev` and a packaged build, so an
 * unguessable path is the only thing actually gating access to the camera feed
 * (see httpstream.go's newStreamPathToken). Every logT line is forwarded to Go's
 * stdout via LogFrontend, and an app log is precisely what gets pasted into a
 * bug report or shown on a screen share - so logging the token there hands the
 * live feed to anyone who reads it.
 *
 * Applied centrally, on the composed line, rather than at each call site: the
 * URL reaches the log inside a whole StreamReadyDTO at two of the three current
 * sites, and a redaction that has to be remembered per call is one that will be
 * forgotten by the next one. The host:port is deliberately kept - it is the
 * useful half for diagnosing a failed fetch.
 */
function redactStreamToken(line: string): string {
    return line.replace(/(\/stream\/)[0-9a-f]{8,}/gi, '$1<redacted>');
}

/**
 * Lifecycle of the live view, as an explicit state rather than a set of
 * independent booleans.
 *
 * This used to be a bare `active` flag written from six different places
 * (start, stop, and the ready/error/stopped/catch-up paths), with readiness
 * derived separately as `active && width > 0`. There was a real state machine
 * underneath, but because it was never named nothing stopped two writers from
 * disagreeing - see the mount-during-sniff case IsStreamActive now covers.
 */
export type StreamPhase = 'idle' | 'starting' | 'live' | 'error';

export interface StreamStats {
    fps: number;
    width: number;
    height: number;
}

const NO_STATS: StreamStats = {fps: 0, width: 0, height: 0};

export interface CameraStream {
    phase: StreamPhase;
    /** Non-null only while phase is 'error'. */
    error: string | null;
    stats: StreamStats;
    /** True once frames are actually reaching the canvas. */
    playing: boolean;
    start: (channel: number) => Promise<void>;
    stop: () => Promise<void>;
    /** True while a start/stop call is in flight. */
    busy: boolean;
}

/**
 * Owns everything between the backend's wire stream and a <canvas>: the
 * WebCodecs decoder lifecycle, the wire-format parser, the fetch reader loop,
 * frame pacing, telemetry, and the stream's lifecycle state.
 *
 * Extracted from LiveView.tsx, which had grown to 485 lines carrying all of the
 * above plus the UI. None of the parsing or decoding logic was reachable from a
 * test while it lived inside a component.
 */
export function useCameraStream(canvasRef: RefObject<HTMLCanvasElement | null>): CameraStream {
    const [phase, setPhase] = useState<StreamPhase>('idle');
    const [error, setError] = useState<string | null>(null);
    const [stats, setStats] = useState<StreamStats>(NO_STATS);
    const [busy, setBusy] = useState(false);

    const abortRef = useRef<AbortController | null>(null);
    const decoderRef = useRef<VideoDecoder | null>(null);
    const rafRef = useRef<number | null>(null);
    const statsIntervalRef = useRef<number | null>(null);
    const latestFrameRef = useRef<VideoFrame | null>(null);

    // t0 for every timing log in a session. Set when the user starts a stream,
    // and also when this hook adopts an already-running one on mount - see
    // adoptTimeline. 0 means "no session", which logT renders as t+?s rather
    // than silently reporting time since page load.
    const streamStartRef = useRef<number>(0);

    // Bumped by every startDecoding call, whichever trigger fired it. Lets the
    // catch-up effect notice it was superseded by a stream:ready event that
    // arrived while its request was still in flight, and skip acting on its
    // now-stale response.
    const generationRef = useRef(0);

    // Sliding one-second window for logT's rate limit. Deliberately driven by the
    // clock rather than refilled by the stats interval: that interval only exists
    // while a decode session is running, so a stall that burned the budget and
    // then ended would leave it at zero with nothing left to reset it - silently
    // muting every later line, including the stream:error explaining what
    // happened. That is the same "diagnostics fail exactly when needed" failure
    // this limiter exists to prevent, one level up.
    const logWindowRef = useRef({startedAt: 0, used: 0, suppressed: 0});

    const logT = useCallback((label: string, extra?: unknown) => {
        const now = performance.now();
        const w = logWindowRef.current;
        if (now - w.startedAt >= 1000) {
            w.startedAt = now;
            w.used = 0;
        }
        if (w.used >= LOG_LINES_PER_SEC) {
            w.suppressed++;
            return;
        }
        w.used++;
        // Surfaced on the next line that does get through, so a burst is visible
        // as a count rather than vanishing.
        const suppressed = w.suppressed;
        w.suppressed = 0;

        const t = streamStartRef.current === 0 ? '?' : ((now - streamStartRef.current) / 1000).toFixed(2);
        const note = suppressed > 0 ? ` (+${suppressed} suppressed)` : '';
        const line = redactStreamToken(
            `[t+${t}s] ${label}${note} ${extra !== undefined ? JSON.stringify(extra) : ''}`,
        );
        console.log(`[camera]${line}`);
        // A production build has no attached devtools console - forward to Go's
        // stdout (see app.go's LogFrontend) so these are visible at all when
        // reproducing an issue outside `wails dev`. Fire-and-forget: never let a
        // logging call itself affect playback.
        LogFrontend(line).catch(() => {});
    }, []);

    const cleanupPlayback = useCallback(() => {
        abortRef.current?.abort();
        abortRef.current = null;
        if (rafRef.current !== null) {
            cancelAnimationFrame(rafRef.current);
            rafRef.current = null;
        }
        if (statsIntervalRef.current !== null) {
            window.clearInterval(statsIntervalRef.current);
            statsIntervalRef.current = null;
        }
        if (latestFrameRef.current) {
            latestFrameRef.current.close();
            latestFrameRef.current = null;
        }
        const decoder = decoderRef.current;
        decoderRef.current = null;
        if (decoder && decoder.state !== 'closed') {
            try {
                decoder.close();
            } catch {
                // already closing/closed - fine
            }
        }
        const canvas = canvasRef.current;
        if (canvas) {
            const ctx = canvas.getContext('2d');
            ctx?.clearRect(0, 0, canvas.width, canvas.height);
        }
        setStats(NO_STATS);
    }, [canvasRef]);

    /**
     * Reports a failure on *this* side of the stream - the decoder rejected the
     * codec, the fetch died, the webview has no WebCodecs - and tears the
     * backend pipeline down to match.
     *
     * The StopStream call is the point. A local decode failure leaves the Go
     * pipeline perfectly healthy, still pulling frames off the camera and
     * publishing them to a viewer that cannot decode any of them. Without this
     * the two sides disagree: the panel shows an error with a Start button, and
     * pressing it fails with "stream already active" - the same contradiction
     * IsStreamActive was added to remove, just arrived at from the other
     * direction. Errors reported *by* the backend do not come through here: it
     * is already tearing itself down and will emit "stream:stopped" on its own.
     */
    const failStream = useCallback(
        (message: string) => {
            setError(message);
            setPhase('error');
            cleanupPlayback();
            // Best-effort: the phase is already 'error', and the resulting
            // "stream:stopped" deliberately leaves it that way so the reason
            // stays on screen.
            StopStream().catch(() => {});
        },
        [cleanupPlayback],
    );

    /**
     * Anchors the timing timeline when this hook joins a stream it did not
     * start. The Go side's own t+ values are relative to its StartStream call,
     * which this mount has no way to know, so rather than silently reporting
     * time since page load the two timelines are declared separately and the
     * discrepancy is logged once.
     */
    const adoptTimeline = useCallback(() => {
        if (streamStartRef.current !== 0) return;
        streamStartRef.current = performance.now();
        logT('adopted an already-running stream; t+ below is relative to this mount, not to StartStream');
    }, [logT]);

    const startDecoding = useCallback(
        (url: string, codec: string) => {
            generationRef.current++;
            // Tear down any decoder/rAF loop/fetch reader still live from a
            // previous session before wiring up a new one. Without this they'd
            // leak, never closed, competing with the new one to draw onto the
            // same canvas.
            cleanupPlayback();

            const canvas = canvasRef.current;
            if (!canvas) return;
            const ctx = canvas.getContext('2d');
            if (!ctx) {
                failStream('This build cannot draw video: the canvas 2D context is unavailable.');
                return;
            }
            if (!('VideoDecoder' in window)) {
                failStream('This build cannot play video: the webview has no WebCodecs support.');
                return;
            }

            let framesDecoded = 0;
            let framesRendered = 0;
            let firstFrameLogged = false;
            let firstRenderLogged = false;
            let consecutiveDecodeErrors = 0;

            // decodeCallTimes pairs each decode() call with its eventual
            // output() to track real per-frame decode latency. Baseline has no
            // B-frames, so decode order == output order - a plain FIFO queue is
            // a valid pairing, not a heuristic. Surfaced via the periodic stats
            // log rather than per-frame (at ~25fps that would be a line every
            // 40ms).
            const decodeCallTimes: {t: number}[] = [];
            let maxDecodeLatencyMs = 0;
            let lastRenderAt = 0;

            const decoder = new VideoDecoder({
                output: frame => {
                    consecutiveDecodeErrors = 0;
                    if (!firstFrameLogged) {
                        firstFrameLogged = true;
                        logT('first decoded VideoFrame', {width: frame.displayWidth, height: frame.displayHeight});
                    }
                    framesDecoded++;
                    const call = decodeCallTimes.shift();
                    if (call) {
                        maxDecodeLatencyMs = Math.max(maxDecodeLatencyMs, performance.now() - call.t);
                    }
                    // Always show the newest frame: if the render loop hasn't
                    // drawn the previous one yet, drop it rather than queueing.
                    // This is what keeps live latency from ever accumulating, in
                    // place of MSE's playbackRate-based catch-up.
                    latestFrameRef.current?.close();
                    latestFrameRef.current = frame;
                },
                error: err => {
                    logT('VideoDecoder error', {err: String(err)});
                    failStream(`The video decoder failed: ${err.message}`);
                },
            });
            try {
                decoder.configure({codec});
            } catch (err) {
                // The codec string is the single most useful detail here (it is
                // what the sniff derived), and err carries the browser's own
                // reason - keep both rather than a friendlier message that says
                // neither.
                failStream(`This build cannot decode the camera's video format (${codec}): ${err}`);
                return;
            }
            decoderRef.current = decoder;

            const renderLoop = () => {
                rafRef.current = requestAnimationFrame(renderLoop);
                const frame = latestFrameRef.current;
                if (!frame) return;
                latestFrameRef.current = null;
                const now = performance.now();
                // A gap much bigger than one frame interval (~40ms at 25fps)
                // between actual draws means the canvas visibly held the
                // previous frame that long - this is what "freezing" looks like
                // from the user's side, wherever it originates.
                if (lastRenderAt !== 0 && now - lastRenderAt > 100) {
                    logT('render gap', {gapMs: (now - lastRenderAt).toFixed(1)});
                }
                lastRenderAt = now;
                if (!firstRenderLogged) {
                    firstRenderLogged = true;
                    logT('first frame drawn to canvas');
                }
                if (canvas.width !== frame.displayWidth || canvas.height !== frame.displayHeight) {
                    canvas.width = frame.displayWidth;
                    canvas.height = frame.displayHeight;
                    setStats(s => ({...s, width: canvas.width, height: canvas.height}));
                }
                ctx.drawImage(frame, 0, 0, canvas.width, canvas.height);
                framesRendered++;
                frame.close();
            };
            rafRef.current = requestAnimationFrame(renderLoop);

            let lastLoggedDecoded = 0;
            let lastLoggedRendered = 0;
            statsIntervalRef.current = window.setInterval(() => {
                const fps = framesRendered - lastLoggedRendered;
                logT('stats', {
                    framesDecoded,
                    framesRendered,
                    decodedSinceLastTick: framesDecoded - lastLoggedDecoded,
                    renderedSinceLastTick: fps,
                    decodeQueueSize: decoder.decodeQueueSize,
                    maxDecodeLatencyMs: maxDecodeLatencyMs.toFixed(1),
                });
                lastLoggedDecoded = framesDecoded;
                lastLoggedRendered = framesRendered;
                maxDecodeLatencyMs = 0;
                setStats(s => ({...s, fps}));
            }, STATS_INTERVAL_MS);

            const abortController = new AbortController();
            abortRef.current = abortController;

            // Growable byte buffer network reads accumulate into until complete
            // wire messages can be extracted. Capacity doubles rather than being
            // reallocated to the exact new size on every chunk: keyframes (~19x
            // a P-frame's size, per the Go side's own logging) arrive across many
            // small network reads, and naively reallocating + copying the
            // *entire* accumulated buffer on every one is O(n^2) in the access
            // unit's size - for a ~97KB keyframe split across ~15 reads that's
            // ~700KB of copying for one frame, concentrated right at every
            // keyframe boundary, a very plausible source of a periodic
            // main-thread hitch. Doubling makes total copy work amortized O(n)
            // regardless of how finely the network chunks it.
            let bufCap = new Uint8Array(256 * 1024); // comfortably above the largest observed access unit
            let bufLen = 0;
            let chunkCount = 0;
            let unitCount = 0;
            let lastEncodedTimestamp = 0;

            function appendBytes(chunk: Uint8Array) {
                const needed = bufLen + chunk.length;
                if (needed > bufCap.length) {
                    let newCap = bufCap.length * 2;
                    while (newCap < needed) newCap *= 2;
                    const grown = new Uint8Array(newCap);
                    grown.set(bufCap.subarray(0, bufLen));
                    bufCap = grown;
                }
                bufCap.set(chunk, bufLen);
                bufLen += chunk.length;
            }

            function extractMessages(): {keyframe: boolean; payload: Uint8Array}[] {
                const out: {keyframe: boolean; payload: Uint8Array}[] = [];
                let offset = 0;
                while (bufLen - offset >= WIRE_HEADER_SIZE) {
                    const flags = bufCap[offset];
                    const len =
                        ((bufCap[offset + 1] << 24) |
                            (bufCap[offset + 2] << 16) |
                            (bufCap[offset + 3] << 8) |
                            bufCap[offset + 4]) >>>
                        0;
                    if (bufLen - offset - WIRE_HEADER_SIZE < len) break;
                    const start = offset + WIRE_HEADER_SIZE;
                    out.push({
                        keyframe: (flags & WIRE_FLAG_KEYFRAME) !== 0,
                        payload: bufCap.slice(start, start + len), // owned copy - safe to hand to EncodedVideoChunk past the next appendBytes
                    });
                    offset = start + len;
                }
                if (offset > 0) {
                    // Shift only the small unconsumed remainder (typically 0
                    // bytes, occasionally a partial next header) to the front -
                    // O(remainder), not O(what was just consumed).
                    bufCap.copyWithin(0, offset, bufLen);
                    bufLen -= offset;
                }
                return out;
            }

            (async () => {
                try {
                    logT('fetch starting', {url});
                    const response = await fetch(url, {signal: abortController.signal});
                    if (!response.ok || !response.body) {
                        throw new Error(`stream fetch failed: ${response.status}`);
                    }
                    const reader = response.body.getReader();
                    let lastChunkAt = 0;
                    for (;;) {
                        const readStart = performance.now();
                        const {done, value} = await reader.read();
                        if (done) break;
                        chunkCount++;
                        if (chunkCount === 1) {
                            logT('first network chunk received', {bytes: value.byteLength});
                        }
                        const now = performance.now();
                        // A slow reader.read() itself would mean the network/pipe
                        // stalled (matching a slow Go-side write - see the
                        // writeDur log); a fast read() but a big gap since the
                        // *previous* chunk finished processing would instead
                        // point at this loop's own body (message extraction +
                        // decode calls) being slow.
                        if (lastChunkAt !== 0 && now - lastChunkAt > 100) {
                            logT('network chunk gap', {
                                gapMs: (now - lastChunkAt).toFixed(1),
                                readCallMs: (now - readStart).toFixed(1),
                                bytes: value.byteLength,
                            });
                        }
                        lastChunkAt = now;

                        appendBytes(value);

                        for (const {keyframe, payload} of extractMessages()) {
                            unitCount++;
                            // Baseline profile (this camera's codec) has no
                            // B-frames, so decode order == display order == a
                            // simple monotonically-increasing timestamp is
                            // correct - no reordering to account for.
                            let ts = Math.round(performance.now() * 1000);
                            if (ts <= lastEncodedTimestamp) ts = lastEncodedTimestamp + 1;
                            lastEncodedTimestamp = ts;
                            if (unitCount === 1) {
                                logT('first access unit decoded', {bytes: payload.byteLength, keyframe});
                            }
                            try {
                                const decodeStartedAt = performance.now();
                                decoder.decode(
                                    new EncodedVideoChunk({
                                        type: keyframe ? 'key' : 'delta',
                                        timestamp: ts,
                                        data: payload as BufferSource,
                                    }),
                                );
                                // Pushed only once decode() has returned without
                                // throwing. A throw produces no matching
                                // output(), so pushing *before* the call left an
                                // orphan entry that every later output() then
                                // shift()ed in place of its own - permanently
                                // pairing each frame with an earlier call's
                                // timestamp and over-reporting maxDecodeLatencyMs
                                // by an offset that grew with each throw and
                                // never recovered. Silent, and in exactly the
                                // measurement main.go's WebviewGpuIsDisabled
                                // decision depends on.
                                decodeCallTimes.push({t: decodeStartedAt});
                            } catch (err) {
                                consecutiveDecodeErrors++;
                                logT('decoder.decode threw', {err: String(err), consecutiveDecodeErrors});
                                if (consecutiveDecodeErrors >= DECODE_FAILURE_THRESHOLD) {
                                    failStream(
                                        'The video stream cannot be decoded by this build. Check that the camera is set to H.264.',
                                    );
                                }
                            }
                        }
                    }
                    logT('fetch reader done', {totalChunks: chunkCount, totalUnits: unitCount});
                } catch (err) {
                    if ((err as DOMException).name !== 'AbortError') {
                        failStream(`Lost the connection to the video stream: ${err}`);
                    }
                }
            })();
        },
        [canvasRef, cleanupPlayback, failStream, logT],
    );

    // Backend events are the authority on the stream's lifecycle: this hook only
    // ever moves to 'live' because the pipeline said it was ready.
    useEffect(() => {
        const offReady = EventsOn(EVT.streamReady, (evt: StreamReadyDTO) => {
            logT('stream:ready event received', evt);
            setError(null);
            setPhase('live');
            startDecoding(evt.url, evt.codec);
        });
        const offError = EventsOn(EVT.streamError, (msg: string) => {
            logT('stream:error event received', {msg});
            setError(msg);
            setPhase('error');
            cleanupPlayback();
        });
        const offStopped = EventsOn(EVT.streamStopped, () => {
            logT('stream:stopped event received');
            setPhase(p => (p === 'error' ? p : 'idle'));
            streamStartRef.current = 0;
            cleanupPlayback();
        });
        return () => {
            offReady();
            offError();
            offStopped();
            cleanupPlayback();
        };
    }, [cleanupPlayback, logT, startDecoding]);

    // Catches up on a stream this hook's own mount missed. "stream:ready" is a
    // one-time push, so a remount (React strict-mode double mount, or this panel
    // being torn down and rebuilt) while a pipeline is still running would
    // otherwise leave the canvas permanently blank even though the camera is
    // streaming fine.
    //
    // IsStreamActive is asked first and separately from GetStreamInfo, because
    // they answer different questions: a pipeline can be running for up to
    // sniffTimeout (8s) before it has a codec to report. Asking only
    // GetStreamInfo - as this used to - made that entire window look like "no
    // stream", so the UI offered a Start button that was guaranteed to fail with
    // "stream already active".
    useEffect(() => {
        let cancelled = false;
        const myGeneration = generationRef.current;

        (async () => {
            try {
                const active = await IsStreamActive();
                if (cancelled || !active) return;
                adoptTimeline();
                setPhase('starting');

                const info = await GetStreamInfo();
                // A stream:ready event can arrive and call startDecoding while
                // these requests are in flight - generationRef changing means
                // exactly that, and acting on a stale response would tear down
                // the correct, already-decoding session and reconfigure it with
                // a possibly-outdated codec.
                if (cancelled || generationRef.current !== myGeneration) return;
                logT('GetStreamInfo caught up with a live stream', info);
                setPhase('live');
                startDecoding(info.url, info.codec);
            } catch {
                // Sniff still in progress (GetStreamInfo rejects until it
                // finishes) - stay in 'starting' and let stream:ready arrive.
            }
        })();

        return () => {
            cancelled = true;
        };
    }, [adoptTimeline, logT, startDecoding]);

    const start = useCallback(
        async (channel: number) => {
            setBusy(true);
            setError(null);
            streamStartRef.current = performance.now();
            setPhase('starting');
            try {
                await StartStream(channel);
                logT('StartStream() resolved');
            } catch (err) {
                logT('StartStream() failed', {err});
                setError(String(err));
                setPhase('error');
            } finally {
                setBusy(false);
            }
        },
        [logT],
    );

    const stop = useCallback(async () => {
        setBusy(true);
        try {
            await StopStream();
        } finally {
            setBusy(false);
            setPhase('idle');
            setError(null);
            streamStartRef.current = 0;
            cleanupPlayback();
        }
    }, [cleanupPlayback]);

    return {
        phase,
        error,
        stats,
        playing: phase === 'live' && stats.width > 0,
        start,
        stop,
        busy,
    };
}
