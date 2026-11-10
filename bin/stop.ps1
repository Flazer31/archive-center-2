param([string]$Data)
$ErrorActionPreference = 'Stop'
if (-not $PSBoundParameters.ContainsKey('Data')) {
    $packageRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
    $Data = & (Join-Path $PSScriptRoot 'ac2-updater.exe') data-path --root $packageRoot
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
}
$pidFile = Join-Path $Data 'backend.pid'
if (-not (Test-Path -LiteralPath $pidFile)) { Write-Host '열린 AC2 시작 창이 없습니다.'; exit 0 }
$backendId = [int]([IO.File]::ReadAllText($pidFile).Trim())
$expected = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot 'ac2.exe'))
$backend = Get-Process -Id $backendId -ErrorAction SilentlyContinue
if ($backend -and $backend.Path -eq $expected) {
    # The owning launcher observes this exit and shuts down its DB via SQL.
    Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;
public static class AC2PackageStop {
    [DllImport("kernel32.dll", SetLastError=true)]
    public static extern bool TerminateProcess(IntPtr process, uint exitCode);
}
'@
    if (-not [AC2PackageStop]::TerminateProcess($backend.Handle, 0)) {
        throw [ComponentModel.Win32Exception]::new([Runtime.InteropServices.Marshal]::GetLastWin32Error())
    }
    Write-Host 'AC2를 종료합니다. 시작 창이 닫힐 때까지 기다려 주세요.'
} elseif ($backend) { throw '이 설치의 백엔드 프로세스가 아닙니다. 열린 시작 창에서 Ctrl+C로 종료해 주세요.' }
