import {FormEvent, useState} from 'react';
import {Connect, Disconnect} from '../../wailsjs/go/main/App';
import {DeviceInfoDTO} from '../types';

interface Props {
    connected: boolean;
    deviceInfo: DeviceInfoDTO | null;
    onConnected: (info: DeviceInfoDTO) => void;
    onDisconnected: () => void;
}

export default function ConnectionPanel({connected, deviceInfo, onConnected, onDisconnected}: Props) {
    const [host, setHost] = useState('192.168.1.64');
    const [port, setPort] = useState(8000);
    const [username, setUsername] = useState('admin');
    const [password, setPassword] = useState('');
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState<string | null>(null);

    async function handleConnect(e: FormEvent) {
        e.preventDefault();
        setBusy(true);
        setError(null);
        try {
            const info = await Connect(host, port, username, password);
            onConnected(info as unknown as DeviceInfoDTO);
        } catch (err) {
            setError(String(err));
        } finally {
            setBusy(false);
        }
    }

    async function handleDisconnect() {
        setBusy(true);
        try {
            await Disconnect();
        } finally {
            setBusy(false);
            onDisconnected();
        }
    }

    if (connected) {
        return (
            <div className="panel connection-panel connected">
                <div className="connection-summary">
                    <span className="status-dot online" />
                    <div>
                        <div className="host-line">{host}:{port}</div>
                        {deviceInfo && (
                            /* The channel ranges are here because Live View's channel box needs
                               them: HCNetSDK numbers channels from startChannel/startIPChannel,
                               not from 1, and a wrong channel number is the most common reason a
                               stream starts and then never produces video. */
                            <div className="device-line">
                                S/N {deviceInfo.serialNumber || 'n/a'} · type {deviceInfo.deviceType} ·{' '}
                                {deviceInfo.ipChannels} IP ch from {deviceInfo.startIPChannel} ·{' '}
                                {deviceInfo.analogChannels} analog ch from {deviceInfo.startChannel}
                            </div>
                        )}
                    </div>
                </div>
                <button className="btn secondary" onClick={handleDisconnect} disabled={busy}>
                    Disconnect
                </button>
            </div>
        );
    }

    return (
        <form className="panel connection-panel" onSubmit={handleConnect}>
            <span className="status-dot offline" />
            <input
                className="input"
                placeholder="Camera IP"
                value={host}
                onChange={e => setHost(e.target.value)}
                required
            />
            <input
                className="input port"
                type="number"
                placeholder="Port"
                value={port}
                onChange={e => setPort(Number(e.target.value))}
                required
            />
            <input
                className="input"
                placeholder="Username"
                value={username}
                onChange={e => setUsername(e.target.value)}
                required
            />
            <input
                className="input"
                type="password"
                placeholder="Password"
                value={password}
                onChange={e => setPassword(e.target.value)}
                required
            />
            <button className="btn primary" type="submit" disabled={busy}>
                {busy ? 'Connecting…' : 'Connect'}
            </button>
            {error && <span className="error-text">{error}</span>}
        </form>
    );
}
