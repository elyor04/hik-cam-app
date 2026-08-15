import {useEffect, useState} from 'react';
import {IsANPRActive, StartANPR, StopANPR} from '../../wailsjs/go/main/App';
import {EventsOn} from '../../wailsjs/runtime';
import {EVT, PlateEventDTO} from '../types';

const MAX_EVENTS = 50;

// hikvision.Direction - only the values a plate event realistically carries
// are named; anything else falls back to the raw number. DirectionUnknown (0)
// is what older, non-ITS alarm formats report, so it is common, not an error.
const DIRECTION_LABEL: Record<number, string> = {
    0: 'unknown',
    1: 'up',
    2: 'down',
    3: 'bidirectional',
    4: 'westward',
    5: 'northward',
    6: 'eastward',
    7: 'southward',
    8: 'other',
};

export default function AnprFeed() {
    const [active, setActive] = useState(false);
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState<string | null>(null);
    const [events, setEvents] = useState<PlateEventDTO[]>([]);

    useEffect(() => {
        const off = EventsOn(EVT.anprEvent, (dto: PlateEventDTO) => {
            setEvents(prev => [dto, ...prev].slice(0, MAX_EVENTS));
        });
        // A remount while a subscription is still live (React strict-mode
        // double mount, or this panel being rebuilt) would otherwise show
        // "Start" for an already-running subscription, and clicking it would
        // just error out with "ANPR already active".
        IsANPRActive()
            .then(setActive)
            .catch(() => {});
        return off;
    }, []);

    async function start() {
        setBusy(true);
        setError(null);
        try {
            await StartANPR();
            setActive(true);
        } catch (err) {
            setError(String(err));
        } finally {
            setBusy(false);
        }
    }

    async function stop() {
        setBusy(true);
        try {
            await StopANPR();
        } finally {
            setBusy(false);
            setActive(false);
        }
    }

    return (
        <div className="panel anpr-feed">
            <div className="panel-header">
                <h2>Plate Detections {events.length > 0 && <span className="count">({events.length})</span>}</h2>
                <div className="controls">
                    {events.length > 0 && (
                        <button className="btn ghost" onClick={() => setEvents([])}>
                            Clear
                        </button>
                    )}
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
            {error && <div className="error-text">{error}</div>}
            <ul className="plate-list">
                {events.length === 0 && (
                    <li className="plate-empty">{active ? 'Waiting for detections…' : 'Not subscribed'}</li>
                )}
                {events.map(ev => (
                    // Keyed by the backend's own monotonic ev.seq. This list is prepended
                    // to, so the array index is not event identity - every surviving row's
                    // key shifted by one on each detection, making React discard and rebuild
                    // all of them (including each <img>'s full base64 JPEG src) instead of
                    // leaving them alone. receivedAt can't stand in either: it's
                    // second-resolution, so two plates read in the same second collide.
                    <li className="plate-item" key={ev.seq}>
                        {(ev.plateImage || ev.sceneImage) && (
                            <img
                                className="plate-thumb"
                                src={ev.plateImage || ev.sceneImage}
                                alt={ev.license}
                            />
                        )}
                        <div className="plate-info">
                            <div className="plate-license">{ev.license || 'unknown'}</div>
                            <div className="plate-meta">
                                {ev.confidence}% confidence · {ev.speedKmh} km/h ·{' '}
                                {DIRECTION_LABEL[ev.direction] ?? ev.direction}
                                {ev.lane > 0 ? ` · lane ${ev.lane}` : ''}
                            </div>
                            <div className="plate-time">{new Date(ev.captureTime).toLocaleString()}</div>
                        </div>
                    </li>
                ))}
            </ul>
        </div>
    );
}
