@echo off
REM mcp-pg-readonly-run-vps.cmd -- shared-VPS secret wrapper for mcp-pg-readonly.
REM
REM Variant of mcp-pg-readonly-run.cmd for the deployment where ALL analysts
REM work on one Windows VPS under their own accounts:
REM   - the BINARY is shared: %PUBLIC%\bin\mcp-pg-readonly.exe (readable by
REM     every local account). Override the exe path with MCP_PG_BIN.
REM   - the SECRETS stay per-user: %USERPROFILE%\.config\mcp-pg-readonly\env.cmd
REM     plus anonymize.yaml in the same directory (both ACL-locked to that
REM     user only). NEVER put the password into %PUBLIC% -- it is world-readable
REM     on the machine; per-user .config with icacls is the secure pattern.
REM
REM The secrets file MUST use `set NAME=value` lines (cmd syntax), NOT
REM `export NAME=value` (sh syntax). For the arm_ai_ro setup the file is:
REM     set PG_USER_PWD=<arm_ai_ro password>
REM and the config.json entry passes --user arm_ai_ro --password-env PG_USER_PWD.
REM
REM Override the secrets path with MCP_PG_SECRETS (default:
REM E:\ARM_AI\.config\mcp-pg-readonly\env.cmd - the shared, AD-group-ACL'd
REM location on this VPS).
REM
REM No setlocal/endlocal here (same reasoning as mcp-pg-readonly-run.cmd):
REM env from `call "%SECRETS%"` must stay live for the exe to see at RUNTIME.

if defined MCP_PG_SECRETS (
    set "SECRETS=%MCP_PG_SECRETS%"
) else (
    set "SECRETS=E:\ARM_AI\.config\mcp-pg-readonly\env.cmd"
)

if exist "%SECRETS%" (
    call "%SECRETS%"
) else (
    1>&2 echo mcp-pg-readonly-run: warning: secrets file not found at "%SECRETS%" - set PG_USER_PWD another way or create it
)

REM Deployment endpoint defaults (NOT secrets). Applied AFTER the secrets
REM file so an operator's env.cmd or the ZCode env block can still override.
REM Without these, manual console runs would fall back to the binary's
REM compiled-in dev defaults (a different host).
if not defined PGHOST set "PGHOST=10.120.24.155"
if not defined PGPORT set "PGPORT=5432"

if defined MCP_PG_BIN (
    set "BIN=%MCP_PG_BIN%"
) else (
    set "BIN=%PUBLIC%\bin\mcp-pg-readonly.exe"
)

"%BIN%" %*
