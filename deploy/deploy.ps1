# Cross-compile rnv3 for the Pi and deploy it over SSH.
# Usage: .\deploy\deploy.ps1 [-PiHost raspinoaa] [-PiUser pi] [-Arch arm64]
param(
    [string]$PiHost = "raspinoaa",
    [string]$PiUser = "pi",
    [string]$Arch = "arm64"   # arm64 for Pi 3/4/5 64-bit; amd64 for x64 PCs
)
$ErrorActionPreference = "Stop"
$repo = Split-Path -Parent $PSScriptRoot

$version = (git -C $repo describe --tags --always --dirty 2>$null)
if (-not $version) { $version = "dev" }

Write-Host "==> Building rnv3 $version for linux/$Arch"
$env:GOOS = "linux"; $env:GOARCH = $Arch; $env:CGO_ENABLED = "0"
New-Item -ItemType Directory -Force "$repo\dist" | Out-Null
go build -C $repo -trimpath -ldflags "-s -w -X main.version=$version" -o "$repo\dist\rnv3" ./cmd/rnv3
Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED

Write-Host "==> Copying to ${PiUser}@${PiHost}"
scp "$repo\dist\rnv3" "$repo\deploy\install.sh" "$repo\deploy\rnv3.service" "$repo\config.example.yaml" "${PiUser}@${PiHost}:/tmp/rnv3-deploy/" 2>$null
if ($LASTEXITCODE -ne 0) {
    ssh "${PiUser}@${PiHost}" "mkdir -p /tmp/rnv3-deploy"
    scp "$repo\dist\rnv3" "$repo\deploy\install.sh" "$repo\deploy\rnv3.service" "${PiUser}@${PiHost}:/tmp/rnv3-deploy/"
    scp "$repo\config.example.yaml" "${PiUser}@${PiHost}:/tmp/rnv3-deploy/"
}

Write-Host "==> Installing on the Pi"
# install.sh expects config.example.yaml one level above itself
ssh "${PiUser}@${PiHost}" "cd /tmp/rnv3-deploy && mkdir -p deploy && cp install.sh rnv3.service deploy/ && chmod +x deploy/install.sh && ./deploy/install.sh ./rnv3"

Write-Host "==> Deployed. Panel: http://${PiHost}/"
