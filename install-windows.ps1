# Archive Center 2 — fresh install for Windows (x64).
#   irm https://raw.githubusercontent.com/Flazer31/archive-center-2/main/install-windows.ps1 | iex
# A chosen version (for example going back to 1.1.0):
#   & ([scriptblock]::Create((irm https://raw.githubusercontent.com/Flazer31/archive-center-2/main/install-windows.ps1))) -Version 1.1.0
# It downloads the Windows package of the release, checks its SHA-256 against
# the release's SHA256SUMS.txt, unpacks it under
# %USERPROFILE%\archive-center2\ac2-<version> and starts it. A folder of that
# version that already exists is never written into.
# Everything is inside one function called on the last line, so a download
# cut off inside it runs nothing.
param(
    [string]$Version = $env:AC2_VERSION,
    [string]$Repo = 'Flazer31/archive-center-2',
    [string]$Root = (Join-Path $env:USERPROFILE 'archive-center2'),
    [switch]$NoStart
)
function Install-ArchiveCenter2([string]$Version, [string]$Repo, [string]$Root, [switch]$NoStart) {
    $ErrorActionPreference = 'Stop'
    function Fail([string]$message) { throw "AC2 설치 오류: $message" }

    if ($Repo -notmatch '^[A-Za-z0-9][A-Za-z0-9-]*/[A-Za-z0-9_][A-Za-z0-9_.-]*$') { Fail 'Repo에는 GitHub 저장소의 OWNER/REPO를 입력해 주세요.' }
    if (-not [Environment]::Is64BitOperatingSystem) { Fail '64비트 Windows에서만 설치할 수 있습니다.' }
    # Windows PowerShell 5.1 does not offer TLS 1.2 by default
    try { [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12 } catch { }
    $headers = @{ 'User-Agent' = 'archive-center-2-installer' }

    function Test-Version([string]$v) {
        if ($v -notmatch '^[0-9]+\.[0-9]+\.[0-9]+$') { Fail '버전은 X.Y.Z 형식이어야 합니다(예: 1.1.0).' }
        $dir = Join-Path $Root "ac2-$v"
        if (Test-Path -LiteralPath $dir) { Fail "이 버전이 이미 설치되어 있습니다. 덮어쓰지 않습니다: $dir (그 폴더의 01_AC2_시작.cmd로 시작하세요)" }
        return $dir
    }
    if ($Version) { $installDir = Test-Version $Version }
    else {
        try { $latest = Invoke-RestMethod -UseBasicParsing -Headers $headers -Uri "https://api.github.com/repos/$Repo/releases/latest" }
        catch { Fail '최신 버전을 확인하지 못했습니다. 잠시 뒤 다시 하거나 버전을 지정해 주세요.' }
        $Version = ([string]$latest.tag_name) -replace '^v', ''
        $installDir = Test-Version $Version
    }

    $asset = "ac2-$Version-windows-amd64.zip"
    $base = "https://github.com/$Repo/releases/download/v$Version"
    $work = Join-Path ([IO.Path]::GetTempPath()) ("ac2-install-" + [Guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $work | Out-Null
    $claimed = $null
    try {
        Write-Host "Archive Center 2 $Version (windows-amd64)을 받습니다."
        $sums = Join-Path $work 'SHA256SUMS.txt'
        $zip = Join-Path $work $asset
        try { Invoke-WebRequest -UseBasicParsing -Headers $headers -Uri "$base/SHA256SUMS.txt" -OutFile $sums } catch { Fail "체크섬 목록을 받지 못했습니다: $base/SHA256SUMS.txt" }
        try { Invoke-WebRequest -UseBasicParsing -Headers $headers -Uri "$base/$asset" -OutFile $zip } catch { Fail "패키지를 받지 못했습니다: $base/$asset" }
        $expected = @(Get-Content -LiteralPath $sums | ForEach-Object {
                $parts = ($_ -replace "`r$", '').Trim() -split '\s+'
                if ($parts.Count -eq 2 -and ($parts[1] -eq $asset -or $parts[1] -eq "*$asset") -and $parts[0] -match '^[0-9a-fA-F]{64}$') { $parts[0].ToLowerInvariant() }
            })
        if ($expected.Count -ne 1) { Fail "체크섬 목록에서 $asset 항목을 확인할 수 없습니다." }
        $actual = (Get-FileHash -Algorithm SHA256 -LiteralPath $zip).Hash.ToLowerInvariant()
        if ($actual -ne $expected[0]) { Fail "$asset SHA-256 검증에 실패했습니다." }

        New-Item -ItemType Directory -Force -Path $Root | Out-Null
        # a folder that appeared meanwhile (another install running) is never written into
        if (Test-Path -LiteralPath $installDir) { Fail "설치 경로가 이미 존재합니다. 덮어쓰지 않습니다: $installDir" }
        New-Item -ItemType Directory -Path $installDir | Out-Null
        $claimed = $installDir
        Expand-Archive -LiteralPath $zip -DestinationPath $installDir
        $start = Get-ChildItem -LiteralPath $installDir -Filter '01_AC2_*.cmd' -File | Select-Object -First 1
        if (-not $start) { Fail '패키지에서 01_AC2_시작.cmd를 찾을 수 없습니다.' }
        $claimed = $null
    }
    finally {
        Remove-Item -LiteralPath $work -Recurse -Force -ErrorAction SilentlyContinue
        if ($claimed) { Remove-Item -LiteralPath $claimed -Recurse -Force -ErrorAction SilentlyContinue }
    }

    Write-Host "AC2 설치 경로: $installDir"
    Write-Host '다음부터는 이 폴더의 01_AC2_시작.cmd로 시작합니다. RisuAI 플러그인 화면의 + 버튼으로 이 폴더의 archive-center-2.js를 가져오세요.'
    if (-not $NoStart) { Start-Process -FilePath $start.FullName -WorkingDirectory $installDir }
}

Install-ArchiveCenter2 -Version $Version -Repo $Repo -Root $Root -NoStart:$NoStart
