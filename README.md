# hik-cam-app

A Windows desktop app (Go + Wails + React/TypeScript) for low-latency live view and
license-plate (ANPR) monitoring against **one** Hikvision camera/NVR, over HCNetSDK.

Connect to a camera, start its live view on any channel, and subscribe to its onboard
plate-recognition events - the two are fully independent, either can run without the
other.

## How live view works

The camera's `NET_DVR_RealPlay_V40` private-protocol stream is a genuine (if minimal)
MPEG Program Stream: a pack header, a Program Stream Map, and PES packets carrying H.264
access units. See `internal/video/psdemux`'s package doc for the exact byte layout,
including the non-obvious part: PES packet boundaries don't align to NAL boundaries, so a
keyframe spans several packets and has to be reassembled.

```
HCNetSDK callback → internal/video/psdemux (native Go PS/PES demux, reassembles access units)
                   → this app's own length-prefixed wire format over localhost HTTP
                   → frontend fetch()/ReadableStream → WebCodecs VideoDecoder → <canvas>
```

No ffmpeg, no MediaSource Extensions, no container remux anywhere in the pipeline -
decode happens exactly once, in the browser's own decoder. `internal/video/h264` holds
the small Annex-B NAL helpers (codec/profile sniffing, keyframe detection) both the
demuxer and `stream.go` use.

ANPR is unrelated to this pipeline: it subscribes to the camera's own onboard
plate-recognition alarm events (`anpr.go`), rather than deriving anything from decoded
video frames.

### Why GPU acceleration is disabled (`main.go`)

WebView2's hardware video decoder stalls for ~650ms on large keyframes on at least one
tested machine/driver combination. Forcing software decode (`WebviewGpuIsDisabled: true`)
eliminates it completely, with per-frame decode settling around ~50ms, well inside the
~40ms budget at 25fps thanks to decode/render pipelining. If you change this, re-verify
with a long (60s+) live session and watch the logs for `render gap` / slow-decode lines.

### Watchdogs

Two independent mechanisms notice a dropped camera connection, because neither is
reliable on its own:

- **The stale-frame watchdog** (`stream.go`'s `streamStaleTimeout`, 5s). Against real
  hardware, HCNetSDK simply stops invoking its frame callback on a cable pull - no error,
  no channel close - so this is the backstop that always fires.
- **HCNetSDK's `EXCEPTION_PREVIEW` callback** (`app.go`'s `handleSDKException`), which is
  faster when it does fire, but has been observed not to fire at all for minutes after an
  actual cable pull.

Either one stops the stream and surfaces `stream:error` to the UI, instead of leaving a
frozen last frame on screen.

## Prerequisites

`third_party/go-hikvision-sdk/` is the HCNetSDK Go wrapper, carried as a git submodule
pinned to a tagged release, so clone with:

```powershell
git clone --recurse-submodules <repo-url>
# or, if already cloned:
git submodule update --init
```

The module published to the Go proxy deliberately does **not** ship Hikvision's
proprietary headers and import libraries (see the submodule's LICENSE), which is why
`go.mod` has a `replace` pointing at the submodule instead - a proxy-fetched copy cannot
cgo-compile at all.

For the same reason the submodule checkout arrives **without** them: populate
`internal/sdklib/` once per machine from your own extracted copy of the official
Hikvision SDK archives, using the script the submodule ships:

```powershell
./third_party/go-hikvision-sdk/scripts/vendor-sdk.ps1 -Win64Sdk "<path to EN-HCNetSDKV6.x_win64>"
```

**The SDK's native DLLs are not on `PATH` by default.** Any Go command that builds, tests,
or runs this module (`go build`, `go test`, `go vet`, `wails dev`, `wails build`) needs
them there first, or you'll hit a cryptic `0xc0000135` (`STATUS_DLL_NOT_FOUND`) failure:

```powershell
$env:PATH = "$PWD\third_party\go-hikvision-sdk\internal\sdklib\windows_amd64\lib;$env:PATH"
```

Set this once per shell session before running any of the commands below.

## Development

```powershell
wails dev
```

Opens the native app window and also serves the frontend at `http://localhost:34115` for
browser-based testing with working Go↔JS bindings (see `wails.json`).

## Building

```powershell
wails build
powershell -ExecutionPolicy Bypass -File build/stage-dlls.ps1
```

The built executable needs the SDK DLLs (`HCNetSDK.dll`, `HCCore.dll`, `PlayCtrl.dll`,
`HCNetSDKCom\`, ...) sitting next to it, and `wails build` does not copy them - that's
what `stage-dlls.ps1` is for. `build/bin/` is gitignored.

## Project layout

- `main.go` - Wails app options (window, GPU flag, bindings).
- `app.go` - `App` and `cameraSession` state, connect/disconnect, SDK exception handling.
- `stream.go` - RealPlay → codec sniff → PS demux → wire format pipeline.
- `httpstream.go` - the loopback `/stream` HTTP endpoint the frontend fetches.
- `broadcast.go` - fans demuxed units out to every connected viewer, non-blocking.
- `anpr.go` - plate-event subscription.
- `types.go` - DTOs and Wails event names.
- `internal/video/psdemux/` - native Go MPEG-PS/PES demuxer, unit-tested against real
  captured camera bytes in `testdata/`.
- `internal/video/h264/` - Annex-B NAL helpers shared by the demuxer and `stream.go`.
- `third_party/go-hikvision-sdk/` - the HCNetSDK Go wrapper (cgo bindings), pulled in as a
  git submodule pinned to a tagged release and linked locally via a `go.mod` replace
  directive, with its own examples covering the SDK surface this app uses (login,
  RealPlay, PTZ, alarms/ANPR, playback).
- `frontend/` - React/TypeScript UI. `src/components/LiveView.tsx` is the WebCodecs
  player; `src/components/AnprFeed.tsx` and `ConnectionPanel.tsx` round out the UI.

## Wails API surface

| Bound method | Purpose |
| --- | --- |
| `Connect(host, port, username, password)` | Log in, returns `DeviceInfoDTO` |
| `Disconnect()` | Stop stream + ANPR, log out |
| `IsConnected()` | Current login state |
| `StartStream(channel)` | Begin live view, returns the stream URL |
| `StopStream()` | End live view |
| `GetStreamInfo()` | Pull the current stream URL/codec (see below) |
| `StartANPR()` / `StopANPR()` / `IsANPRActive()` | Plate-event subscription |
| `LogFrontend(msg)` | Forward a frontend log line to Go's stdout |

Events pushed to the frontend: `stream:ready` (a `StreamReadyDTO`), `stream:error`
(string), `stream:stopped` (no payload), `anpr:event` (a `PlateEventDTO`).

`stream:ready` fires exactly once per session, so a component that mounts afterwards
would never learn a stream is already live - `GetStreamInfo` is the same information as a
pull, which `LiveView.tsx` calls on mount to catch up.


<img width="2560" height="1380" alt="Screenshot 2026-08-14 094744" src="https://github.com/user-attachments/assets/6acf6853-efd6-476b-b534-dcf89b3d7cf7" />
