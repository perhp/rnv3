# Dev fast path: build the Pi binaries, copy them over ssh and run install.sh.
# Thin wrapper around `go run ./tools/release -deploy`.
#
#   .\deploy\deploy.ps1 [-PiHost raspinoaa] [-PiUser pi] [-Arch arm64]
#                       [-InstallArgs "--skip-builds --no-start"] [-NoInstall]
param(
    [string]$PiHost = "raspinoaa",
    [string]$PiUser = "pi",
    [string]$Arch = "arm64",
    [string]$InstallArgs = "",
    [switch]$NoInstall
)
$ErrorActionPreference = "Stop"
$flags = @("-deploy", $PiHost, "-user", $PiUser, "-arch", $Arch)
if ($InstallArgs) { $flags += @("-install-args", $InstallArgs) }
if ($NoInstall) { $flags += "-no-install" }
& "$PSScriptRoot\release.ps1" @flags
exit $LASTEXITCODE
