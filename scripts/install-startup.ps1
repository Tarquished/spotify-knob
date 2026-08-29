# Registers spotify-knob to start at logon, windowless, via Task Scheduler.
#
# Task Scheduler is used instead of the Startup folder because it can run the
# daemon elevated without a UAC prompt. That matters: a low-level keyboard hook
# installed by a normal process does not receive keys while an elevated window
# has focus (some games and tools run as admin), so the knob would silently
# stop working there.
#
# Running this WITHOUT admin still works - the task is just registered at
# normal privilege, and the knob will be dead inside elevated windows only.
# Re-run from an elevated PowerShell to upgrade it.
$ErrorActionPreference = "Stop"

$root     = Split-Path -Parent $PSScriptRoot
$exe      = Join-Path $root "bin\spotify-knobw.exe"
$taskName = "spotify-knob"

if (-not (Test-Path $exe)) {
    throw "Not built yet. Run scripts\build.ps1 first (expected $exe)."
}

$identity  = [Security.Principal.WindowsIdentity]::GetCurrent()
$elevated  = (New-Object Security.Principal.WindowsPrincipal($identity)).IsInRole(
                [Security.Principal.WindowsBuiltInRole]::Administrator)
$runLevel  = if ($elevated) { "Highest" } else { "Limited" }
$principalId = "$env:USERDOMAIN\$env:USERNAME"

# Stop whatever is already running so the new binary can take over port 8888.
# A daemon started from an elevated console cannot be stopped from here; say so
# instead of failing, and skip the auto-start so we do not fight over the port.
# foreach (not ForEach-Object) so $blocked is set in this scope, and $p stays
# the process even inside catch, where $_ is the error record.
$blocked = $false
foreach ($p in @(Get-Process -Name "spotify-knobw", "spotify-knob" -ErrorAction SilentlyContinue)) {
    try {
        Stop-Process -Id $p.Id -Force -ErrorAction Stop
        Write-Host "Stopped old daemon (PID $($p.Id), $($p.ProcessName))."
    } catch {
        Write-Host "Could not stop PID $($p.Id) ($($p.ProcessName)) - it is running elevated."
        $blocked = $true
    }
}

$action    = New-ScheduledTaskAction -Execute $exe
$trigger   = New-ScheduledTaskTrigger -AtLogOn -User $principalId
$principal = New-ScheduledTaskPrincipal -UserId $principalId -LogonType Interactive -RunLevel $runLevel
$settings  = New-ScheduledTaskSettingsSet `
    -AllowStartIfOnBatteries `
    -DontStopIfGoingOnBatteries `
    -DontStopOnIdleEnd `
    -ExecutionTimeLimit ([TimeSpan]::Zero) `
    -RestartCount 3 `
    -RestartInterval (New-TimeSpan -Minutes 1)

# Give the desktop a moment to settle before the hook goes in.
$trigger.Delay = "PT10S"

Register-ScheduledTask -TaskName $taskName -Action $action -Trigger $trigger `
    -Principal $principal -Settings $settings -Force | Out-Null

Write-Host "Registered scheduled task '$taskName' (RunLevel: $runLevel) -> $exe"
if (-not $elevated) {
    Write-Host "Note: not elevated, so the hook will not see keys while an admin window has focus."
    Write-Host "      Re-run this from an elevated PowerShell to fix that."
}

if ($blocked) {
    Write-Host ""
    Write-Host "An old daemon still holds port 8888, so the task was NOT started."
    Write-Host "Close that console window (Ctrl+C in it), then run:"
    Write-Host "    Start-ScheduledTask -TaskName $taskName"
    exit 0
}

Write-Host "Starting it now..."
Start-ScheduledTask -TaskName $taskName
Start-Sleep -Seconds 3
Get-ScheduledTask -TaskName $taskName | Select-Object TaskName, State | Format-Table
Get-Process -Name "spotify-knobw" -ErrorAction SilentlyContinue |
    Select-Object Id, ProcessName, StartTime | Format-Table
Write-Host "Check it with:  .\bin\spotify-knob.exe -status"
