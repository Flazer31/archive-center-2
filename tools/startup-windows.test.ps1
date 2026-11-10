# Run against disposable binaries built from this worktree. No credential reads.
param([string]$BuildRoot = (Join-Path $PSScriptRoot '../.startup-test'), [string]$Output = '')
$ErrorActionPreference = 'Stop'
$BuildRoot = [IO.Path]::GetFullPath($BuildRoot)
$sourceRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$driver = Join-Path $PSScriptRoot 'startup-console-test.ps1'
$powerShell = Join-Path $PSHOME 'powershell.exe'
$results = @()
function Assert-True($Value, [string]$Message) { if (-not $Value) { throw $Message } }
function Read-TestLog([string]$Root, [string]$DataRoot) {
    $latest = Get-ChildItem -LiteralPath (Join-Path $DataRoot 'logs'),(Join-Path $Root 'ArchiveCenter2-start-logs') -Filter 'start-*.log' -ErrorAction SilentlyContinue |
        Sort-Object LastWriteTime -Descending | Select-Object -First 1
    if ($latest) {
        $file = [IO.FileStream]::new($latest.FullName, [IO.FileMode]::Open, [IO.FileAccess]::Read, [IO.FileShare]::ReadWrite)
        $reader = [IO.StreamReader]::new($file, [Text.Encoding]::UTF8)
        try { return $reader.ReadToEnd() } finally { $reader.Dispose() }
    }
    return ''
}
function Send-Key([int]$Id) { & $powerShell -NoProfile -ExecutionPolicy Bypass -File $driver -Action key -TargetId $Id }
foreach ($case in @('missing-updater', 'missing-backend', 'invalid-backend', 'busy-port', 'download-failure', 'log-fallback', 'retention', 'missing-powershell', 'missing-start-script', 'shared-default', 'profile-default', 'explicit-data')) {
    $root = Join-Path $BuildRoot ($case + '-' + [guid]::NewGuid().ToString('N'))
    $dataRoot = Join-Path $root 'shared-data'
    [IO.Directory]::CreateDirectory((Join-Path $root 'bin')) | Out-Null
    foreach ($script in @('start.cmd', 'start.ps1', 'stop.ps1')) { Copy-Item -LiteralPath (Join-Path $sourceRoot ('bin/' + $script)) -Destination (Join-Path $root 'bin') }
    if ($case -eq 'missing-start-script') { Remove-Item -LiteralPath (Join-Path $root 'bin/start.ps1') }
    $noUpdater = $case -in @('missing-updater','log-fallback','retention','missing-powershell','missing-start-script','shared-default','profile-default','explicit-data')
    if (-not $noUpdater) { Copy-Item -LiteralPath (Join-Path $BuildRoot 'bin/ac2-updater.exe') -Destination (Join-Path $root 'bin') }
    if (-not $noUpdater -and $case -ne 'missing-backend') { Copy-Item -LiteralPath (Join-Path $BuildRoot 'bin/ac2.exe') -Destination (Join-Path $root 'bin') }
    if ($case -eq 'invalid-backend') { [IO.File]::WriteAllText((Join-Path $root 'bin/ac2.exe'), 'synthetic invalid executable') }
    if (-not $noUpdater) {
        $files = @(Get-ChildItem -LiteralPath (Join-Path $root 'bin') -File | ForEach-Object { 'bin/' + $_.Name }) -join ','
        & (Join-Path $BuildRoot 'bin/ac2-updater.exe') manifest --root $root --version 0.0.1 --files $files
        Assert-True ($LASTEXITCODE -eq 0) 'test manifest creation failed'
    }
    $listener = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, 0)
    $listener.Start()
    $port = $listener.LocalEndpoint.Port
    if ($case -ne 'busy-port') { $listener.Stop() }
    $env:LOCALAPPDATA = Join-Path $root 'localappdata'
    $env:USERPROFILE = Join-Path $root 'profile'
    $env:AC2_DATA_DIR = $dataRoot
    $env:AC2_START_LOG = $null
    [IO.Directory]::CreateDirectory($env:LOCALAPPDATA) | Out-Null
    $env:TEMP = $root
    $env:TMP = $root
    $dataArgs = @()
    if ($case -eq 'shared-default') {
        $env:AC2_DATA_DIR = $null
        $dataRoot = Join-Path $env:LOCALAPPDATA 'ArchiveCenter2/data'
    } elseif ($case -eq 'profile-default') {
        $env:AC2_DATA_DIR = $null
        $env:LOCALAPPDATA = $null
        $dataRoot = Join-Path $env:USERPROFILE 'AppData/Local/ArchiveCenter2/data'
    } elseif ($case -eq 'explicit-data') {
        $dataArgs = @('-data', 'ignored', '--data=chosen data')
        $dataRoot = Join-Path $root 'chosen data'
    }
    $resolved = & (Join-Path $BuildRoot 'bin/ac2-updater.exe') data-path --root $root -- @dataArgs
    Assert-True ($LASTEXITCODE -eq 0 -and $resolved -eq $dataRoot) "$case data-path mismatch"
    if ($case -eq 'log-fallback') {
        [IO.Directory]::CreateDirectory($dataRoot) | Out-Null
        [IO.File]::WriteAllText((Join-Path $dataRoot 'logs'), 'synthetic blocked log directory')
    }
    if ($case -eq 'retention') {
        $logDir = Join-Path $dataRoot 'logs'
        [IO.Directory]::CreateDirectory($logDir) | Out-Null
        foreach ($index in 1..14) { $file = Join-Path $logDir ('start-old-' + $index + '.log'); [IO.File]::WriteAllText($file,'old'); [IO.File]::SetLastWriteTime($file,[DateTime]::Now.AddDays(-1)) }
        [IO.File]::WriteAllText((Join-Path $logDir 'other.log'),'keep')
    }
    $env:HTTPS_PROXY = ''
    if ($case -eq 'download-failure') { $env:HTTPS_PROXY = 'http://127.0.0.1:' + $port }
    $dataCommand = if ($case -eq 'explicit-data') { ' -data ignored "--data=chosen data"' } else { '' }
    $env:AC2_CONSOLE_TEST_COMMAND = 'cmd.exe /d /c ""' + $root + '\bin\start.cmd" -listen 127.0.0.1 -port ' + $port + $dataCommand + '"'
    $env:AC2_CONSOLE_TEST_DIRECTORY = $root
    try {
        if ($case -eq 'missing-powershell') { $env:AC2_CONSOLE_TEST_SYSTEMROOT = Join-Path $root 'missing-windows' }
        $consoleId = [int](& $powerShell -NoProfile -ExecutionPolicy Bypass -File $driver -Action start)
    } finally { $env:AC2_CONSOLE_TEST_SYSTEMROOT = $null }
    [PSCustomObject]@{console=$consoleId;case=$case;root=$root} | ConvertTo-Json | Set-Content -Encoding UTF8 -LiteralPath (Join-Path $root 'console-test.json')
    $process = $null
    try {
        $deadline = [DateTime]::UtcNow.AddSeconds(45)
        do {
            Start-Sleep -Milliseconds 100
            $text = Read-TestLog $root $dataRoot
        } until ($text.Contains('창을 닫으려면 아무 키나 누르세요.') -or [DateTime]::UtcNow -gt $deadline)
        Assert-True ($text.StartsWith('Archive Center 2 백엔드를 시작합니다.')) "$case omitted immediate banner"
        Assert-True ($text.Contains('오류:') -and $text.Contains('종료 코드:') -and $text.Contains('백신')) "$case omitted error details"
        Assert-True ($text.Contains('시작 로그:')) "$case omitted log location"
        Assert-True (-not (Test-Path -LiteralPath (Join-Path $root 'data'))) "$case created package-local data"
        switch ($case) {
            'missing-updater' { Assert-True ($text.Contains('ac2-updater.exe') -and $text.Contains('업데이트 도구 준비에서')) 'missing updater cause omitted' }
            { $_ -in @('shared-default','profile-default','explicit-data') } { Assert-True ($text.Contains('ac2-updater.exe') -and $text.Contains('업데이트 도구 준비에서')) 'path case missing updater cause omitted'; Assert-True (Test-Path -LiteralPath (Join-Path $dataRoot 'logs')) 'resolved shared log path missing' }
            'missing-backend' { Assert-True ($text.Contains('ac2.exe') -and $text.Contains('launcher_failed')) 'missing backend cause omitted' }
            'invalid-backend' { Assert-True ($text.Contains('ac2.exe') -and $text.Contains('launcher_failed')) 'invalid backend cause omitted' }
            'busy-port' { Assert-True ($text.Contains([string]$port) -and $text.Contains('포트')) 'port failure cause omitted' }
            'download-failure' { Assert-True ($text.Contains('MariaDB 내려받기 실패') -and $text.Contains('proxyconnect')) 'download failure cause omitted' }
            'log-fallback' { Assert-True ($text.Contains('ArchiveCenter2-start-logs')) 'temporary log fallback missing' }
            'retention' { Assert-True (@(Get-ChildItem -LiteralPath $logDir -Filter 'start-*.log').Count -eq 10) 'retention count incorrect'; Assert-True (Test-Path -LiteralPath (Join-Path $logDir 'other.log')) 'unrelated log deleted' }
            'missing-powershell' { Assert-True ($text.Contains('시작 명령에서 멈췄습니다') -and $text.Contains('PowerShell 실행 파일:')) 'PowerShell startup cause missing' }
            'missing-start-script' { Assert-True ($text.Contains('시작 명령에서 멈췄습니다') -and $text.Contains('시작 스크립트:')) 'missing script cause missing' }
        }
        Assert-True ($text.Contains('창을 닫으려면 아무 키나 누르세요.')) "$case did not reach pause"
        # PowerShell publishes its waiting CMD parent's PID before the updater
        # starts. That CMD, rather than the short-lived entry CMD, owns pause.
        if ($case -notin @('missing-powershell','missing-start-script')) {
            Assert-True ($text -match '시작 창 프로세스: (\d+)') "$case omitted console owner"
            $consoleId = [int]$Matches[1]
        } else {
            $consoleId = [int](& $powerShell -NoProfile -ExecutionPolicy Bypass -File $driver -Action owner -TargetId $consoleId)
        }
        $process = Get-Process -Id $consoleId
        $null = $process.Handle
        Start-Sleep -Milliseconds 350
        Assert-True (-not $process.HasExited) "$case window closed before key"
        Send-Key $consoleId
        Assert-True ($process.WaitForExit(5000)) "$case did not leave pause after key"
        Assert-True ($null -ne $process.ExitCode -and $process.ExitCode -ne 0) "$case swallowed failure status"
        $results += [PSCustomObject]@{case=$case;passed=$true;exit=$process.ExitCode;console_pause=$true;log=(Join-Path $dataRoot 'logs')}
        Write-Host "PASS $case"
    } finally {
        $listener.Stop()
        if ($process -and -not $process.HasExited) { Send-Key $consoleId }
    }
}
if ($Output) { $results | ConvertTo-Json -Depth 4 | Set-Content -Encoding UTF8 -LiteralPath $Output }
$results
