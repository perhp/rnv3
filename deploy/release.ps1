# Thin wrapper for the release tool: finds Go (even when it is not on PATH)
# and runs `go run ./tools/release` with whatever arguments you pass.
#
#   .\deploy\release.ps1                        # dist\rnv3-setup.exe for this PC
#   .\deploy\release.ps1 -target darwin/arm64   # the setup tool for a Mac
#   .\deploy\release.ps1 -arch amd64            # Pi binaries for an x64 station
#
# All build logic lives in tools/release/main.go.
$ErrorActionPreference = "Stop"
$repo = Split-Path -Parent $PSScriptRoot

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

# PowerShell parameters are case-insensitive (-Arch, -Target); Go's flag parser
# is not. Lowercase flag names before forwarding; values are passed untouched.
$forward = @(foreach ($a in $args) {
    if ($a -is [string] -and $a -match '^-{1,2}[A-Za-z][A-Za-z-]*(=.*)?$') {
        $name, $value = $a -split '=', 2
        if ($null -ne $value) { "$($name.ToLower())=$value" } else { $name.ToLower() }
    } else { $a }
})

& (Find-Go) run -C $repo ./tools/release @forward
exit $LASTEXITCODE
