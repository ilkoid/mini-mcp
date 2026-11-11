@echo off
REM mcp-mssql-readonly-run.cmd -- shared-VPS secret wrapper for
REM mcp-mssql-readonly (the SQL Server sibling of mcp-pg-readonly-run-vps.cmd).
REM
REM   - the BINARY is shared: %PUBLIC%\bin\mcp-mssql-readonly.exe (readable
REM     by every local account). Override the exe path with MCP_MSSQL_BIN.
REM   - the SECRETS stay in the AD-group-ACL'd location:
REM     E:\ARM_AI\.config\mcp-mssql-readonly\env.cmd (default). NEVER put the
REM     password into %PUBLIC% -- it is world-readable on the machine.
REM
REM The secrets file MUST use `set NAME=value` lines (cmd syntax), NO quotes,
REM NOT `export NAME=value` (sh syntax). Its content is:
REM     set MSSQL_AI_PWD=<ai_reader password>
REM (placeholder only in this repo's docs -- the real value lives only in the
REM ACL-locked env.cmd).
REM
REM Override the secrets path with MCP_MSSQL_SECRETS.
REM
REM No setlocal/endlocal here (same reasoning as the pg wrapper): env from
REM `call "%SECRETS%"` must stay live for the exe to see at RUNTIME. A local
REM scope would roll the vars back before the binary reads them -- the
REM classic cmd footgun behind "empty password" errors.

if defined MCP_MSSQL_SECRETS (
    set "SECRETS=%MCP_MSSQL_SECRETS%"
) else (
    set "SECRETS=E:\ARM_AI\.config\mcp-mssql-readonly\env.cmd"
)

if exist "%SECRETS%" (
    call "%SECRETS%"
) else (
    1>&2 echo mcp-mssql-readonly-run: warning: secrets file not found at "%SECRETS%" - set MSSQL_AI_PWD another way or create it
)

REM Deployment endpoint defaults (NOT secrets). Applied AFTER the secrets
REM file so an operator's env.cmd or the ZCode env block can still override.
if not defined MSSQL_HOST set "MSSQL_HOST=10.120.11.233"
if not defined MSSQL_PORT set "MSSQL_PORT=1433"

if defined MCP_MSSQL_BIN (
    set "BIN=%MCP_MSSQL_BIN%"
) else (
    set "BIN=%PUBLIC%\bin\mcp-mssql-readonly.exe"
)

"%BIN%" %*
