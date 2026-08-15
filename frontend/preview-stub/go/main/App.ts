// FRONTEND-ONLY PREVIEW STUB -- see preview-stub/README.md.
//
// Mirrors the bound-method surface of app.go/stream.go/anpr.go with demo data, so the UI can be
// rendered in a plain browser on a machine that cannot build the Go backend.
//
// Unlike a purely static stub, the live view here is real: a synthetic H.264 stream is encoded in
// the browser with WebCodecs' VideoEncoder, framed in this app's own wire format, and served
// through an intercepted fetch. That means `npm run dev` exercises the genuine path -- wire parser,
// VideoDecoder, render loop, stats -- rather than just drawing the panel around a hole. What it
// does NOT cover is anything on the Go side of the boundary: RealPlay, the PS demuxer, the
// broadcaster, and the loopback HTTP server are all absent here and only run against real hardware.

import { EventsEmit } from '../../runtime';

const now = () => new Date().toISOString();

function logStub(name: string, ...args: unknown[]) {
  // eslint-disable-next-line no-console
  console.info(`[wailsjs stub] ${name}`, ...args);
}

// Event names, mirroring types.go's evt* constants and frontend/src/types.ts's EVT.
const EVT_STREAM_READY = 'stream:ready';
const EVT_STREAM_ERROR = 'stream:error';
const EVT_STREAM_STOPPED = 'stream:stopped';
const EVT_ANPR_EVENT = 'anpr:event';

// ---------------------------------------------------------------------------
// Wire format -- must match internal/wire's Encode exactly:
// [1 byte flags (bit0 = keyframe)][4-byte big-endian payload length][payload]
// ---------------------------------------------------------------------------

const WIRE_HEADER_SIZE = 5;
const WIRE_FLAG_KEYFRAME = 0x01;

function encodeWireMessage(payload: Uint8Array, keyframe: boolean): Uint8Array {
  const msg = new Uint8Array(WIRE_HEADER_SIZE + payload.byteLength);
  msg[0] = keyframe ? WIRE_FLAG_KEYFRAME : 0;
  new DataView(msg.buffer).setUint32(1, payload.byteLength, false); // big-endian
  msg.set(payload, WIRE_HEADER_SIZE);
  return msg;
}

// ---------------------------------------------------------------------------
// Synthetic camera stream
// ---------------------------------------------------------------------------

// A deliberately unroutable host (RFC 2606 reserves .invalid): if the fetch interception below ever
// fails to match, the request fails immediately and obviously instead of escaping to the network.
const STREAM_URL = 'https://preview-stub.invalid/stream';
const STREAM_CODEC = 'avc1.42001f'; // Baseline 3.1 -- what this project's camera actually sends
const STREAM_WIDTH = 960;
const STREAM_HEIGHT = 540;
const STREAM_FPS = 25;
const KEYFRAME_INTERVAL = STREAM_FPS * 2; // a 2s GOP, like the real camera

/** Draws one frame of an obviously-synthetic test pattern, so nobody mistakes this for a camera. */
function drawTestPattern(ctx: CanvasRenderingContext2D, frameNo: number) {
  const t = frameNo / STREAM_FPS;

  const bg = ctx.createLinearGradient(0, 0, STREAM_WIDTH, STREAM_HEIGHT);
  bg.addColorStop(0, '#0f242b');
  bg.addColorStop(1, '#14323d');
  ctx.fillStyle = bg;
  ctx.fillRect(0, 0, STREAM_WIDTH, STREAM_HEIGHT);

  // Moving bar -- motion is what makes a decode stall visible by eye.
  const x = (t * 220) % (STREAM_WIDTH + 160) - 80;
  ctx.fillStyle = '#41c7d8';
  ctx.fillRect(x, 0, 80, STREAM_HEIGHT);

  // Sweeping second hand, so a frozen canvas is unmistakable.
  ctx.strokeStyle = '#f2f6f7';
  ctx.lineWidth = 4;
  ctx.beginPath();
  ctx.moveTo(STREAM_WIDTH / 2, STREAM_HEIGHT / 2);
  ctx.lineTo(
    STREAM_WIDTH / 2 + Math.cos(t * Math.PI * 2 - Math.PI / 2) * 120,
    STREAM_HEIGHT / 2 + Math.sin(t * Math.PI * 2 - Math.PI / 2) * 120,
  );
  ctx.stroke();

  ctx.fillStyle = '#f2f6f7';
  ctx.font = 'bold 34px ui-monospace, Menlo, monospace';
  ctx.fillText('PREVIEW STUB — synthetic stream', 40, 70);
  ctx.font = '26px ui-monospace, Menlo, monospace';
  ctx.fillText(`frame ${frameNo}   t+${t.toFixed(2)}s`, 40, STREAM_HEIGHT - 48);
}

/**
 * Produces the wire-framed byte stream the frontend's reader loop consumes, encoding frames on the
 * fly. Annex-B is requested explicitly: WebCodecs defaults H.264 output to AVCC (length-prefixed
 * NALs), which the decoder on the other side of this pipeline would reject outright, since the real
 * Go path is Annex-B from the demuxer onward.
 */
function syntheticStreamBody(signal?: AbortSignal | null): ReadableStream<Uint8Array> {
  let encoder: VideoEncoder | null = null;
  let timer: number | undefined;
  let frameNo = 0;

  return new ReadableStream<Uint8Array>({
    start(controller) {
      const canvas = document.createElement('canvas');
      canvas.width = STREAM_WIDTH;
      canvas.height = STREAM_HEIGHT;
      const ctx = canvas.getContext('2d');
      if (!ctx) {
        controller.error(new Error('preview stub: no 2D canvas context'));
        return;
      }

      const stop = () => {
        if (timer !== undefined) window.clearInterval(timer);
        timer = undefined;
        try {
          if (encoder && encoder.state !== 'closed') encoder.close();
        } catch {
          // already closing - fine
        }
        encoder = null;
        try {
          controller.close();
        } catch {
          // already closed - fine
        }
      };

      encoder = new VideoEncoder({
        output: (chunk) => {
          const payload = new Uint8Array(chunk.byteLength);
          chunk.copyTo(payload);
          try {
            controller.enqueue(encodeWireMessage(payload, chunk.type === 'key'));
          } catch {
            stop(); // reader went away
          }
        },
        error: (err) => {
          controller.error(err);
          stop();
        },
      });

      encoder.configure({
        codec: STREAM_CODEC,
        width: STREAM_WIDTH,
        height: STREAM_HEIGHT,
        bitrate: 2_000_000,
        framerate: STREAM_FPS,
        latencyMode: 'realtime',
        avc: { format: 'annexb' },
      });

      signal?.addEventListener('abort', stop, { once: true });

      timer = window.setInterval(() => {
        if (!encoder || encoder.state !== 'configured') return;
        // Never let an encoder that has fallen behind grow an unbounded queue - drop instead, the
        // same shedding policy the real broadcaster applies to a slow viewer.
        if (encoder.encodeQueueSize > 4) return;
        drawTestPattern(ctx, frameNo);
        const frame = new VideoFrame(canvas, {
          timestamp: Math.round((frameNo * 1_000_000) / STREAM_FPS),
        });
        encoder.encode(frame, { keyFrame: frameNo % KEYFRAME_INTERVAL === 0 });
        frame.close();
        frameNo++;
      }, 1000 / STREAM_FPS);
    },
    cancel() {
      if (timer !== undefined) window.clearInterval(timer);
      try {
        if (encoder && encoder.state !== 'closed') encoder.close();
      } catch {
        // already closing - fine
      }
    },
  });
}

/**
 * Serves STREAM_URL from inside the page.
 *
 * The frontend fetches its video over real HTTP from the Go process's loopback listener, so a stub
 * that only mocks bound methods leaves the live view permanently empty. Intercepting one exact URL
 * is what lets the genuine reader/parser/decoder path run here. Everything else is delegated
 * untouched, so Vite's own requests and HMR are unaffected.
 */
function installStreamFetchInterceptor() {
  const w = window as typeof window & { __previewStubFetchPatched?: boolean };
  if (w.__previewStubFetchPatched) return;
  w.__previewStubFetchPatched = true;

  const realFetch = window.fetch.bind(window);
  window.fetch = ((input: RequestInfo | URL, init?: RequestInit) => {
    const url =
      typeof input === 'string' ? input : input instanceof URL ? input.href : (input as Request).url;
    if (url !== STREAM_URL) return realFetch(input as RequestInfo, init);

    if (typeof VideoEncoder === 'undefined') {
      return Promise.reject(
        new Error('preview stub: this browser has no WebCodecs VideoEncoder, cannot synthesise a stream'),
      );
    }
    return Promise.resolve(
      new Response(syntheticStreamBody(init?.signal), {
        status: 200,
        headers: { 'Content-Type': 'application/octet-stream' },
      }),
    );
  }) as typeof window.fetch;
}

installStreamFetchInterceptor();

// ---------------------------------------------------------------------------
// Device connection
// ---------------------------------------------------------------------------

const deviceInfo = {
  serialNumber: 'PREVIEW-STUB-0001',
  analogChannels: 0,
  startChannel: 1,
  ipChannels: 2,
  startIPChannel: 1,
  deviceType: 0,
};

let connected = false;

export function Connect(host: string, port: number, username: string, password: string) {
  logStub('Connect', host, port, username, password ? '***' : '');
  connected = true;
  return Promise.resolve(deviceInfo);
}

export function Disconnect() {
  logStub('Disconnect');
  void StopStream();
  void StopANPR();
  connected = false;
  return Promise.resolve();
}

export function IsConnected() {
  logStub('IsConnected');
  return Promise.resolve(connected);
}

// ---------------------------------------------------------------------------
// Live view
// ---------------------------------------------------------------------------

let streamActive = false;
let streamReady: { url: string; codec: string } | null = null;
let sniffTimer: number | undefined;

// Mimics the backend's codec sniff: "stream:ready" only lands once an SPS has been seen, which
// against real hardware takes a few hundred ms. Keeping the delay here is what lets the preview
// exercise the hook's 'starting' phase and its IsStreamActive catch-up path at all.
const SNIFF_DELAY_MS = 600;

export function StartStream(channel: number) {
  logStub('StartStream', channel);
  if (!connected) return Promise.reject('not connected');
  if (streamActive) return Promise.reject('stream already active');

  streamActive = true;
  streamReady = null;
  sniffTimer = window.setTimeout(() => {
    if (!streamActive) return;
    streamReady = { url: STREAM_URL, codec: STREAM_CODEC };
    EventsEmit(EVT_STREAM_READY, streamReady);
  }, SNIFF_DELAY_MS);

  return Promise.resolve();
}

export function StopStream() {
  logStub('StopStream');
  if (sniffTimer !== undefined) {
    window.clearTimeout(sniffTimer);
    sniffTimer = undefined;
  }
  if (!streamActive) return Promise.resolve();
  streamActive = false;
  streamReady = null;
  EventsEmit(EVT_STREAM_STOPPED);
  return Promise.resolve();
}

export function IsStreamActive() {
  logStub('IsStreamActive');
  return Promise.resolve(streamActive);
}

export function GetStreamInfo(): Promise<{ url: string; codec: string }> {
  logStub('GetStreamInfo');
  // Rejects while the sniff is still "running", exactly as the Go side does - that window is the
  // reason IsStreamActive exists.
  if (!streamReady) return Promise.reject('no active stream');
  return Promise.resolve(streamReady);
}

export function LogFrontend(msg: string) {
  logStub('LogFrontend', msg);
  return Promise.resolve();
}

// ---------------------------------------------------------------------------
// ANPR
// ---------------------------------------------------------------------------

const DEMO_PLATES = ['01A123BC', '01B456DE', '01C789FG', '30H555AA', '80K021PC'];

let anprActive = false;
let anprTimer: number | undefined;
// Mirrors App.anprSeq: a per-process monotonic id, and the plate feed's React key. Deliberately not
// reset by StopANPR - the feed keeps its rows across a stop/start, so a restarting counter would
// hand a new event the same key as a row still on screen.
let anprSeq = 0;

/** A small JPEG data URI, so the feed's thumbnails render like they do against real hardware. */
function demoPlateImage(text: string): string {
  const canvas = document.createElement('canvas');
  canvas.width = 160;
  canvas.height = 48;
  const ctx = canvas.getContext('2d');
  if (!ctx) return '';
  ctx.fillStyle = '#f2f2f2';
  ctx.fillRect(0, 0, canvas.width, canvas.height);
  ctx.fillStyle = '#111';
  ctx.font = 'bold 24px ui-monospace, Menlo, monospace';
  ctx.fillText(text, 10, 33);
  return canvas.toDataURL('image/jpeg', 0.8);
}

export function StartANPR() {
  logStub('StartANPR');
  if (!connected) return Promise.reject('not connected');
  if (anprActive) return Promise.reject('ANPR already active');
  anprActive = true;

  anprTimer = window.setInterval(() => {
    const license = DEMO_PLATES[Math.floor(Math.random() * DEMO_PLATES.length)];
    anprSeq++;
    EventsEmit(EVT_ANPR_EVENT, {
      seq: anprSeq,
      license,
      confidence: 80 + Math.floor(Math.random() * 20),
      speedKmh: 5 + Math.floor(Math.random() * 20),
      direction: 2,
      lane: 1,
      captureTime: now(),
      receivedAt: now(),
      plateImage: demoPlateImage(license),
      sceneImage: '',
    });
  }, 4000);

  return Promise.resolve();
}

export function StopANPR() {
  logStub('StopANPR');
  if (anprTimer !== undefined) {
    window.clearInterval(anprTimer);
    anprTimer = undefined;
  }
  anprActive = false;
  return Promise.resolve();
}

export function IsANPRActive() {
  logStub('IsANPRActive');
  return Promise.resolve(anprActive);
}
