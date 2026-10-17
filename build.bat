@echo off
setlocal ENABLEEXTENSIONS ENABLEDELAYEDEXPANSION

set VERSION=0.0.1
set APP=urnetwork
set APPNAME=urNetwork
set MAIN_GO=connectctl/main.go

echo =====================================
echo Building %APPNAME% (%VERSION%)
echo =====================================

mkdir ..\build\windows 2>nul
mkdir ..\build\linux 2>nul
mkdir ..\build\android 2>nul

:: =====================
:: LINUX AMD64
:: =====================
echo [1/3] Linux amd64
set GOOS=linux
set GOARCH=amd64
set CGO_ENABLED=0

go build -trimpath -ldflags "-X main.Version=%VERSION% -s -w" -o ..\build\linux\%APP% %MAIN_GO%
if errorlevel 1 goto :error

:: =====================
:: ANDROID ARM64
:: =====================
echo [2/3] Android arm64
set GOOS=android
set GOARCH=arm64
set CGO_ENABLED=0

go build -trimpath -ldflags "-X main.Version=%VERSION% -s -w" -o ..\build\android\%APP% %MAIN_GO%
if errorlevel 1 goto :error

:: =====================
:: WINDOWS AMD64 (LAST)
:: =====================
echo [3/3] Windows amd64
set GOOS=windows
set GOARCH=amd64
set CGO_ENABLED=0

go build -trimpath -ldflags "-X main.Version=%VERSION% -s -w" -o ..\build\windows\%APP%.exe %MAIN_GO%
if errorlevel 1 goto :error

:: =====================
:: CLEAN ENV (optional tapi cakep)
:: =====================
set GOOS=
set GOARCH=
set CGO_ENABLED=

echo.
echo [OK] Build selesai
goto :eof

:error
echo.
echo [NOTOK] Build gagal
exit /b 1
