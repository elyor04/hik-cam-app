package main

import (
	"embed"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	// Create an instance of the app structure
	app := NewApp()

	// Create application with options
	err := wails.Run(&options.App{
		Title: "hik-cam-app",
		// Wide enough for the live view and the plate feed side by side -
		// see frontend/src/App.tsx.
		Width:  1280,
		Height: 800,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		BackgroundColour: &options.RGBA{R: 27, G: 38, B: 54, A: 1},
		OnStartup:        app.startup,
		OnShutdown:       app.shutdown,
		// GPU-accelerated video decode in WebView2 stalls ~650ms whenever a
		// large keyframe is decoded on at least one tested machine/driver
		// combination - measured directly against the compiled app via the
		// decode-latency/render-gap logging in
		// frontend/src/components/LiveView.tsx: with GPU acceleration on,
		// every keyframe caused a 630-660ms freeze, with framesDecoded and
		// framesRendered diverging as stalled frames were silently dropped.
		// Disabling it forces software decode, which eliminates the stall
		// completely - per-frame decode settles to a steady ~50ms,
		// comfortably inside the ~40ms budget at 25fps thanks to
		// decode/render pipelining. This app has no other GPU-dependent
		// rendering (just the video <canvas> and plain UI), so the trade-off
		// costs nothing else. If you change this, re-verify with a long
		// (60s+) live session and watch for `render gap` / slow-decode log
		// lines.
		Windows: &windows.Options{
			WebviewGpuIsDisabled: true,
		},
		Bind: []interface{}{
			app,
		},
	})

	if err != nil {
		println("Error:", err.Error())
	}
}
