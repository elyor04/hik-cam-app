# Copies the vendored Hikvision SDK's native DLLs next to the built .exe.
#
# `wails build` does not do this automatically (Wails v2's wails.json has no postbuild-hook
# mechanism), so run this script manually after every `wails build`. Without it the built
# executable fails to launch at all with a bare 0xc0000135 (STATUS_DLL_NOT_FOUND).
#
# Usage (from the repo root):
#   wails build
#   powershell -ExecutionPolicy Bypass -File build/stage-dlls.ps1
#
# Missing the HCNetSDKCom/ subfolder specifically is the single most likely deployment
# mistake: the app will still launch and even log in, then fail at the first real SDK call
# (RealPlay/PlateEvents) with an opaque SDK error code -- not a DLL-load error -- so it is
# easy to miss until the camera is actually exercised.

$ErrorActionPreference = 'Stop'

$repoRoot = Split-Path -Parent $PSScriptRoot
$sdkLib = Join-Path $repoRoot 'third_party\go-hikvision-sdk\internal\sdklib\windows_amd64\lib'
$dst = Join-Path $repoRoot 'build\bin'

if (-not (Test-Path $sdkLib)) {
    Write-Error "Vendored SDK lib dir not found at '$sdkLib'. Is third_party/go-hikvision-sdk populated? See README."
}
if (-not (Test-Path $dst)) {
    Write-Error "Build output dir not found at '$dst'. Run 'wails build' first."
}

Write-Host "Staging Hikvision SDK DLLs from $sdkLib to $dst ..."
Copy-Item -Path (Join-Path $sdkLib '*.dll') -Destination $dst -Force

$comSrc = Join-Path $sdkLib 'HCNetSDKCom'
$comDst = Join-Path $dst 'HCNetSDKCom'
if (-not (Test-Path $comSrc)) {
    Write-Error "HCNetSDKCom/ plugin subfolder not found at '$comSrc' -- the SDK vendor step is incomplete."
}
Copy-Item -Path $comSrc -Destination $comDst -Recurse -Force

$requiredDlls = @(
    'HCNetSDK.dll', 'HCCore.dll', 'PlayCtrl.dll', 'AudioRender.dll', 'SuperRender.dll',
    'MP_Render.dll', 'HXVA.dll', 'HmMerge.dll', 'NPQos.dll', 'OpenAL32.dll', 'GdiPlus.dll',
    'hlog.dll', 'hpr.dll', 'zlib1.dll', 'libcrypto-1_1-x64.dll', 'libssl-1_1-x64.dll', 'libmmd.dll'
)
$missing = $requiredDlls | Where-Object { -not (Test-Path (Join-Path $dst $_)) }
if ($missing.Count -gt 0) {
    Write-Warning "Missing expected DLLs in build/bin/: $($missing -join ', ')"
} else {
    Write-Host "All expected DLLs present in build/bin/."
}

Write-Host "Done. build/bin/ is now a self-contained deployable directory."
