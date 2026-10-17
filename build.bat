
@echo off
setlocal EnableExtensions

set "VERSION=0.0.1"
set "APP=urnetwork"
set "APPNAME=urNetwork"
set "MAIN_GO=./connectctl"

:: Jalankan dari lokasi build.bat
cd /d "%~dp0"

echo =====================================
echo Building %APPNAME% (%VERSION%)
echo =====================================

if not exist "build\windows" mkdir "build\windows"
if not exist "build\linux" mkdir "build\linux"
if not exist "build\android" mkdir "build\android"

:: =====================
:: LINUX AMD64
:: =====================
echo [1/3] Linux amd64
set "GOOS=linux"
set "GOARCH=amd64"
set "CGO_ENABLED=0"

go build -trimpath -ldflags "-s -w" -o "build\linux\%APP%" %MAIN_GO%
if errorlevel 1 goto :error

:: =====================
:: ANDROID ARM64
:: =====================
echo [2/3] Android arm64
set "GOOS=android"
set "GOARCH=arm64"
set "CGO_ENABLED=0"

go build -trimpath -ldflags "-s -w" -o "build\android\%APP%" %MAIN_GO%
if errorlevel 1 goto :error

:: =====================
:: WINDOWS AMD64
:: =====================
echo [3/3] Windows amd64
set "GOOS=windows"
set "GOARCH=amd64"
set "CGO_ENABLED=0"

go build -trimpath -ldflags "-s -w" -o "build\windows\%APP%.exe" %MAIN_GO%
if errorlevel 1 goto :error

echo.
echo =====================================
echo [OK] Semua build berhasil!
echo =====================================

endlocal
exit /b 0

:error
echo.
echo =====================================
echo [NOTOK] Build gagal!
echo =====================================

endlocal
exit /b 1
