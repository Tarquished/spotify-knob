# Builds both binaries into .\bin
#   spotify-knob.exe  - console build, use this for -auth and for debugging
#   spotify-knobw.exe - no console window, use this for autostart
$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
Push-Location $root
try {
    New-Item -ItemType Directory -Force -Path "$root\bin" | Out-Null
    Write-Host "building spotify-knob.exe (console)..."
    go build -trimpath -ldflags "-s -w" -o "$root\bin\spotify-knob.exe" ./cmd/spotify-knob
    Write-Host "building spotify-knobw.exe (windowless)..."
    go build -trimpath -ldflags "-s -w -H=windowsgui" -o "$root\bin\spotify-knobw.exe" ./cmd/spotify-knob
    Get-ChildItem "$root\bin" | Format-Table Name, Length, LastWriteTime
}
finally {
    Pop-Location
}
