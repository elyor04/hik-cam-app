// Hand-written to mirror types.go - Wails only auto-generates TS types for
// bound-method signatures, not for payloads pushed via EventsEmit/EventsOn.

export interface DeviceInfoDTO {
    serialNumber: string;
    analogChannels: number;
    startChannel: number;
    ipChannels: number;
    startIPChannel: number;
    deviceType: number;
}

export interface StreamReadyDTO {
    url: string;
    /** A plain `avc1.PPCCLL` string for VideoDecoderConfig.codec - not a MIME type. */
    codec: string;
}

export interface PlateEventDTO {
    license: string;
    confidence: number;
    speedKmh: number;
    /** hikvision.Direction: 0 unknown, 1 up, 2 down, 3 bidirectional, 4-7 compass, 8 other. */
    direction: number;
    /** 1-based traffic lane index, 0 if the device didn't report one. */
    lane: number;
    captureTime: string;
    receivedAt: string;
    sceneImage?: string;
    plateImage?: string;
}

// Wails event names, mirroring types.go's evt* constants. One camera, so no
// per-camera suffix.
export const EVT = {
    streamReady: 'stream:ready',
    streamError: 'stream:error',
    streamStopped: 'stream:stopped',
    anprEvent: 'anpr:event',
} as const;
