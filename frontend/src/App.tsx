import {useEffect, useState} from 'react';
import './App.css';
import ConnectionPanel from './components/ConnectionPanel';
import LiveView from './components/LiveView';
import AnprFeed from './components/AnprFeed';
import {IsConnected} from '../wailsjs/go/main/App';
import {DeviceInfoDTO} from './types';

function App() {
    const [connected, setConnected] = useState(false);
    const [deviceInfo, setDeviceInfo] = useState<DeviceInfoDTO | null>(null);

    // The Go side owns the login, and it outlives a frontend reload (`wails
    // dev` hot-reloads the webview without restarting the process), so ask
    // it what the truth is rather than assuming "disconnected" on mount.
    useEffect(() => {
        IsConnected()
            .then(setConnected)
            .catch(() => {});
    }, []);

    return (
        <div id="App">
            <header className="app-header">
                <h1>Hikvision Live View &amp; ANPR</h1>
                <ConnectionPanel
                    connected={connected}
                    deviceInfo={deviceInfo}
                    onConnected={info => {
                        setDeviceInfo(info);
                        setConnected(true);
                    }}
                    onDisconnected={() => {
                        setConnected(false);
                        setDeviceInfo(null);
                    }}
                />
            </header>
            <main className="app-main">
                {connected ? (
                    <>
                        <LiveView />
                        <AnprFeed />
                    </>
                ) : (
                    <div className="empty-state">Connect to a camera to begin.</div>
                )}
            </main>
        </div>
    );
}

export default App;
