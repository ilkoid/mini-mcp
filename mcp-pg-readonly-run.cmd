@echo off
REM mcp-pg-readonly-run.cmd -- Windows secret wrapper for mcp-pg-readonly.
REM
REM Mirrors mcp-pg-readonly-run.sh: reads secrets from a .env-style file
REM (lines like `set PG_RO_PWD=...`) so the password never has to live in
REM ~/.zcode/cli/config.json, then execs the .exe with all forwarded args.
REM Used as the MCP `command` from ZCode on Windows.
REM
REM Override the secrets path with MCP_PG_SECRETS (default:
REM %USERPROFILE%\.config\mcp-pg-readonly\env.cmd).
REM
REM The secrets file MUST use `set NAME=value` lines (cmd syntax), NOT
REM `export NAME=value` (sh syntax). The companion file on Unix uses `export`.
REM Keep this file CRLF (see .gitattributes); cmd is picky about line endings.

setlocal enableextensions

if defined MCP_PG_SECRETS (
    set "SECRETS=%MCP_PG_SECRETS%"
) else (
    set "SECRETS=%USERPROFILE%\.config\mcp-pg-readonly\env.cmd"
)

if exist "%SECRETS%" (
    call "%SECRETS%"
) else (
    1>&2 echo mcp-pg-readonly-run: warning: secrets file not found at "%SECRETS%" (set PG_RO_PWD another way or create it)
)

REM env. must be enabled so PG_RO_PWD / PGHOST / etc. reach the .exe.
endlocal & "%USERPROFILE%\bin\mcp-pg-readonly.exe" %*
