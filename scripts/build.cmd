@echo off
setlocal enabledelayedexpansion
rem ---------------------------------------------------------------------------
rem  Build herdr-expose on Windows: the React app first, then the Go binary with
rem  that app embedded in it. The Windows twin of scripts/build.sh.
rem
rem  WHY A .cmd AND NOT A .ps1. Herdr runs a plugin's [[build]] command with
rem  CreateProcess, which cannot execute a .sh -- no shebang handling, and a
rem  default Git for Windows puts bash.exe in Git\bin, which is not on PATH. So
rem  Windows needs its own script. PowerShell would work, but only when invoked
rem  as `powershell -ExecutionPolicy Bypass -File ...`, and "bypass the execution
rem  policy" is precisely the pattern endpoint security software blocks or flags.
rem  cmd.exe is always present, needs no policy argument, and is boring. There
rem  is no PowerShell here at all, not even for a timestamp.
rem
rem  Without this file a Windows `herdr plugin install` SUCCEEDS and builds
rem  nothing: Herdr prints "build (skipped on windows)", installs the plugin,
rem  enables it, and leaves every action pointing at a .\bin\herdr-expose.exe
rem  that does not exist. That was measured, not imagined.
rem
rem  Source only. There is deliberately no release-asset download path here:
rem  the project publishes no Windows release asset yet, so a fallback would be
rem  untested code pretending to be a safety net.
rem
rem    scripts\build.cmd              build from source -> bin\herdr-expose.exe
rem    scripts\build.cmd --skip-web   Go only (web\dist must already exist)
rem    scripts\build.cmd --no-link    build only; install no agent skill
rem ---------------------------------------------------------------------------

rem Resolve the checkout BEFORE parsing arguments. `shift` in the loop below
rem rewrites %0 -- after one shift %~dp0 is no longer this script, it is the
rem current directory -- so reading it later silently yields the PARENT of the
rem checkout. Measured: with no arguments the build worked, and with any flag
rem (`--no-link`, `--skip-web`) REPO_ROOT came out one level too high and the
rem build died with "no web\ directory at <parent>\web". The manifest passes
rem --no-link, so this was load-bearing.
rem %~dp0 ends with a backslash; strip it, then take the parent.
set "SCRIPT_DIR=%~dp0"
set "SCRIPT_DIR=%SCRIPT_DIR:~0,-1%"
for %%I in ("%SCRIPT_DIR%\..") do set "REPO_ROOT=%%~fI"

set "SKIP_WEB="
set "NO_LINK="
:parse
if "%~1"=="" goto parsed
if /i "%~1"=="--skip-web" set "SKIP_WEB=1" & shift & goto parse
if /i "%~1"=="--no-link"  set "NO_LINK=1"  & shift & goto parse
echo error: unknown option %~1 1>&2
exit /b 2
:parsed
if "%HERDR_EXPOSE_NO_LINK%"=="1" set "NO_LINK=1"

cd /d "%REPO_ROOT%"
if errorlevel 1 goto :err_nocd

set "BIN_NAME=herdr-expose"
set "PKG=./cmd/herdr-expose"

rem --- toolchain -------------------------------------------------------------
rem `where` is the only dependency check that matters: what is on PATH, not what
rem exists on disk. A tool installed but unreachable is a tool we do not have.
where go >nul 2>&1
if errorlevel 1 goto :err_nogo
if defined SKIP_WEB goto :toolchain_done
if exist "%REPO_ROOT%\web\dist\index.html" goto :toolchain_done
where npm >nul 2>&1
if errorlevel 1 goto :err_nonpm
:toolchain_done

rem --- version stamp ---------------------------------------------------------
rem Judged by exit code, never by output: git writes to stderr while succeeding,
rem and `git describe` in a checkout with no tags fails outright. Herdr's own
rem clone carries no tags, so "dev" is the honest answer there, not an error.
if not defined VERSION (
  set "VERSION=dev"
  for /f "usebackq delims=" %%V in (`git describe --tags --always --dirty 2^>nul`) do set "VERSION=%%V"
)
rem No COMMIT or BUILD_DATE stamp: `main.commit` and `main.buildDate` do not
rem exist. The linker silently ignores -X for a symbol that is not there, so both
rem values were computed and discarded. Removing them also removes the only
rem reason this script had to shell out to PowerShell at all -- cmd.exe has no
rem locale-independent UTC clock, and a field nothing reads is not worth one.

rem --- web -------------------------------------------------------------------
rem
rem EVERY failure below jumps to a label at the BOTTOM of this file instead of
rem calling `exit /b 1` where it happened. That is not style, it is the only
rem thing that works: `exit /b N` inside a parenthesised block that is nested in
rem an if/else PRINTS the error and then returns 0. Measured twice on a real
rem box -- a deliberately failing `npm run build` and a failing `npm ci` both
rem reported their error and exited SUCCESS, with and without the &-chaining
rem that was my first suspect. A build script that exits 0 having built nothing
rem is the precise bug this file exists to prevent, so the error paths are
rem tested, not assumed.
if defined SKIP_WEB goto :skipweb

echo ==^> building web app ^(npm ci; npm run build^)
pushd "%REPO_ROOT%\web"
if errorlevel 1 goto :err_noweb
rem `call` is required: npm is npm.cmd, and without it control never returns.
call npm ci
if errorlevel 1 goto :err_npmci
call npm run build
if errorlevel 1 goto :err_npmbuild
popd
if not exist "%REPO_ROOT%\web\dist\index.html" goto :err_nodist
goto :web_done

:skipweb
echo ==^> skipping web build ^(--skip-web^)
if not exist "%REPO_ROOT%\web\dist\index.html" goto :err_needdist

:web_done

rem --- go --------------------------------------------------------------------
echo ==^> building bin\%BIN_NAME%.exe, version %VERSION%
if not exist "%REPO_ROOT%\bin" mkdir "%REPO_ROOT%\bin"
set "CGO_ENABLED=0"
rem Delete any previous binary first: without this a FAILING build leaves the
rem stale one in place and the "was a binary produced?" guard passes on last
rem build's output.
if exist "%REPO_ROOT%\bin\%BIN_NAME%.exe" del /q "%REPO_ROOT%\bin\%BIN_NAME%.exe"
go build -trimpath -ldflags "-s -w -X main.version=%VERSION%" -o "%REPO_ROOT%\bin\%BIN_NAME%.exe" %PKG%
if errorlevel 1 goto :err_gobuild
if not exist "%REPO_ROOT%\bin\%BIN_NAME%.exe" goto :err_nobin
echo ==^> built bin\%BIN_NAME%.exe

rem --- skill -----------------------------------------------------------------
rem There is no ~/.local/bin convention on Windows and no privilege to link into
rem one, so say where the binary is rather than pretend to put it on PATH.
echo ==^> add %REPO_ROOT%\bin to your PATH, or call the binary directly
if defined NO_LINK goto :nolink
rem The skill half lives in the binary, so there is exactly one implementation of
rem "where does skill\ belong". On Windows it lands as a directory junction,
rem because os.Symlink needs a privilege an ordinary user does not have.
"%REPO_ROOT%\bin\%BIN_NAME%.exe" skill install
if errorlevel 1 echo warning: the agent skill was not linked. Fix that, then run: %BIN_NAME% skill install 1>&2
exit /b 0

:nolink
echo ==^> --no-link: no agent skill installed ^(later: %BIN_NAME% skill install^)
exit /b 0

rem --- error exits -----------------------------------------------------------
:err_noweb
echo error: no web\ directory at %REPO_ROOT%\web 1>&2
exit /b 1
:err_npmci
popd
echo error: npm ci failed 1>&2
exit /b 1
:err_npmbuild
popd
echo error: npm run build failed 1>&2
exit /b 1
:err_nodist
echo error: web build produced no web\dist\index.html 1>&2
exit /b 1
:err_needdist
echo error: web\dist\index.html missing; cannot --skip-web 1>&2
exit /b 1
:err_gobuild
echo error: go build failed 1>&2
exit /b 1
:err_nobin
echo error: no bin\%BIN_NAME%.exe was produced 1>&2
exit /b 1
:err_nogo
echo error: Go 1.25+ is not on PATH. Install it from https://go.dev/dl/ 1>&2
exit /b 1
:err_nonpm
echo error: node 20+ is not on PATH and web\dist is not built. 1>&2
echo        Install node from https://nodejs.org/ , or pass --skip-web. 1>&2
exit /b 1
:err_nocd
echo error: cannot enter %REPO_ROOT% 1>&2
exit /b 1
