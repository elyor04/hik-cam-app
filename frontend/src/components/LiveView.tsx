import {useEffect, useRef, useState} from 'react';
import {GetStreamInfo, LogFrontend, StartStream, StopStream} from '../../wailsjs/go/main/App';
import {EventsOn} from '../../wailsjs/runtime';
import {EVT, StreamReadyDTO} from '../types';

// Wire format written by stream.go's runDemuxPipeline: repeated
// [1 byte flags (bit0 = keyframe)][4-byte big-endian payload length][payload]
// messages, each payload one complete Annex-B H.264 access unit - no
// container, no ffmpeg, no MSE involved. Decoding happens here, once, via
// WebCodecs directly.
const WIRE_HEADER_SIZE = 5;
const WIRE_FLAG_KEYFRAME = 0x01;

const STATS_INTERVAL_MS = 1000;

// Consecutive decoder.decode() throws with no successful output in between
// before the failure is surfaced to the user. A single throw is expected and
// self-healing (a delta chunk fed before the decoder has seen a keyframe,
// which the stream's next keyframe fixes), but a sustained run means
// something is genuinely wrong and would otherwise be invisible outside a
// dev console.
const DECODE_FAILURE_THRESHOLD = 10;

export default function LiveView() {
    const [channel, setChannel] = useState(1);
    const [active, setActive] = useState(false);
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState<string | null>(null);
    const [stats, setStats] = useState({fps: 0, width: 0, height: 0});

    const canvasRef = useRef<HTMLCanvasElement | null>(null);
    const abortRef = useRef<AbortController | null>(null);
    const decoderRef = useRef<VideoDecoder | null>(null);
    const rafRef = useRef<number | null>(null);
    const statsIntervalRef = useRef<number | null>(null);
    const latestFrameRef = useRef<VideoFrame | null>(null);
    // t0 for every timing log in a session - set the moment the user clicks
    // Start, reused everywhere else so the Go-side logs (also relative to
    // their own StartStream t0) and these line up on one timeline.
    const streamStartRef = useRef<number>(0);
    // Bumped by every startDecoding call, whichever trigger fired it. Lets
    // the GetStreamInfo catch-up effect notice it was superseded by a
    // stream:ready event that arrived while its request was still in flight,
    // and skip acting on its now-stale response.
    const generationRef = useRef(0);

    function logT(label: string, extra?: unknown) {
        const t = ((performance.now() - streamStartRef.current) / 1000).toFixed(2);
        const line = `[t+${t}s] ${label} ${extra !== undefined ? JSON.stringify(extra) : ''}`;
        console.log(`[LiveView]${line}`);
        // A production build has no attached devtools console - forward to
        // Go's stdout (see app.go's LogFrontend) so these are visible at all
        // when reproducing an issue outside `wails dev`. Fire-and-forget:
        // never let a logging call itself affect playback.
        LogFrontend(line).catch(() => {});
    }

    useEffect(() => {
        const offReady = EventsOn(EVT.streamReady, (evt: StreamReadyDTO) => {
            logT('stream:ready event received', evt);
            setActive(true);
            setError(null);
            startDecoding(evt.url, evt.codec);
        });
        const offError = EventsOn(EVT.streamError, (msg: string) => {
            logT('stream:error event received', {msg});
            setError(msg);
            setActive(false);
            cleanupPlayback();
        });
        const offStopped = EventsOn(EVT.streamStopped, () => {
            logT('stream:stopped event received');
            setActive(false);
            cleanupPlayback();
        });
        return () => {
            offReady();
            offError();
            offStopped();
            cleanupPlayback();
        };
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, []);

    // Catches up on a "stream:ready" this component's own mount missed. That
    // event is a one-time push, so a remount (React strict-mode double
    // mount, or this panel being torn down and rebuilt) while a pipeline is
    // still running would otherwise leave the canvas permanently blank even
    // though the camera is streaming fine. Rejects harmlessly when nothing
    // is live, in which case this stays idle exactly as before.
    useEffect(() => {
        let cancelled = false;
        const myGeneration = generationRef.current;
        GetStreamInfo()
            .then(info => {
                // A stream:ready event can arrive and call startDecoding
                // while this request is still in flight - generationRef
                // changing means exactly that, and acting on this stale
                // response would tear down the correct, already-decoding
                // session and reconfigure it with a possibly-outdated codec.
                if (cancelled || generationRef.current !== myGeneration) return;
                logT('GetStreamInfo caught up with a live stream', info);
                setActive(true);
                startDecoding(info.url, info.codec);
            })
            .catch(() => {});
        return () => {
            cancelled = true;
        };
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, []);

    function cleanupPlayback() {
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
        setStats({fps: 0, width: 0, height: 0});
    }

    function startDecoding(url: string, codec: string) {
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
            setError('canvas 2D context unavailable');
            return;
        }
        if (!('VideoDecoder' in window)) {
            setError('Browser does not support WebCodecs (VideoDecoder)');
            return;
        }

        let framesDecoded = 0;
        let framesRendered = 0;
        let firstFrameLogged = false;
        let firstRenderLogged = false;
        let consecutiveDecodeErrors = 0;

        // decodeCallTimes pairs each decode() call with its eventual output()
        // to track real per-frame decode latency. Baseline has no B-frames,
        // so decode order == output order - a plain FIFO queue is a valid
        // pairing, not a heuristic. Surfaced via the periodic stats log
        // rather than per-frame (at ~25fps that would be a line every 40ms).
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
                setError(`decode error: ${err.message}`);
            },
        });
        try {
            decoder.configure({codec});
        } catch (err) {
            setError(`VideoDecoder.configure failed: ${err}`);
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
        // reallocated to the exact new size on every chunk: keyframes
        // (~19x a P-frame's size, per stream.go's own logging) arrive across
        // many small network reads, and naively reallocating + copying the
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
                    // stalled (matching a slow Go-side write - see stream.go's
                    // writeDur log); a fast read() but a big gap since the
                    // *previous* chunk finished processing would instead point
                    // at this loop's own body (message extraction + decode
                    // calls) being slow.
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
                            decodeCallTimes.push({t: performance.now()});
                            decoder.decode(
                                new EncodedVideoChunk({
                                    type: keyframe ? 'key' : 'delta',
                                    timestamp: ts,
                                    data: payload as BufferSource,
                                }),
                            );
                        } catch (err) {
                            consecutiveDecodeErrors++;
                            logT('decoder.decode threw', {err: String(err), consecutiveDecodeErrors});
                            if (consecutiveDecodeErrors >= DECODE_FAILURE_THRESHOLD) {
                                setError('decoding is failing repeatedly - the stream may be a codec this build cannot decode');
                            }
                        }
                    }
                }
                logT('fetch reader done', {totalChunks: chunkCount, totalUnits: unitCount});
            } catch (err) {
                if ((err as DOMException).name !== 'AbortError') {
                    setError(`stream read failed: ${err}`);
                }
            }
        })();
    }

    async function start() {
        setBusy(true);
        setError(null);
        streamStartRef.current = performance.now();
        try {
            await StartStream(channel);
            logT('StartStream() resolved');
            setActive(true);
        } catch (err) {
            logT('StartStream() failed', {err});
            setError(String(err));
        } finally {
            setBusy(false);
        }
    }

    async function stop() {
        setBusy(true);
        try {
            await StopStream();
        } finally {
            setBusy(false);
            setActive(false);
            cleanupPlayback();
        }
    }

    // A stream is "playing" only once frames are actually being drawn; until
    // then the backend is still logging in, sniffing the codec, etc.
    const playing = active && stats.width > 0;

    return (
        <div className="panel live-view">
            <div className="panel-header">
                <h2>Live View</h2>
                <div className="controls">
                    <label>
                        Channel
                        <input
                            className="input channel"
                            type="number"
                            min={1}
                            value={channel}
                            onChange={e => setChannel(Number(e.target.value))}
                            disabled={active}
                        />
                    </label>
                    {!active ? (
                        <button className="btn primary" onClick={start} disabled={busy}>
                            Start
                        </button>
                    ) : (
                        <button className="btn secondary" onClick={stop} disabled={busy}>
                            Stop
                        </button>
                    )}
                </div>
            </div>
            <div className="video-frame">
                <canvas ref={canvasRef} style={{display: playing ? 'block' : 'none'}} />
                {playing && (
                    <div className="video-tag">
                        <span className="status-dot live" /> LIVE · {stats.width}×{stats.height}
                        {stats.fps ? ` · ${stats.fps} fps` : ''}
                    </div>
                )}
                {!playing && (
                    <div className="video-placeholder">
                        {error ? (
                            <span className="error-text">{error}</span>
                        ) : active ? (
                            'Starting stream…'
                        ) : (
                            'Stream not started'
                        )}
                    </div>
                )}
            </div>
            {playing && error && <div className="error-text">{error}</div>}
        </div>
    );
}
