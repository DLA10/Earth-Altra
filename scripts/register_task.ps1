$TaskName = "Optimus_Rolling_Retrain"
$RepoRoot = Split-Path -Parent $PSScriptRoot
$PythonExe = Join-Path $RepoRoot "ml\.venv\Scripts\python.exe"
$ScriptPath = Join-Path $RepoRoot "ml\rolling_retrain.py"

$Action = New-ScheduledTaskAction -Execute $PythonExe -Argument $ScriptPath
$Trigger = New-ScheduledTaskTrigger -Weekly -DaysOfWeek Saturday -At "02:00AM"
$Settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -StartWhenAvailable -RunOnlyIfNetworkAvailable

Register-ScheduledTask -Action $Action -Trigger $Trigger -Settings $Settings -TaskName $TaskName -Description "Runs the ML Rolling Retrain script every Saturday at 2:00 AM" -Force

Write-Host "Successfully registered Scheduled Task: $TaskName"
Write-Host "It will run $PythonExe $ScriptPath every Saturday at 2:00 AM."
