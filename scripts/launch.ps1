# Live-Optimus launcher. Starts backend + frontend in their own windows, waits for
# the UI, then opens the browser. Called by START-Live-Optimus.bat.
$ErrorActionPreference = "Stop"

$root     = Split-Path -Parent $PSScriptRoot
$backend  = Join-Path $root "backend"
$frontend = Join-Path $root "frontend"
$goBin    = "C:\Program Files\Go\bin"

# Make `go` available to the child cmd windows (they inherit this process's PATH).
if (Test-Path (Join-Path $goBin "go.exe")) {
    $env:PATH = "$env:PATH;$goBin"
}

Write-Host "Starting Live-Optimus..." -ForegroundColor Cyan

# Install frontend deps here (first run only) — doing it in PowerShell avoids cmd's
# fragile `if`/`&&` parsing.
if (-not (Test-Path (Join-Path $frontend "node_modules"))) {
    Write-Host "Installing frontend dependencies (first run, ~30s)..." -ForegroundColor Cyan
    Push-Location $frontend
    npm install
    Pop-Location
}

# Backend window.
Start-Process -FilePath "cmd.exe" `
    -ArgumentList '/k', 'title Live-Optimus Backend && go run ./cmd/server' `
    -WorkingDirectory $backend

# Frontend window.
Start-Process -FilePath "cmd.exe" `
    -ArgumentList '/k', 'title Live-Optimus Frontend && npm run dev' `
    -WorkingDirectory $frontend

# Wait (up to ~60s) for the Vite dev server, then open the browser.
Write-Host "Waiting for the UI to come up..." -ForegroundColor Cyan
$ready = $false
for ($i = 0; $i -lt 80; $i++) {
    try {
        $r = Invoke-WebRequest -Uri 'http://localhost:5173' -UseBasicParsing -TimeoutSec 1
        if ($r.StatusCode -eq 200) { $ready = $true; break }
    } catch { }
    Start-Sleep -Milliseconds 750
}

if ($ready) {
    Start-Process 'http://localhost:5173'
    Write-Host "Live-Optimus is up. Browser opening..." -ForegroundColor Green
} else {
    Write-Host "UI did not respond yet. Check the 'Live-Optimus Frontend' window for errors," -ForegroundColor Yellow
    Write-Host "then open http://localhost:5173 manually." -ForegroundColor Yellow
    Start-Process 'http://localhost:5173'
}

Write-Host "Close the Backend and Frontend windows to stop Live-Optimus."
Start-Sleep -Seconds 2
