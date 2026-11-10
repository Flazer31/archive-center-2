$ErrorActionPreference = 'Stop'
$packageRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$stage = '시작 로그 준비'
$code = 1
$utf8 = New-Object Text.UTF8Encoding($false)
$logPath = $env:AC2_START_LOG
function Write-StartMessage([string]$Text) {
    Write-Host $Text
    if ($script:logPath) {
        try { [IO.File]::AppendAllText($script:logPath, $Text + [Environment]::NewLine, $script:utf8) }
        catch { Write-Host ('시작 로그를 쓰지 못했습니다: ' + $_.Exception.Message) }
    }
}
# A native executable can be held by a scanner before its Go heartbeat starts.
# This timer only displays progress; it never interrupts or times out a command.
try {
Add-Type -TypeDefinition @'
using System;
using System.IO;
using System.Text;
using System.Threading;
using System.Runtime.InteropServices;
public sealed class AC2StartupWait : IDisposable {
    private readonly Timer timer;
    private delegate bool Handler(uint kind);
    private static readonly Handler control = kind => kind == 0 || kind == 1;
    [DllImport("kernel32.dll", EntryPoint="SetConsoleCtrlHandler")]
    private static extern bool SetHandler(Handler handler, bool add);
    [DllImport("kernel32.dll", EntryPoint="SetConsoleCtrlHandler")]
    private static extern bool SetIgnored(IntPtr handler, bool ignored);
    [StructLayout(LayoutKind.Sequential)]
    private struct BasicProcess { public IntPtr reserved, peb, reserved2, reserved3, pid, parent; }
    [DllImport("ntdll.dll")]
    private static extern int NtQueryInformationProcess(IntPtr process, int kind, out BasicProcess info, int size, out int returned);
    public static long ConsoleOwner() {
        BasicProcess info; int returned;
        if (NtQueryInformationProcess(System.Diagnostics.Process.GetCurrentProcess().Handle, 0, out info, Marshal.SizeOf(typeof(BasicProcess)), out returned) != 0)
            return 0; // Diagnostic only; never block startup on a PID lookup.
        return info.parent.ToInt64();
    }
    public static void OwnConsole() {
        // The shell stays alive; Go's existing signal owner stops its children.
        SetIgnored(IntPtr.Zero, false);
        SetHandler(control, true);
    }
    public AC2StartupWait(string path) {
        timer = new Timer(_ => {
            const string text = "아직 진행 중입니다: 업데이트 도구 준비 (백신 검사 등으로 늦어질 수 있어요).";
            Console.Error.WriteLine(text);
            try {
                using (var file = new FileStream(path, FileMode.Append, FileAccess.Write, FileShare.ReadWrite))
                using (var writer = new StreamWriter(file, new UTF8Encoding(false))) { writer.WriteLine(text); }
            } catch { }
        }, null, 15000, 15000);
    }
    public void Dispose() { timer.Dispose(); }
}
'@
    [AC2StartupWait]::OwnConsole()
    if ($logPath) {
        try { [IO.File]::AppendAllText($logPath, '', $utf8) }
        catch { Write-Host ('시작 로그 경로를 사용할 수 없습니다: ' + $_.Exception.Message); $logPath = $null }
    }
    if (-not $logPath) {
        # Same default as the backend's shared data folder (datapath.Resolve); logs only.
        $dataRoot = $env:AC2_DATA_DIR
        if (-not $dataRoot) {
            $localRoot = $env:LOCALAPPDATA
            if (-not $localRoot) { $localRoot = Join-Path $env:USERPROFILE 'AppData\Local' }
            $dataRoot = Join-Path $localRoot 'ArchiveCenter2\data'
        }
        $expectData = $false
        foreach ($argument in $args) {
            if ($expectData) { $dataRoot = $argument; $expectData = $false; continue }
            if ($argument -in @('-data', '--data')) { $expectData = $true }
            elseif ($argument -match '^--?data=(.*)$') { $dataRoot = $Matches[1] }
        }
        if (-not [IO.Path]::IsPathRooted($dataRoot)) { $dataRoot = Join-Path $packageRoot $dataRoot }
        foreach ($logDir in @((Join-Path $dataRoot 'logs'), (Join-Path ([IO.Path]::GetTempPath()) 'ArchiveCenter2-start-logs'))) {
            try {
                [IO.Directory]::CreateDirectory($logDir) | Out-Null
                $candidate = Join-Path $logDir ('start-' + (Get-Date -Format 'yyyyMMdd-HHmmss-fff') + '-' + $PID + '.log')
                [IO.File]::WriteAllText($candidate, 'Archive Center 2 백엔드를 시작합니다. 이 창을 닫으면 백엔드가 꺼집니다.' + [Environment]::NewLine, $utf8)
                $logPath = $candidate
                break
            } catch { Write-Host ('시작 로그 경로를 사용할 수 없습니다: ' + $_.Exception.Message) }
        }
    }
    $env:AC2_START_LOG = $logPath
    Write-StartMessage ('시작 창 프로세스: ' + [AC2StartupWait]::ConsoleOwner())
    if ($logPath) {
        Get-ChildItem -LiteralPath ([IO.Path]::GetDirectoryName($logPath)) -Filter 'start-*.log' -File -ErrorAction SilentlyContinue |
            Sort-Object LastWriteTime -Descending | Select-Object -Skip 10 | ForEach-Object { Remove-Item -LiteralPath $_.FullName -ErrorAction SilentlyContinue }
    }
    Write-StartMessage ('시작 로그: ' + $logPath)
    # A program still marked as downloaded from the internet may be held or
    # scanned long by Windows security (1.0 P02/O10); only told, nothing changed
    foreach ($exe in @('ac2.exe', 'ac2-updater.exe')) {
        $p = Join-Path $PSScriptRoot $exe
        if ((Test-Path -LiteralPath $p) -and (Get-Item -LiteralPath $p -Stream Zone.Identifier -ErrorAction SilentlyContinue)) {
            Write-StartMessage ('안내: ' + $exe + '에 인터넷에서 받은 표시가 남아 있어요. Windows 보안이 검사하느라 시작이 오래 걸리거나 막을 수 있어요. 막히면 ZIP 파일 속성에서 ''차단 해제''를 선택한 뒤 다시 풀고, 03_AC2_진단.cmd로 보고서를 만들어 주세요.')
            break
        }
    }
    $stage = '업데이트 도구 준비'
    Write-StartMessage ('[시작] ' + $stage)
    $bootstrap = Join-Path $PSScriptRoot 'ac2-updater.exe'
    $stateFile = Join-Path $packageRoot '.updates/update-state.json'
    $state = $null
    if (Test-Path -LiteralPath $stateFile) {
        try { $state = Get-Content -LiteralPath $stateFile -Raw -Encoding UTF8 | ConvertFrom-Json } catch { }
    }
    if (-not $state -and (Test-Path -LiteralPath ($stateFile + '.previous'))) {
        $state = Get-Content -LiteralPath ($stateFile + '.previous') -Raw -Encoding UTF8 | ConvertFrom-Json
    }
    # Preserve the interrupted update's original runner identity and containment.
    if ($state.status -in @('applying', 'applied_pending_health') -and $state.runner_path) {
        $bootstrap = [IO.Path]::GetFullPath((Join-Path $packageRoot $state.runner_path))
        $runnerRoot = [IO.Path]::GetFullPath((Join-Path $packageRoot '.updates/runner'))
        if (-not $bootstrap.StartsWith($runnerRoot + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) {
            throw 'runner_path escapes .updates/runner'
        }
    }
    $waiting = New-Object AC2StartupWait($logPath)
    try {
        $runner = & $bootstrap prepare-runner --root $packageRoot
        $code = $LASTEXITCODE
    } finally { $waiting.Dispose() }
    if ($code -ne 0) { throw '업데이트 도구 준비 실패. 위 원래 오류를 확인해 주세요.' }
    $stage = '백엔드 실행'
    Write-StartMessage ('[시작] ' + $stage)
    & $runner run --root $packageRoot -- -managed @args
    $code = $LASTEXITCODE
    if ($code -ne 0) { throw '백엔드 실행 실패. 위 단계와 원래 오류를 확인해 주세요.' }
} catch {
    if ($code -eq 0) { $code = 1 }
    Write-StartMessage ('오류: ' + $stage + '에서 멈췄습니다. 종료 코드: ' + $code)
    # one record the support report summarizes (diagnostics.projectLog): the
    # error text, possibly quoting a file's content, never becomes log lines
    Write-StartMessage ('시작 오류: ' + ($_.Exception.Message -replace '\s*[\r\n]+\s*', ' '))
    Write-StartMessage '실행 파일이 없거나 열리지 않으면 압축을 모두 풀었는지, 백신 차단·격리를 확인해 주세요.'
    Write-StartMessage '접근 거부는 파일을 사용하는 프로그램을 확인한 뒤 다시 실행하세요. 포트 사용 중이면 이미 켜진 시작 창을 확인하고, MariaDB 내려받기 오류는 인터넷 연결을 확인해 주세요.'
    Write-StartMessage ('시작 로그: ' + $logPath)
}
exit $code
