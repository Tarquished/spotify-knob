# Removes the autostart task and stops a running daemon.
# Run from an ELEVATED PowerShell.
$ErrorActionPreference = "Stop"
$taskName = "spotify-knob"

try {
    Stop-ScheduledTask -TaskName $taskName -ErrorAction Stop
} catch {
    Write-Host "Task was not running."
}

try {
    Unregister-ScheduledTask -TaskName $taskName -Confirm:$false -ErrorAction Stop
    Write-Host "Removed scheduled task '$taskName'."
} catch {
    Write-Host "No scheduled task named '$taskName'."
}

Get-Process -Name "spotify-knobw", "spotify-knob" -ErrorAction SilentlyContinue |
    ForEach-Object { Stop-Process -Id $_.Id -Force; Write-Host "Stopped PID $($_.Id)." }
