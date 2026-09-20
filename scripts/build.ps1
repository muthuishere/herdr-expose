#Requires -Version 5.1
<#
  Build herdr-expose on Windows: the React app first, then the Go binary with
  that app embedded in it. The Windows twin of scripts/build.sh.

  WHY THIS FILE EXISTS. Herdr runs a plugin's [[build]] command with
  CreateProcess, which cannot execute a .sh file — there is no shebang handling
  on Windows, and even with Git's bash.exe present it is not on PATH (a default
  Git for Windows install puts only Git\cmd there, not Git\bin). So on Windows
  ./scripts/build.sh is unrunnable, and herdr-plugin.toml selects this script
  instead via the per-entry `platforms` filter on [[build]].

  Without it, a Windows `herdr plugin install` SUCCEEDS and builds nothing:
  Herdr prints "build (skipped on windows)", installs the plugin, enables it,
  and leaves every action pointing at a ./bin/herdr-expose that does not exist.
  That was measured, not imagined.

    .\scripts\build.ps1               source if the toolchain is there,
                                      else a release asset -> bin\herdr-expose.exe
    .\scripts\build.ps1 -Source       build from source; fail if go/npm missing
    .\scripts\build.ps1 -Download     always fetch the release asset
    .\scripts\build.ps1 -SkipWeb      Go only (web\dist must already exist)
    .\scripts\build.ps1 -NoLink       build only; install no agent skill
#>
[CmdletBinding()]
param(
  [switch]$Source,
  [switch]$Download,
  [switch]$SkipWeb,
  [switch]$NoLink
)

# NOT 'Stop'. With ErrorActionPreference=Stop, ANY native command that writes a
# line to stderr becomes a terminating error -- and git, npm and go all do that
# routinely while succeeding. `git describe` in a checkout with no .git killed
# this script at line 1 of real use. A build script that shells out constantly
# has to judge by EXIT CODE, which is what every call below does.
$ErrorActionPreference = 'Continue'
$RepoRoot = Split-Path -Parent $PSScriptRoot
Set-Location $RepoRoot

$BinName = 'herdr-expose'
$Pkg     = './cmd/herdr-expose'
$Repo    = if ($env:REPO) { $env:REPO } else { 'muthuishere/herdr-expose' }
$RepoApi = if ($env:REPO_API) { $env:REPO_API } else { 'https://api.github.com' }
if ($env:HERDR_EXPOSE_NO_LINK -eq '1') { $NoLink = $true }

function Log  ($m) { Write-Host "==> $m" -ForegroundColor Blue }
function Warn ($m) { Write-Host "warning: $m" -ForegroundColor Yellow }
function Die  ($m) { Write-Host "error: $m" -ForegroundColor Red; exit 1 }
function Have ($n) { $null -ne (Get-Command $n -ErrorAction SilentlyContinue -CommandType Application) }

# git describe, when git is around; a plugin install always has it, since Herdr
# used git to clone this checkout in the first place.
function Try-Native {
  param([string]$Exe, [string[]]$Args)
  if (-not (Get-Command $Exe -ErrorAction SilentlyContinue)) { return $null }
  $out = (& $Exe @Args 2>&1 | Out-String).Trim()
  if ($LASTEXITCODE -ne 0) { return $null }
  return $out
}

$Version = $env:VERSION
if (-not $Version) {
  $Version = Try-Native 'git' @('describe','--tags','--always','--dirty')
  if (-not $Version) { $Version = 'dev' }
}
$Commit = Try-Native 'git' @('rev-parse','--short','HEAD'); if (-not $Commit) { $Commit = 'unknown' }
$BuildDate = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')
$LdFlags = "-s -w -X main.version=$Version -X main.commit=$Commit -X main.buildDate=$BuildDate"


# ------------------------------------------------------------------ finish ---
function Show-Result {
  $target = "$RepoRoot\bin\$BinName.exe"
  if (-not (Test-Path $target)) { Die "no $target was produced" }
  # There is no ~/.local/bin convention on Windows and no privilege to symlink
  # into one, so say where it is rather than pretending to put it on PATH.
  Log "add $RepoRoot\bin to your PATH, or call $target directly"
  if ($NoLink) {
    Log "-NoLink: no agent skill installed (do it later with: $BinName skill install)"
    return
  }
  # The skill half lives in the binary, so there is exactly one implementation
  # of "where does skill\ belong". On Windows it lands as a directory junction,
  # because os.Symlink needs a privilege an ordinary user does not have.
  & $target skill install
  if ($LASTEXITCODE -ne 0) { Warn "the agent skill was not linked. Fix that, then run: $BinName skill install" }
}

# ------------------------------------------------------------ release asset --
function Install-FromRelease {
  # curl.exe and tar.exe have shipped in System32 since Windows 10 1803, so this
  # path needs nothing installed. Note curl.exe, NOT curl: in PowerShell `curl`
  # is an alias for Invoke-WebRequest and takes different arguments entirely.
  $arch = switch ($env:PROCESSOR_ARCHITECTURE) {
    'AMD64' { 'amd64' } 'ARM64' { 'arm64' } default { $null }
  }
  if (-not $arch) { Die "no prebuilt binary for $env:PROCESSOR_ARCHITECTURE; install Go 1.25+ and node 20+ and build from source." }

  Log "fetching the latest $Repo release for windows/$arch"
  $api = "$RepoApi/repos/$Repo/releases/latest"
  try { $rel = Invoke-RestMethod -Uri $api -Headers @{ Accept = 'application/vnd.github+json' } -UseBasicParsing }
  catch { Die "cannot read $api ($($_.Exception.Message)). Install Go 1.25+ and node 20+ to build from source instead." }

  $asset = $rel.assets | Where-Object { $_.name -like "*_windows_$arch.tar.gz" } | Select-Object -First 1
  $sums  = $rel.assets | Where-Object { $_.name -like '*_SHA256SUMS' } | Select-Object -First 1
  if (-not $asset) { Die "no windows/$arch asset in the latest $Repo release. Install Go 1.25+ and node 20+ and build from source." }
  if (-not $sums)  { Die "the latest $Repo release has no SHA256SUMS; refusing to install an unverified binary." }

  $tmp = Join-Path ([IO.Path]::GetTempPath()) ("herdr-expose-" + [Guid]::NewGuid().ToString('N'))
  New-Item -ItemType Directory -Force -Path $tmp | Out-Null
  try {
    Invoke-WebRequest -Uri $asset.browser_download_url -OutFile "$tmp\$($asset.name)" -UseBasicParsing
    Invoke-WebRequest -Uri $sums.browser_download_url  -OutFile "$tmp\SHA256SUMS"     -UseBasicParsing

    $want = (Select-String -Path "$tmp\SHA256SUMS" -Pattern ([regex]::Escape($asset.name)) |
             Select-Object -First 1).Line -split '\s+' | Select-Object -First 1
    if (-not $want) { Die "$($asset.name) is not listed in the release SHA256SUMS." }
    $got = (Get-FileHash -Algorithm SHA256 -Path "$tmp\$($asset.name)").Hash.ToLower()
    if ($want.ToLower() -ne $got) { Die "checksum mismatch for $($asset.name) (expected $want, got $got)." }
    Log 'checksum ok'

    New-Item -ItemType Directory -Force -Path "$RepoRoot\bin" | Out-Null
    & tar.exe -xzf "$tmp\$($asset.name)" -C "$RepoRoot\bin" "$BinName.exe"
    if ($LASTEXITCODE -ne 0) { Die "could not unpack $($asset.name)" }
    Log "installed bin\$BinName.exe from $($asset.name)"
  } finally { Remove-Item $tmp -Recurse -Force -ErrorAction SilentlyContinue }
}

# --------------------------------------------------------------------- web ---
function Build-Web {
  if (-not (Have 'npm') -and -not (Get-Command npm -ErrorAction SilentlyContinue)) {
    Die 'Install node 20+ to build the web app, or run with -SkipWeb.'
  }
  Log 'building web app (npm ci; npm run build)'
  Push-Location "$RepoRoot\web"
  try {
    & npm ci;          if ($LASTEXITCODE -ne 0) { Die 'npm ci failed' }
    & npm run build;   if ($LASTEXITCODE -ne 0) { Die 'npm run build failed' }
  } finally { Pop-Location }
  if (-not (Test-Path "$RepoRoot\web\dist\index.html")) { Die 'web build produced no web\dist\index.html' }
}

# ---------------------------------------------------------------- dispatch ---
$haveGo  = $null -ne (Get-Command go  -ErrorAction SilentlyContinue)
$haveNpm = $null -ne (Get-Command npm -ErrorAction SilentlyContinue)

if ($Download) { Install-FromRelease; Show-Result; exit 0 }

if (-not $Source) {
  $missing = @()
  if (-not $haveGo) { $missing += 'Go 1.25+' }
  if (-not $SkipWeb -and -not $haveNpm -and -not (Test-Path "$RepoRoot\web\dist\index.html")) { $missing += 'node 20+' }
  if ($missing.Count -gt 0) {
    Warn "$($missing -join ' and ') not found on PATH; falling back to a published release binary."
    Install-FromRelease
    Show-Result
    exit 0
  }
}

if (-not $SkipWeb) { Build-Web }
else {
  Log 'skipping web build (-SkipWeb)'
  if (-not (Test-Path "$RepoRoot\web\dist\index.html")) { Die 'web\dist\index.html missing; cannot -SkipWeb' }
}

if (-not $haveGo) { Die 'Install Go 1.25+ from https://go.dev/dl/, or run .\scripts\build.ps1 -Download.' }

Log "building bin\$BinName.exe, version $Version"
New-Item -ItemType Directory -Force -Path "$RepoRoot\bin" | Out-Null
$env:CGO_ENABLED = '0'
& go build -trimpath -ldflags $LdFlags -o "$RepoRoot\bin\$BinName.exe" $Pkg
if ($LASTEXITCODE -ne 0) { Die 'go build failed' }
Log "built bin\$BinName.exe"

Show-Result
