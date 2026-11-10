@echo off
setlocal
if defined AC2_START_CONSOLE goto console_owner
chcp 65001 >nul
echo Archive Center 2 백엔드를 시작합니다. 이 창을 닫으면 백엔드가 꺼집니다.
set "AC2_SCRIPT_DIR=%~dp0"
pushd "%AC2_SCRIPT_DIR%.."
set "AC2_LOG_DATA=%AC2_DATA_DIR%"
if defined AC2_LOG_DATA goto data_selected
set "AC2_LOG_DATA=%LOCALAPPDATA%\ArchiveCenter2\data"
if defined LOCALAPPDATA goto data_selected
set "AC2_LOG_DATA=%USERPROFILE%\AppData\Local\ArchiveCenter2\data"
if not defined USERPROFILE set "AC2_LOG_DATA="
:data_selected
call :select_log_data %*
if not defined AC2_LOG_DATA goto log_fallback
for %%I in ("%AC2_LOG_DATA%") do set "AC2_LOG_DATA=%%~fI"
set "AC2_LOG_DIR=%AC2_LOG_DATA%\logs"
set "AC2_START_LOG=%AC2_LOG_DIR%\start-%RANDOM%-%RANDOM%.log"
if not exist "%AC2_LOG_DIR%" mkdir "%AC2_LOG_DIR%" 2>nul
(echo Archive Center 2 백엔드를 시작합니다. 이 창을 닫으면 백엔드가 꺼집니다.) >"%AC2_START_LOG%" 2>nul
if exist "%AC2_START_LOG%" goto log_ready
:log_fallback
set "AC2_LOG_DIR=%TEMP%\ArchiveCenter2-start-logs"
if not exist "%AC2_LOG_DIR%" mkdir "%AC2_LOG_DIR%" 2>nul
set "AC2_START_LOG=%AC2_LOG_DIR%\start-%RANDOM%-%RANDOM%.log"
(echo Archive Center 2 백엔드를 시작합니다. 이 창을 닫으면 백엔드가 꺼집니다.) >"%AC2_START_LOG%" 2>nul
if not exist "%AC2_START_LOG%" set "AC2_START_LOG="
:log_ready
echo [시작] PowerShell 시작
if defined AC2_START_LOG (echo [시작] PowerShell 시작)>>"%AC2_START_LOG%"
rem START /B gives this waiting CMD an ignored Ctrl+C disposition. PowerShell
rem re-enables Ctrl+C for itself and Go; CMD can then always do the final pause
rem without a separate batch-cancellation question.
set "AC2_START_CONSOLE=1"
start "" /b "%ComSpec%" /d /c call "%~f0" %*
set "AC2_EXIT=%ERRORLEVEL%"
if "%AC2_EXIT%"=="0" exit /b 0
goto launch_failed
:console_owner
"%SystemRoot%\System32\WindowsPowerShell\v1.0\powershell.exe" -NoProfile -ExecutionPolicy Bypass -File "%AC2_SCRIPT_DIR%start.ps1" %*
set "AC2_EXIT=%ERRORLEVEL%"
if "%AC2_EXIT%"=="0" goto finished
:launch_failed
echo 오류: 시작 명령에서 멈췄습니다. 종료 코드: %AC2_EXIT%
echo PowerShell 실행 파일: %SystemRoot%\System32\WindowsPowerShell\v1.0\powershell.exe
echo 시작 스크립트: %AC2_SCRIPT_DIR%start.ps1
echo PowerShell 또는 실행 파일을 열 수 없으면 압축 해제 상태와 백신 차단·격리를 확인해 주세요.
if defined AC2_START_LOG (echo 오류: 시작 명령에서 멈췄습니다. 종료 코드: %AC2_EXIT%)>>"%AC2_START_LOG%"
if defined AC2_START_LOG (echo PowerShell 실행 파일: %SystemRoot%\System32\WindowsPowerShell\v1.0\powershell.exe)>>"%AC2_START_LOG%"
if defined AC2_START_LOG (echo 시작 스크립트: %AC2_SCRIPT_DIR%start.ps1)>>"%AC2_START_LOG%"
if defined AC2_START_LOG (echo PowerShell 또는 실행 파일을 열 수 없으면 압축 해제 상태와 백신 차단·격리를 확인해 주세요.)>>"%AC2_START_LOG%"
if defined AC2_START_LOG echo 시작 로그: %AC2_START_LOG%
if defined AC2_START_LOG (echo 시작 로그: %AC2_START_LOG%)>>"%AC2_START_LOG%"
:finished
echo 창을 닫으려면 아무 키나 누르세요.
if defined AC2_START_LOG (echo 창을 닫으려면 아무 키나 누르세요.)>>"%AC2_START_LOG%"
pause >nul
popd
exit /b %AC2_EXIT%

:select_log_data
if "%~1"=="" exit /b
set "AC2_LOG_ARGUMENT=%~1"
if "%AC2_LOG_ARGUMENT%"=="-data" goto next_log_data
if "%AC2_LOG_ARGUMENT%"=="--data" goto next_log_data
if "%AC2_LOG_ARGUMENT:~0,6%"=="-data=" set "AC2_LOG_DATA=%AC2_LOG_ARGUMENT:~6%"
if "%AC2_LOG_ARGUMENT:~0,7%"=="--data=" set "AC2_LOG_DATA=%AC2_LOG_ARGUMENT:~7%"
shift
goto select_log_data
:next_log_data
set "AC2_LOG_DATA=%~2"
shift
shift
goto select_log_data
