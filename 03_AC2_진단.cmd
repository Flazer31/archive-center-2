@echo off
setlocal
chcp 65001 >nul
"%~dp0bin\ac2-updater.exe" diagnostics --root "%~dp0." %*
set "AC2_EXIT=%ERRORLEVEL%"
if not "%AC2_EXIT%"=="0" echo 진단 보고서를 저장하지 못했습니다. 위의 오류를 확인해 주세요.
pause
exit /b %AC2_EXIT%
