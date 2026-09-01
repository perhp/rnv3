# Build the release set: linux/arm64 rnv3 + rnv3-migrate, and rnv3-setup for
# this PC with those binaries and the deploy scripts embedded.
#
#   .\deploy\release.ps1 [-Arch arm64]
#
# Output in dist\: rnv3, rnv3-migrate (Pi), rnv3-setup.exe (PC).
param(
    [string]$Arch = "arm64"
)
$ErrorActionPreference = "Stop"
$repo = Split-Path -Parent $PSScriptRoot
$version = (git -C $repo describe --tags --always --dirty 2>$null)
if (-not $version) { $version = "dev" }
$dist = "$repo\dist"
$payload = "$repo\cmd\rnv3-setup\payload"
New-Item -ItemType Directory -Force $dist | Out-Null

Write-Host "==> Building rnv3 + rnv3-migrate $version for linux/$Arch"
$env:GOOS = "linux"; $env:GOARCH = $Arch; $env:CGO_ENABLED = "0"
go build -C $repo -trimpath -ldflags "-s -w -X main.version=$version" -o "$dist\rnv3" ./cmd/rnv3
if ($LASTEXITCODE -ne 0) { throw "rnv3 build failed" }
go build -C $repo -trimpath -ldflags "-s -w" -o "$dist\rnv3-migrate" ./tools/migrate
if ($LASTEXITCODE -ne 0) { throw "rnv3-migrate build failed" }
Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED

Write-Host "==> Assembling the rnv3-setup payload"
Copy-Item "$dist\rnv3" "$payload\rnv3" -Force
Copy-Item "$dist\rnv3-migrate" "$payload\rnv3-migrate" -Force
Copy-Item "$repo\deploy\install.sh", "$repo\deploy\cutover.sh", "$repo\deploy\rnv3.service", "$repo\config.example.yaml" $payload -Force

Write-Host "==> Building rnv3-setup $version for this PC"
go build -C $repo -trimpath -ldflags "-s -w -X main.version=$version" -o "$dist\rnv3-setup.exe" ./cmd/rnv3-setup
if ($LASTEXITCODE -ne 0) { throw "rnv3-setup build failed" }

Write-Host "==> Done:"
Get-ChildItem $dist | ForEach-Object { Write-Host ("    {0,-18} {1,10:N0} bytes" -f $_.Name, $_.Length) }
Write-Host "Run: .\dist\rnv3-setup.exe"
