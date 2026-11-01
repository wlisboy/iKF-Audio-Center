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
rem    * Preview pages are NOT built here; run tools\build_preview.py.
rem    * Console stays terse (one line per step); full tool output goes
rem      to build.log, and is shown on the console only on failure.
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
set "SCRATCH=%ROOT%\build.step.log"
set "EXE=%APP%\build\bin\iKF-Audio-Center.exe"
set "DLL=%APP%\audiocore\target\release\ikf_audio.dll"

set "SRC=%~1"
if not defined SRC set "SRC=%ROOT%\..\iKF headset\WIN10(WHQL)\SoftwareDriver"

if exist "%LOG%" copy /y "%LOG%" "%LOG%.prev" >nul
>"%LOG%" echo [%DATE% %TIME%] rebuild start
>>"%LOG%" echo   src: %SRC%
if exist "%SCRATCH%" del "%SCRATCH%" >nul
set "T_START=%TIME%"
set "T_STEP=%TIME%"

echo iKF Audio Center - portable rebuild

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

rem ---------- driver source (step 4) ----------
rem checked up front on purpose: fail in 2s instead of after 3 steps
if exist "%SRC%" goto :src_ok
echo [FAIL] driver source not found:
echo   %SRC%
echo   pass it explicitly: build.bat "D:\path\to\SoftwareDriver"
goto :fail
:src_ok

rem ---------- 2b. rust audiocore -> ikf_audio.dll (launch only) ----------
rem P1: audio hot path lives in audiocore (rust cdylib). Missing cargo or a
rem failed build is NOT fatal: the exe degrades to the go path at runtime.
rem 2026-11-01: cargo does not depend on steps 1/2 and is the slowest link,
rem so it is LAUNCHED here in the background, steps 1/2 overlap with it, and
rem we join at :rust_collect right after step 2 (before wails build).
rem Quoting is verified - do not "simplify" the (call echo %%^errorlevel%%).
set "CARGO_FLAG=%ROOT%\build.cargo.done"
set "CARGO_LOG=%ROOT%\build.cargo.log"
if exist "%CARGO_FLAG%" del "%CARGO_FLAG%" >nul
where cargo >nul 2>nul
if errorlevel 1 (
    echo [2b/4] cargo not found - go audio path
    >>"%LOG%" echo [2b/4] cargo missing - skipped
    rem no flag -> the join step below skips waiting entirely
    set "CARGO_FLAG="
    goto :rust_started
)
>>"%LOG%" echo [2b/4] cargo build audiocore (background)
pushd "%APP%\audiocore"
start "rustbuild" /b cmd /c "cargo build --release > "%CARGO_LOG%" 2>&1 & (call echo %%^errorlevel%%)>"%CARGO_FLAG%""
popd
:rust_started

rem ---------- 1. design -> Python app ----------
set "STEP=[1/4] python app"
%PY% "%ROOT%\tools\build_center.py" >"%SCRATCH%" 2>&1
if errorlevel 1 goto :stepfail
>>"%LOG%" type "%SCRATCH%"
if not exist "%ROOT%\iKF-Audio-Center.py" goto :stepnoout
call :mark "build_center"
echo %STEP% ok

rem ---------- 2. design + bridge -> frontend dist ----------
set "STEP=[2/4] frontend"
%PY% "%APP%\tools\build_frontend.py" >"%SCRATCH%" 2>&1
if errorlevel 1 goto :stepfail
>>"%LOG%" type "%SCRATCH%"
if not exist "%APP%\frontend\dist\index.html" goto :stepnoout
call :mark "build_frontend"
echo %STEP% ok

rem ---------- 2c. join background cargo ----------
if not defined CARGO_FLAG goto :rust_done
if exist "%CARGO_FLAG%" goto :rust_collect
echo [2b/4] waiting for rust audiocore
:rust_wait
if exist "%CARGO_FLAG%" goto :rust_collect
rem 10 min watchdog: a hung cargo must not wedge the build forever
ping -n 2 127.0.0.1 >nul
set /a CARGO_WAIT+=1
if %CARGO_WAIT% lss 600 goto :rust_wait
echo [WARN] rust wait timeout - go audio path
>>"%LOG%" echo [2b/4] cargo wait timeout
goto :rust_done

:rust_collect
set /p CARGO_RC=<"%CARGO_FLAG%"
>>"%LOG%" type "%CARGO_LOG%"
if not "%CARGO_RC%"=="0" (
    type "%CARGO_LOG%"
    echo [WARN] rust build failed - go audio path
    >>"%LOG%" echo [2b/4] rust build failed
    goto :rust_done
)
call :mark "cargo(bg)"
if not exist "%DLL%" (
    echo [WARN] rust dll missing - go audio path
    goto :rust_done
)
:rust_done
if exist "%CARGO_FLAG%" del "%CARGO_FLAG%" >nul
if exist "%CARGO_LOG%" del "%CARGO_LOG%" >nul

rem ---------- 3. wails build -> exe ----------
set "STEP=[3/4] wails build"
%PY% "%APP%\tools\build.py" >"%SCRATCH%" 2>&1
if errorlevel 1 goto :stepfail
>>"%LOG%" type "%SCRATCH%"
if not exist "%EXE%" goto :stepnoout
if exist "%DLL%" (
    copy /y "%DLL%" "%APP%\build\bin\ikf_audio.dll" >nul
    >>"%LOG%" echo [3/4] ikf_audio.dll copied to build/bin
)
call :mark "wails_build"
echo %STEP% ok

rem ---------- 4. portable package ----------
set "STEP=[4/4] portable package"
%PY% "%ROOT%\tools\build_portable.py" --src "%SRC%" >"%SCRATCH%" 2>&1
if errorlevel 1 goto :stepfail
>>"%LOG%" type "%SCRATCH%"
call :mark "build_portable"
echo %STEP% ok

:summary
echo.
rem total wall time since build start, not since the last step
set "T_STEP=%T_START%"
call :mark "TOTAL"
set /a CNT=0
for /r "%OUT%" %%f in (*) do set /a CNT+=1
echo Build OK - %ELAPSED%s - !CNT! files
echo   %OUT%
if exist "%SCRATCH%" del "%SCRATCH%" >nul
>>"%LOG%" echo DONE
pause
exit /b 0

rem ============================================================
rem  failure paths - every one of them ends at :fail (pause shown)
rem ============================================================

rem ---------- step timing ----------
rem :mark <name>  - log seconds since the last :mark, reset the clock.
rem :elapsed <var> - sets <var> to seconds since the time in T_STEP.
rem %TIME% has centiseconds and may carry a leading space before 10:00,
rem so parse each field numerically and wrap at 24h.
:mark
call :elapsed ELAPSED T_STEP
>>"%LOG%" echo       + %ELAPSED%s  %~1
set "T_STEP=%TIME%"
exit /b 0

:elapsed
setlocal
rem %2 = name of the variable holding the start time
call set "TS=%%%2%%%"
set "TE=%TIME%"
rem normalize " 9:xx:xx.cc" -> "09:xx:xx.cc" so field slicing is stable
if "%TS:~0,1%"==" " set "TS=0%TS:~1%"
if "%TE:~0,1%"==" " set "TE=0%TE:~1%"
set /a S1=((1%TS:~0,2%-100)*3600)+((1%TS:~3,2%-100)*60)+(1%TS:~6,2%-100)
set /a S2=((1%TE:~0,2%-100)*3600)+((1%TE:~3,2%-100)*60)+(1%TE:~6,2%-100)
set /a D=S2-S1
if %D% lss 0 set /a D+=86400
endlocal & set "%~1=%D%"
exit /b 0

rem a build step returned an error: show its captured output
:stepfail
echo %STEP% failed
>>"%LOG%" type "%SCRATCH%"
type "%SCRATCH%"
goto :fail

rem a build step "succeeded" but its output file is missing
:stepnoout
echo %STEP% failed - expected output missing
goto :fail

:no_python
echo [FAIL] python not found on PATH.
goto :fail

:no_go
echo [FAIL] go not found on PATH.
goto :fail

:no_wails
echo [FAIL] wails not found on PATH.
goto :fail

:fail
if exist "%SCRATCH%" del "%SCRATCH%" >nul
>>"%LOG%" echo FAILED
echo.
echo BUILD FAILED - full log: %LOG%
pause
exit /b 1
