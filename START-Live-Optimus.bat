@echo off
REM ============================================================
REM  Live-Optimus one-click launcher
REM  Double-click to start the backend + frontend and open the UI.
REM  Close the two terminal windows that appear to stop it.
REM ============================================================
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0scripts\launch.ps1"
