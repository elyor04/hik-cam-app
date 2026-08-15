import {useRef, useState} from 'react';
import {useCameraStream} from '../hooks/useCameraStream';

// The decoder, wire-format parser, fetch reader, telemetry and lifecycle state
// all live in useCameraStream. This component is only the panel around them.
export default function LiveView() {
    const [channel, setChannel] = useState(1);
    const canvasRef = useRef<HTMLCanvasElement | null>(null);
    const {phase, error, stats, playing, start, stop, busy} = useCameraStream(canvasRef);

    // 'idle' and 'error' are the two phases a stream can be started from; the
    // rest mean a pipeline is already running on the Go side, whether or not it
    // has produced a frame yet.
    const canStart = phase === 'idle' || phase === 'error';

    function placeholder() {
        if (error) return <span className="error-text">{error}</span>;
        if (phase === 'starting') return 'Starting stream…';
        if (phase === 'live') return 'Waiting for video…';
        return 'Stream not started';
    }

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
                            disabled={!canStart}
                        />
                    </label>
                    {canStart ? (
                        <button className="btn primary" onClick={() => start(channel)} disabled={busy}>
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
                {!playing && <div className="video-placeholder">{placeholder()}</div>}
            </div>
        </div>
    );
}
