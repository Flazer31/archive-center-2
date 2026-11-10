# Production launcher/managed DB with isolated data, loopback and synthetic credentials only.
param([string]$BuildRoot = (Join-Path $PSScriptRoot '../.startup-test'), [string]$Output = '')
$ErrorActionPreference = 'Stop'
$BuildRoot = [IO.Path]::GetFullPath($BuildRoot)
$sourceRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$driver = Join-Path $PSScriptRoot 'startup-console-test.ps1'
$results = @()
function Assert-True($Value, [string]$Message) { if (-not $Value) { throw $Message } }
function Read-Log([string]$DataRoot) {
    $file = Get-ChildItem -LiteralPath (Join-Path $DataRoot 'logs') -Filter 'start-*.log' -ErrorAction SilentlyContinue | Sort-Object LastWriteTime -Descending | Select-Object -First 1
    if (-not $file) { return '' }
    $stream = [IO.FileStream]::new($file.FullName, [IO.FileMode]::Open, [IO.FileAccess]::Read, [IO.FileShare]::ReadWrite)
    $reader = [IO.StreamReader]::new($stream, [Text.Encoding]::UTF8)
    try { return $reader.ReadToEnd() } finally { $reader.Dispose() }
}
function Status([string]$Url, [string]$Token) {
    $request = [Net.HttpWebRequest]::Create($Url)
    $request.Timeout = 3000
    if ($Token) { $request.Headers['X-AC2-Token'] = $Token }
    try { $response = $request.GetResponse() } catch [Net.WebException] { $response = $_.Exception.Response; if (-not $response) { throw } }
    try { return [int]$response.StatusCode } finally { $response.Close() }
}
foreach ($case in @('stop-file', 'ctrl-c')) {
    $root = Join-Path $BuildRoot ('managed-' + $case + '-' + [guid]::NewGuid().ToString('N'))
    $dataRoot = Join-Path $root 'shared-data'
    [IO.Directory]::CreateDirectory((Join-Path $root 'bin')) | Out-Null
    [IO.Directory]::CreateDirectory((Join-Path $dataRoot 'logs')) | Out-Null
    foreach ($script in @('start.cmd','start.ps1','stop.ps1')) { Copy-Item -LiteralPath (Join-Path $sourceRoot ('bin/' + $script)) -Destination (Join-Path $root 'bin') }
    Copy-Item -LiteralPath (Join-Path $sourceRoot '01_AC2_시작.cmd') -Destination $root
    foreach ($binary in @('ac2.exe','ac2-updater.exe')) { Copy-Item -LiteralPath (Join-Path $BuildRoot ('bin/' + $binary)) -Destination (Join-Path $root 'bin') }
    # No real token is read. A known test input checks authentication; the
    # second case lets the backend create its first token and only checks logs.
    $synthetic = 'synthetic-startup-connection-test-value'
    if ($case -eq 'stop-file') { [IO.File]::WriteAllText((Join-Path $dataRoot 'token.txt'), $synthetic) }
    $files = @(Get-ChildItem -LiteralPath (Join-Path $root 'bin') -File | ForEach-Object { 'bin/' + $_.Name }) -join ','
    & (Join-Path $BuildRoot 'bin/ac2-updater.exe') manifest --root $root --version 0.0.1 --files $files
    Assert-True ($LASTEXITCODE -eq 0) 'manifest failed'
    $listener = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, 0)
    $listener.Start(); $port = $listener.LocalEndpoint.Port; $listener.Stop()
    $env:LOCALAPPDATA = Join-Path $BuildRoot 'localappdata'
    $env:AC2_DATA_DIR = $dataRoot
    $env:AC2_START_LOG = $null
    $env:TEMP = $root; $env:TMP = $root
    $env:AC2_CONSOLE_TEST_DIRECTORY = $root
    $env:AC2_CONSOLE_TEST_COMMAND = 'cmd.exe /d /c ""' + $root + '\01_AC2_시작.cmd" -listen 127.0.0.1 -port ' + $port + '"'
    $bootstrapId = [int](& powershell -NoProfile -ExecutionPolicy Bypass -File $driver -Action start)
    $process = $null
    try {
        $deadline = [DateTime]::UtcNow.AddSeconds(90)
        do {
            Start-Sleep -Milliseconds 100; $text = Read-Log $dataRoot
            if (-not $process -and $text -match '시작 창 프로세스: (\d+)') {
                $consoleId = [int]$Matches[1]
                $process = Get-Process -Id $consoleId; $null = $process.Handle
            }
        } until ($text.Contains('[준비 완료]') -or $text.Contains('창을 닫으려면') -or [DateTime]::UtcNow -gt $deadline)
        Assert-True ($text.Contains('[준비 완료]')) "$case readiness missing (inspect isolated startup log)"
        $cursor = -1
        foreach ($marker in @('Archive Center 2 백엔드를 시작합니다.', '[시작] PowerShell 시작', '[시작] 업데이트 도구 준비', '[시작] 백엔드 실행', '[시작] 백엔드 포트', '[시작] 데이터 폴더', '[1/5]', '[2/5]', '[3/5]', '[4/5]', '[5/5]', '[준비 완료]')) {
            $next = $text.IndexOf($marker, $cursor + 1)
            Assert-True ($next -gt $cursor) "$case stage missing or out of order: $marker"
            $cursor = $next
        }
        Assert-True ($text.Contains('http://127.0.0.1:' + $port)) 'connection address missing'
        Assert-True ($text.Contains('데이터 폴더: ' + $dataRoot)) 'shared data folder missing'
        Assert-True (-not (Test-Path -LiteralPath (Join-Path $root 'data'))) 'package-local data was created'
        Assert-True ($text -match '시작 창 프로세스: (\d+)') 'console owner missing'
        $consoleId = [int]$Matches[1]
        $process = Get-Process -Id $consoleId; $null = $process.Handle
        Assert-True ((Status ('http://127.0.0.1:' + $port + '/health') '') -eq 401) 'token check missing'
        if ($case -eq 'stop-file') {
            Assert-True ((Status ('http://127.0.0.1:' + $port + '/health') $synthetic) -eq 200) 'health failed'
            Assert-True ((Status ('http://127.0.0.1:' + $port + '/ready') $synthetic) -eq 200) 'DB readiness failed'
            Assert-True ((Status ('http://127.0.0.1:' + $port + '/health') 'wrong-synthetic-token') -eq 401) 'wrong token accepted'
            & powershell -NoProfile -ExecutionPolicy Bypass -File (Join-Path $root 'bin/stop.ps1') | Out-Null
        } else {
            Assert-True ($text.Contains('접속 토큰은 화면에만 표시')) 'first-token redaction missing'
            & powershell -NoProfile -ExecutionPolicy Bypass -File $driver -Action ctrl -TargetId $consoleId
        }
        $deadline = [DateTime]::UtcNow.AddSeconds(30)
        do { Start-Sleep -Milliseconds 100; $text = Read-Log $dataRoot } until ($text.Contains('창을 닫으려면') -or [DateTime]::UtcNow -gt $deadline)
        Assert-True ($text.Contains('정상 종료됨. 종료 코드: 0') -and $text.Contains('창을 닫으려면 아무 키나 누르세요.')) "$case did not report normal exit and wait"
        Assert-True (-not $process.HasExited) "$case closed before key"
        Assert-True (-not (Test-Path (Join-Path $dataRoot 'backend.pid'))) 'managed backend was not cleaned up'
        $remaining = @(Get-Process -Name ac2 -ErrorAction SilentlyContinue | Where-Object { $_.Path -and $_.Path.StartsWith($root + '\', [StringComparison]::OrdinalIgnoreCase) })
        Assert-True ($remaining.Count -eq 0) 'managed parent still running'
        $dbPidFiles = @(Get-ChildItem -LiteralPath (Join-Path $dataRoot 'mariadb') -Filter '*.pid' -ErrorAction SilentlyContinue)
        Assert-True ($dbPidFiles.Count -eq 0) 'managed MariaDB pid file remains after shutdown'
        Assert-True (-not ($text -match 'ac2-[a-z2-7]{6}(?:-[a-z2-7]{6}){3}|ends with|synthetic-startup-connection-test-value')) 'token material logged'
        & powershell -NoProfile -ExecutionPolicy Bypass -File $driver -Action key -TargetId $consoleId
        Assert-True ($process.WaitForExit(5000)) "$case did not exit after key"
        Assert-True ($process.ExitCode -eq 0) "$case lost successful exit code"
        $results += [PSCustomObject]@{case=$case;passed=$true;exit=$process.ExitCode;ordered_stages=$true;address=$true;token_required=$true;console_pause=$true}
        Write-Host "PASS managed $case"
    } finally {
        & powershell -NoProfile -ExecutionPolicy Bypass -File (Join-Path $root 'bin/stop.ps1') | Out-Null
        if ($process -and -not $process.HasExited) {
            & powershell -NoProfile -ExecutionPolicy Bypass -File $driver -Action key -TargetId $consoleId
        }
    }
}
if ($Output) { $results | ConvertTo-Json -Depth 4 | Set-Content -Encoding UTF8 -LiteralPath $Output }
