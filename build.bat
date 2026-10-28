@echo off
rem ============================================================
rem  iKF Audio Center - portable package rebuild
rem ------------------------------------------------------------
rem  Usage:
rem    build.bat                                full rebuild -> portable package
rem    build.bat "D:\path\to\SoftwareDriver"    custom driver source folder
rem
rem  Output: release\iKF-Audio-Center-Portable  (the one true deliverable)
rem
rem  Notes:
rem    * Close iKF-Audio-Center.exe before running: step 4 wipes and
rem      rebuilds release\iKF-Audio-Center-Portable.
rem    * Step 4 auto-stashes your own scene files and puts them back.
rem    * Needs: python / go / wails on PATH; cargo is optional (missing
rem      cargo = exe degrades to the go audio path).
rem    * Preview pages are NOT built here any more; run
rem      tools\build_preview.py manually if you need them.
rem    * Every step is appended to build.log next to this file, so a
rem      vanished window still leaves a trace of where it stopped.
rem
rem  ------------------------------------------------------------
rem  !! BATCH GOTCHA - DO NOT REGRESS (2026-10-22)
rem  (keep this file pure 7-bit ASCII + CRLF; a non-ASCII byte or a bare
rem   LF line ending makes cmd mis-parse it and the window dies silently)
rem  A variable whose VALUE contains parentheses must never be echoed
rem  inside a parenthesised if-block. Our driver path contains "(WHQL)"
rem  so %SRC% is exactly such a variable. All echo %SRC% below live on
rem  plain top-level lines, never inside a ( ) block.
rem  ------------------------------------------------------------
setlocal enabledelayedexpansion
chcp 65001 >nul
cd /d "%~dp0"
title iKF Audio Center - portable rebuild

set "ROOT=%~dp0"
if "%ROOT:~-1%"=="\" set "ROOT=%ROOT:~0,-1%"
set "APP=%ROOT%\iKF-Audio-Center-Wails"
set "OUT=%ROOT%\release\iKF-Audio-Center-Portable"
set "LOG=%ROOT%\build.log"
set "EXE=%APP%\build\bin\iKF-Audio-Center.exe"
set "PEXE=%OUT%\iKF-Audio-Center.exe"
set "DLL=%APP%\audiocore\target\release\ikf_audio.dll"

set "SRC=%~1"
if not defined SRC set "SRC=%ROOT%\..\iKF headset\WIN10(WHQL)\SoftwareDriver"

>"%LOG%" echo [%DATE% %TIME%] rebuild start

echo ============================================================
echo  iKF Audio Center - portable rebuild
echo ============================================================
echo   root : %ROOT%
echo   src  : %SRC%
echo.

rem ---------- toolchain ----------
set "PY="
where python >nul 2>nul && set "PY=python"
if defined PY (
    %PY% -c "import sys" >nul 2>nul
    if errorlevel 1 set "PY="
)
if not defined PY goto :no_python
where go >nul 2>nul || goto :no_go
where wails >nul 2>nul || goto :no_wails
echo [ok] toolchain: python / go / wails
echo.

rem ---------- driver source (step 4) ----------
rem checked up front on purpose: fail in 2s instead of after 3 steps
if exist "%SRC%" goto :src_ok
echo [FAIL] driver source folder not found:
echo        %SRC%
echo.
echo        Either pass it explicitly:
echo          build.bat "D:\path\to\SoftwareDriver"
echo        or put it at the default place above.
goto :fail
:src_ok

rem ---------- 1. design -> Python app ----------
echo [1/4] build_center.py  -^> iKF-Audio-Center.py
>>"%LOG%" echo [1/4] build_center.py
%PY% "%ROOT%\tools\build_center.py"
if errorlevel 1 goto :fail
if not exist "%ROOT%\iKF-Audio-Center.py" (
    echo [FAIL] build_center.py did not produce iKF-Audio-Center.py
    goto :fail
)
echo.

rem ---------- 2. design + bridge -> frontend dist ----------
echo [2/4] build_frontend.py  -^> frontend\dist\index.html
>>"%LOG%" echo [2/4] build_frontend.py
%PY% "%APP%\tools\build_frontend.py"
if errorlevel 1 goto :fail
if not exist "%APP%\frontend\dist\index.html" (
    echo [FAIL] build_frontend.py did not produce frontend\dist\index.html
    goto :fail
)
echo.

rem ---------- 2b. rust audiocore -> ikf_audio.dll ----------
rem P1: audio hot path lives in audiocore (rust cdylib). Missing cargo or a
rem failed build is NOT fatal: the exe degrades to the go path at runtime.
where cargo >nul 2>nul
if errorlevel 1 (
    echo [2b/4] cargo not on PATH - skipping rust build, exe will degrade
    >>"%LOG%" echo [2b/4] cargo missing - skipped
    goto :rust_done
)
echo [2b/4] cargo build --release  -^> audiocore\target\release\ikf_audio.dll
>>"%LOG%" echo [2b/4] cargo build audiocore
pushd "%APP%\audiocore"
cargo build --release
if errorlevel 1 (
    popd
    echo [2b/4] WARN rust build failed - exe will degrade to go path
    >>"%LOG%" echo [2b/4] rust build failed
    goto :rust_done
)
popd
if not exist "%DLL%" (
    echo [2b/4] WARN dll not produced - exe will degrade to go path
    goto :rust_done
)
echo [2b/4] ok
:rust_done
echo.

rem ---------- 3. wails build -> exe ----------
echo [3/4] build.py  -^> build\bin\iKF-Audio-Center.exe
>>"%LOG%" echo [3/4] build.py
%PY% "%APP%\tools\build.py"
if errorlevel 1 goto :fail
if not exist "%EXE%" (
    echo [FAIL] wails build did not produce the exe
    goto :fail
)
if exist "%DLL%" (
    copy /y "%DLL%" "%APP%\build\bin\ikf_audio.dll" >nul
    >>"%LOG%" echo [3/4] ikf_audio.dll copied to build/bin
)
echo.

rem ---------- 4. portable package ----------
echo [4/4] build_portable.py  -^> release\iKF-Audio-Center-Portable
>>"%LOG%" echo [4/4] build_portable.py
%PY% "%ROOT%\tools\build_portable.py" --src "%SRC%"
if errorlevel 1 goto :portable_fail

:summary
echo.
echo ============================================================
echo  Done.
echo ============================================================
if exist "%EXE%" for %%f in ("%EXE%") do echo   exe     : %%~zf bytes
if exist "%PEXE%" (
    for %%f in ("%PEXE%") do echo   portable: %%~zf bytes
    set /a CNT=0
    for /r "%OUT%" %%f in (*) do set /a CNT+=1
    echo   files   : !CNT!
)
echo.
>>"%LOG%" echo DONE
pause
exit /b 0

rem ============================================================
rem  failure paths - every one of them ends at :fail (pause shown)
rem ============================================================
:no_python
echo [FAIL] python not found on PATH.
goto :fail

:no_go
echo [FAIL] go not found on PATH.
goto :fail

:no_wails
echo [FAIL] wails not found on PATH.
goto :fail

:portable_fail
echo.
echo [FAILED] build_portable.py returned an error.
echo          If it failed while deleting the release folder, close
echo          iKF-Audio-Center.exe / iKF driver manager first, then retry.
goto :fail

:fail
>>"%LOG%" echo FAILED
echo.
echo [FAILED] rebuild stopped. See the message above.
echo          Log: %LOG%
echo.
pause
exit /b 1
