package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/elyor04/go-hikvision-sdk/hikvision"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// cameraSession holds every piece of per-camera state: the device login, the
// active live-stream pipeline, and the ANPR subscription. This app drives a
// single camera, so there is exactly one of these (App.cam) - the mutex is
// still needed, since Wails-bound methods, the HTTP stream handler, the
// demux goroutine, and HCNetSDK's own callback threads all touch it
// concurrently.
type cameraSession struct {
	mu         sync.Mutex
	device     *hikvision.Device
	connecting bool

	streamCancel    context.CancelFunc
	streamStartedAt time.Time // set on StartStream; every timing log is relative to it
	// broadcast fans this session's demuxed access units out to every
	// currently-connected HTTP viewer (see broadcast.go) - non-nil for
	// exactly as long as a stream pipeline is running, the same lifetime
	// streamCancel has.
	broadcast *broadcaster
	// streamReady caches the most recent "stream:ready" payload for as long
	// as the pipeline that produced it stays alive. That event is a
	// one-time push, so a frontend component that mounts (or remounts)
	// afterwards would otherwise never learn a stream is already live;
	// GetStreamInfo turns it into a pull. Cleared alongside
	// broadcast/streamCancel once the pipeline ends.
	streamReady   StreamReadyDTO
	streamReadyOK bool

	anprCancel context.CancelFunc
	// anprToken identifies which StartANPR call anprCancel currently belongs
	// to. context.CancelFunc values can't be compared with == (only against
	// nil), so this pointer stands in as the ownership token the delivery
	// goroutine's cleanup compares against before clearing anprCancel - see
	// StartANPR in anpr.go.
	anprToken *int
}

// App is the Wails-bound backend: one Hikvision camera's live view (H.264
// access units served over a loopback HTTP stream for browser-side WebCodecs
// decode) and its onboard ANPR plate events (pushed as Wails events).
type App struct {
	ctx context.Context
	cam cameraSession

	httpSrv  *http.Server
	httpAddr string
}

// NewApp creates a new App application struct.
func NewApp() *App {
	return &App{}
}

// startup is called when the app starts. It opens the loopback HTTP server
// (see httpstream.go) for the lifetime of the process; StartStream/StopStream
// just attach/detach the active broadcast behind the /stream route.
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx

	configureSDKComponentPath()

	if err := a.startHTTP(); err != nil {
		log.Printf("[http] %v", err)
	}

	// HCNetSDK reports a broken live-view connection (e.g. the camera's
	// network cable pulled) through this process-wide callback, generally
	// well before its own NET_DVR_SetReconnect interval would notice.
	// Registering it is best-effort: runDemuxPipeline's stale-frame
	// watchdog (stream.go) independently catches the same condition within
	// streamStaleTimeout, which matters because against real hardware this
	// callback has been observed *not* to fire at all on a cable pull.
	if err := hikvision.OnException(a.handleSDKException); err != nil {
		log.Printf("[sdk] OnException registration failed, network drops will only be caught by the stale-stream watchdog: %v", err)
	}
}

// shutdown tears everything down when the app quits.
func (a *App) shutdown(ctx context.Context) {
	_ = a.StopANPR()
	_ = a.StopStream()

	a.cam.mu.Lock()
	dev := a.cam.device
	a.cam.device = nil
	a.cam.mu.Unlock()
	if dev != nil {
		_ = dev.Close()
	}

	if a.httpSrv != nil {
		shCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = a.httpSrv.Shutdown(shCtx)
	}
}

// configureSDKComponentPath points HCNetSDK at the executable's own
// directory when a packaged HCNetSDKCom/ folder is found there (see
// build/stage-dlls.ps1). Running from the module directory during
// development needs no override - the SDK wrapper defaults to its own
// vendored lib dir. Must run before the first Login, hence startup.
//
// Getting this wrong is quiet rather than loud: the app still launches and
// even logs in, then fails at the first real SDK call (RealPlay,
// PlateEvents) with an opaque error code rather than a DLL-load error.
func configureSDKComponentPath() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	dir := filepath.Dir(exe)
	if _, err := os.Stat(filepath.Join(dir, "HCNetSDKCom")); err != nil {
		return // dev-mode run (PATH-based), nothing to override
	}
	if err := hikvision.Configure(hikvision.Config{ComponentPath: dir, AutoReconnect: true}); err != nil {
		log.Printf("[sdk] Configure failed: %v", err)
	}
}

// handleSDKException is HCNetSDK's process-wide exception callback. Only
// ExceptionPreview is acted on - its real-time signal that this live-view
// connection just broke - by tearing the stream down so the UI surfaces it
// immediately instead of showing a frozen last frame. The device login
// itself is left alone: this app connects manually, so the user decides
// whether to restart the stream or reconnect entirely.
func (a *App) handleSDKException(t hikvision.ExceptionType, userID, handle int32) {
	if t != hikvision.ExceptionPreview {
		return
	}
	a.cam.mu.Lock()
	dev := a.cam.device
	streaming := a.cam.streamCancel != nil
	a.cam.mu.Unlock()
	if dev == nil || dev.UserID() != userID || !streaming {
		return
	}
	log.Printf("[sdk] preview exception (likely network drop), stopping stream (handle=%d)", handle)
	// Never call back into the SDK synchronously from inside this callback:
	// HCNetSDK invokes it from its own internal thread, and StopStream's
	// cancellation leads to blocking SDK calls (StopRealPlay) that could
	// deadlock or re-enter if run inline here.
	go func() {
		a.emit(evtStreamError, "camera connection lost (preview exception reported by the SDK)")
		_ = a.StopStream()
	}()
}

// Connect logs into the camera/NVR at the given address.
func (a *App) Connect(host string, port int, username, password string) (DeviceInfoDTO, error) {
	cs := &a.cam

	cs.mu.Lock()
	if cs.device != nil || cs.connecting {
		cs.mu.Unlock()
		return DeviceInfoDTO{}, fmt.Errorf("already connected or connecting")
	}
	cs.connecting = true
	cs.mu.Unlock()
	defer func() {
		cs.mu.Lock()
		cs.connecting = false
		cs.mu.Unlock()
	}()

	log.Printf("[device] login %s:%d as %q", host, port, username)
	dev, err := hikvision.Login(hikvision.LoginOptions{
		Address:  host,
		Port:     uint16(port),
		Username: username,
		Password: password,
	})
	if err != nil {
		log.Printf("[device] login failed: %v", err)
		return DeviceInfoDTO{}, fmt.Errorf("connect: %w", err)
	}
	log.Printf("[device] logged in: %s", dev)

	cs.mu.Lock()
	cs.device = dev
	cs.mu.Unlock()
	return deviceInfoToDTO(dev.Info), nil
}

// Disconnect stops any active stream/ANPR subscription and logs out.
func (a *App) Disconnect() error {
	cs := &a.cam

	cs.mu.Lock()
	dev := cs.device
	cs.mu.Unlock()
	if dev == nil {
		return nil
	}

	_ = a.StopStream()
	_ = a.StopANPR()

	cs.mu.Lock()
	cs.device = nil
	cs.mu.Unlock()
	log.Printf("[device] logging out")
	return dev.Close()
}

// IsConnected reports whether a device session is currently open.
func (a *App) IsConnected() bool {
	a.cam.mu.Lock()
	defer a.cam.mu.Unlock()
	return a.cam.device != nil
}

// GetStreamInfo returns the currently-active stream's URL/codec, or an error
// if no stream pipeline is live right now. This is the "stream:ready" event
// as a pull instead of the one-time push it is - see cameraSession.streamReady.
func (a *App) GetStreamInfo() (StreamReadyDTO, error) {
	a.cam.mu.Lock()
	defer a.cam.mu.Unlock()
	if !a.cam.streamReadyOK {
		return StreamReadyDTO{}, fmt.Errorf("no active stream")
	}
	return a.cam.streamReady, nil
}

// LogFrontend forwards a frontend console line into this process's own
// stdout (see frontend/src/components/LiveView.tsx's logT). A production
// build has no attached devtools console, so this is what makes frontend
// timing/diagnostic logs visible at all outside a `wails dev` browser window.
func (a *App) LogFrontend(msg string) {
	log.Printf("[frontend] %s", msg)
}

// emit pushes a Wails event, tolerating a nil ctx (possible only if an SDK
// callback fires before startup, which shouldn't happen but is cheap to
// guard).
func (a *App) emit(name string, data ...interface{}) {
	if a.ctx == nil {
		return
	}
	wailsruntime.EventsEmit(a.ctx, name, data...)
}
