$ErrorActionPreference = 'Stop'

$repo = if ($env:TAURUS_RELAY_REPO) { $env:TAURUS_RELAY_REPO } else { 'taurusagents/taurus-relay' }
$taurusUrl = if ($env:TAURUS_URL) { $env:TAURUS_URL.TrimEnd('/') } else { 'https://app.taurus.cloud' }
$version = if ($env:TAURUS_RELAY_VERSION) { $env:TAURUS_RELAY_VERSION } else { 'latest' }
$skipConnect = $env:TAURUS_RELAY_SKIP_CONNECT -eq '1'
$installDir = if ($env:TAURUS_INSTALL_DIR) {
  $env:TAURUS_INSTALL_DIR
} elseif ($env:LOCALAPPDATA) {
  Join-Path $env:LOCALAPPDATA 'Programs\TaurusRelay\bin'
} else {
  Join-Path $HOME 'AppData\Local\Programs\TaurusRelay\bin'
}

function Write-InstallNote {
  param([string]$Message)
  Write-Host $Message
}

function Get-RelayArch {
  $arch = if ($env:PROCESSOR_ARCHITEW6432) { [string]$env:PROCESSOR_ARCHITEW6432 } else { [string]$env:PROCESSOR_ARCHITECTURE }
  switch -Regex ($arch.ToUpperInvariant()) {
    '^AMD64$' { return 'amd64' }
    '^ARM64$' { return 'arm64' }
    default { throw "Unsupported Windows architecture: $arch" }
  }
}

function Ensure-UserPathContains {
  param([string]$Dir)

  $segments = @()
  if ($env:Path) {
    $segments = $env:Path -split ';'
  }
  if ($segments -contains $Dir) {
    return
  }

  $env:Path = if ($env:Path) { "$env:Path;$Dir" } else { $Dir }

  $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
  $userSegments = @()
  if ($userPath) {
    $userSegments = $userPath -split ';'
  }
  if ($userSegments -contains $Dir) {
    return
  }

  $nextUserPath = if ($userPath) { "$userPath;$Dir" } else { $Dir }
  [Environment]::SetEnvironmentVariable('Path', $nextUserPath, 'User')
  Write-InstallNote "Added $Dir to your user PATH."
}

function Verify-Checksum {
  param(
    [string]$ArchivePath,
    [string]$ChecksumsPath,
    [string]$ArchiveName
  )

  $expectedLine = Select-String -Path $ChecksumsPath -Pattern ([regex]::Escape($ArchiveName) + '$') | Select-Object -First 1
  if (-not $expectedLine) {
    throw "Could not find checksum for $ArchiveName"
  }

  $expected = (($expectedLine.Line -split '\s+')[0]).Trim()
  $actual = (Get-FileHash -Path $ArchivePath -Algorithm SHA256).Hash.ToLowerInvariant()
  if ($actual -ne $expected.ToLowerInvariant()) {
    throw "Checksum verification failed for $ArchiveName"
  }
}

if ($version -eq 'latest') {
  $releaseBaseUrl = "https://github.com/$repo/releases/latest/download"
  $releaseLabel = 'latest release'
} elseif (-not $version.StartsWith('v')) {
  $version = "v$version"
  $releaseBaseUrl = "https://github.com/$repo/releases/download/$version"
  $releaseLabel = $version
} else {
  $releaseBaseUrl = "https://github.com/$repo/releases/download/$version"
  $releaseLabel = $version
}

$arch = Get-RelayArch
$archiveName = "taurus-relay_windows_${arch}.zip"
$archiveUrl = "$releaseBaseUrl/$archiveName"
$checksumsUrl = "$releaseBaseUrl/checksums.txt"

$tmpDir = Join-Path ([System.IO.Path]::GetTempPath()) ("taurus-relay-install-" + [guid]::NewGuid().ToString('N'))
$archivePath = Join-Path $tmpDir $archiveName
$checksumsPath = Join-Path $tmpDir 'checksums.txt'
$extractDir = Join-Path $tmpDir 'extract'
$binaryPath = Join-Path $installDir 'taurus-relay.exe'

New-Item -ItemType Directory -Path $tmpDir, $extractDir, $installDir -Force | Out-Null

try {
  Write-InstallNote "Downloading $archiveName ($releaseLabel)..."
  Invoke-WebRequest -Uri $archiveUrl -OutFile $archivePath
  Invoke-WebRequest -Uri $checksumsUrl -OutFile $checksumsPath
  Verify-Checksum -ArchivePath $archivePath -ChecksumsPath $checksumsPath -ArchiveName $archiveName

  Expand-Archive -Path $archivePath -DestinationPath $extractDir -Force
  $extractedBinary = Join-Path $extractDir 'taurus-relay.exe'
  if (-not (Test-Path $extractedBinary)) {
    throw 'Archive did not contain taurus-relay.exe'
  }

  Copy-Item -Path $extractedBinary -Destination $binaryPath -Force
  Ensure-UserPathContains -Dir $installDir
  Write-InstallNote "Installed taurus-relay to $binaryPath"

  if ($skipConnect) {
    Write-InstallNote 'Skipping taurus-relay connect because TAURUS_RELAY_SKIP_CONNECT=1.'
    return
  }

  if (-not $env:TAURUS_TOKEN) {
    throw 'TAURUS_TOKEN is required unless TAURUS_RELAY_SKIP_CONNECT=1'
  }

  $connectArgs = @('connect')
  if ($taurusUrl -like 'http://*') {
    Write-InstallNote "Warning: $taurusUrl is non-TLS; passing --insecure to taurus-relay connect."
    $connectArgs += '--insecure'
  }
  $connectArgs += @('--token', $env:TAURUS_TOKEN, '--server', $taurusUrl)

  Write-InstallNote "Starting taurus-relay connect against $taurusUrl..."
  & $binaryPath @connectArgs
  if ($LASTEXITCODE -ne 0) {
    exit $LASTEXITCODE
  }
} finally {
  Remove-Item -Path $tmpDir -Recurse -Force -ErrorAction SilentlyContinue
}
