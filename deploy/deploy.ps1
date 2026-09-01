# Cross-compile rnv3 (+ rnv3-migrate) for the Pi and deploy over SSH.
#
#   .\deploy\deploy.ps1 [-PiHost raspinoaa] [-PiUser pi] [-Arch arm64]
#                       [-InstallArgs "--skip-builds --no-start"] [-NoInstall]
#
# -InstallArgs are passed through to deploy/install.sh on the Pi.
# -NoInstall only copies the files (to /tmp/rnv3-deploy) without installing.
param(
    [string]$PiHost = "raspinoaa",
    [string]$PiUser = "pi",
    [string]$Arch = "arm64",   # arm64 for Pi 3/4/5 64-bit; amd64 for x64 PCs
    [string]$InstallArgs = "",
    [switch]$NoInstall
)
$ErrorActionPreference = "Stop"
$repo = Split-Path -Parent $PSScriptRoot

# Find the Go toolchain: PATH first, then the usual install locations.
function Find-Go {
    $cmd = Get-Command go -ErrorAction SilentlyContinue
    if ($cmd) { return $cmd.Source }
    foreach ($candidate in @(
        "$env:LOCALAPPDATA\go\bin\go.exe",
        "$env:ProgramFiles\Go\bin\go.exe",
        "$env:USERPROFILE\go\bin\go.exe",
        "C:\Go\bin\go.exe")) {
        if (Test-Path $candidate) { return $candidate }
    }
    throw "Go was not found. Install it from https://go.dev/dl/ (or put go.exe on your PATH)."
}
$go = Find-Go
Write-Host "==> Using $go"

$version = (git -C $repo describe --tags --always --dirty 2>$null)
if (-not $version) { $version = "dev" }

Write-Host "==> Building rnv3 $version for linux/$Arch"
$env:GOOS = "linux"; $env:GOARCH = $Arch; $env:CGO_ENABLED = "0"
New-Item -ItemType Directory -Force "$repo\dist" | Out-Null
& $go build -C $repo -trimpath -ldflags "-s -w -X main.version=$version" -o "$repo\dist\rnv3" ./cmd/rnv3
if ($LASTEXITCODE -ne 0) { throw "build failed" }
& $go build -C $repo -trimpath -ldflags "-s -w" -o "$repo\dist\rnv3-migrate" ./tools/migrate
if ($LASTEXITCODE -ne 0) { throw "migrate build failed" }
Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED

$target = "${PiUser}@${PiHost}"
Write-Host "==> Copying to ${target}:/tmp/rnv3-deploy"
ssh $target "rm -rf /tmp/rnv3-deploy && mkdir -p /tmp/rnv3-deploy/deploy"
if ($LASTEXITCODE -ne 0) { throw "ssh failed" }
scp "$repo\dist\rnv3" "$repo\dist\rnv3-migrate" "$repo\config.example.yaml" "${target}:/tmp/rnv3-deploy/"
scp "$repo\deploy\install.sh" "$repo\deploy\cutover.sh" "$repo\deploy\rnv3.service" "${target}:/tmp/rnv3-deploy/deploy/"
if ($LASTEXITCODE -ne 0) { throw "scp failed" }
# scp from Windows carries no executable bit.
ssh $target "chmod +x /tmp/rnv3-deploy/deploy/*.sh"
if ($LASTEXITCODE -ne 0) { throw "chmod failed" }

if ($NoInstall) {
    Write-Host "==> Copied. Install with: ssh $target 'cd /tmp/rnv3-deploy && ./deploy/install.sh $InstallArgs ./rnv3'"
    exit 0
}

Write-Host "==> Installing on the Pi"
ssh -t $target "cd /tmp/rnv3-deploy && ./deploy/install.sh $InstallArgs ./rnv3"
if ($LASTEXITCODE -ne 0) { throw "install failed" }

Write-Host "==> Deployed $version. Log: ssh $target journalctl -u rnv3 -f"
