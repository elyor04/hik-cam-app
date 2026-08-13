package main

import (
	"encoding/base64"
	"time"

	"github.com/elyor04/go-hikvision-sdk/hikvision"
)

// Wails event names. This app drives exactly one camera, so unlike the
// two-slot reference apps (hik-cam-test, tarozi-post-app) these carry no
// per-camera suffix - there is nothing to disambiguate.
const (
	// evtStreamReady carries a StreamReadyDTO once the pipeline has sniffed
	// the codec (see stream.go's runStreamPipeline).
	evtStreamReady = "stream:ready"
	// evtStreamError carries a plain error string.
	evtStreamError = "stream:error"
	// evtStreamStopped has no payload; the pipeline has fully torn down.
	evtStreamStopped = "stream:stopped"
	// evtANPREvent carries a PlateEventDTO per plate detection.
	evtANPREvent = "anpr:event"
)

// DeviceInfoDTO is the JSON shape of hikvision.DeviceInfo returned to the
// frontend by Connect.
type DeviceInfoDTO struct {
	SerialNumber   string `json:"serialNumber"`
	AnalogChannels uint8  `json:"analogChannels"`
	StartChannel   uint8  `json:"startChannel"`
	IPChannels     uint32 `json:"ipChannels"`
	StartIPChannel uint8  `json:"startIPChannel"`
	DeviceType     uint16 `json:"deviceType"`
}

func deviceInfoToDTO(info hikvision.DeviceInfo) DeviceInfoDTO {
	return DeviceInfoDTO{
		SerialNumber:   info.SerialNumber,
		AnalogChannels: info.AnalogChannels,
		StartChannel:   info.StartChannel,
		IPChannels:     info.IPChannels,
		StartIPChannel: info.StartIPChannel,
		DeviceType:     info.DeviceType,
	}
}

// StreamReadyDTO is pushed over the "stream:ready" event, and also returned
// by GetStreamInfo for a frontend that mounted after that one-time push
// already fired (see app.go's GetStreamInfo). Codec is a plain
// `avc1.PPCCLL` string (WebCodecs' VideoDecoderConfig.codec), not a MIME
// type - there's no container format on the wire to describe one for.
type StreamReadyDTO struct {
	URL   string `json:"url"`
	Codec string `json:"codec"`
}

// PlateEventDTO is the JSON shape of hikvision.PlateEvent pushed over the
// "anpr:event" event. Images are embedded as base64 data URIs so the
// frontend can drop them straight into an <img src>.
type PlateEventDTO struct {
	License     string `json:"license"`
	Confidence  uint8  `json:"confidence"`
	SpeedKMH    uint16 `json:"speedKmh"`
	Direction   uint8  `json:"direction"`
	Lane        uint8  `json:"lane"`
	CaptureTime string `json:"captureTime"`
	ReceivedAt  string `json:"receivedAt"`
	SceneImage  string `json:"sceneImage,omitempty"`
	PlateImage  string `json:"plateImage,omitempty"`
}

func plateEventToDTO(ev hikvision.PlateEvent) PlateEventDTO {
	dto := PlateEventDTO{
		License:     ev.License,
		Confidence:  ev.Confidence,
		SpeedKMH:    ev.SpeedKMH,
		Direction:   uint8(ev.Direction),
		Lane:        ev.Lane,
		CaptureTime: ev.CaptureTime.Format(time.RFC3339),
		ReceivedAt:  time.Now().Format(time.RFC3339),
	}
	if len(ev.SceneImage) > 0 {
		dto.SceneImage = "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(ev.SceneImage)
	}
	if len(ev.PlateImage) > 0 {
		dto.PlateImage = "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(ev.PlateImage)
	}
	return dto
}
