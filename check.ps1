#!/usr/bin/env pwsh
# Standalone gate for localgateway-go.
# Run from any directory; all commands execute with this repository as GOWORK root.
[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'

$repoRoot = Split-Path -Parent $MyInvocation.MyCommand.Path
Push-Location -LiteralPath $repoRoot
try {
    $go = Get-Command go -ErrorAction Stop
    $gofmt = Get-Command gofmt -ErrorAction Stop

    $previousGoWork = $env:GOWORK
    $env:GOWORK = 'off'
    try {
        $unformatted = @(& $gofmt.Source -l .)
        if ($LASTEXITCODE -ne 0) { throw "gofmt failed with exit code $LASTEXITCODE." }
        if ($unformatted.Count -gt 0) {
            throw ("gofmt found unformatted files:`n  " + ($unformatted -join "`n  "))
        }

        & $go.Source vet ./...
        if ($LASTEXITCODE -ne 0) { throw "go vet failed with exit code $LASTEXITCODE." }

        & $go.Source test ./...
        if ($LASTEXITCODE -ne 0) { throw "go test failed with exit code $LASTEXITCODE." }

        & $go.Source build ./...
        if ($LASTEXITCODE -ne 0) { throw "go build failed with exit code $LASTEXITCODE." }
    }
    finally {
        $env:GOWORK = $previousGoWork
    }
}
finally {
    Pop-Location
}
