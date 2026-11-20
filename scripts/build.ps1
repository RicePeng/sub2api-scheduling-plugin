param(
    [Parameter(Mandatory = $true)][string]$PrivateKey,
    [string]$Version = "0.1.1",
    [string]$HostVersion = "0.2.23",
    [string]$KeyId = "local-cache-rate-manager-v1",
    [string]$Targets = "linux/amd64,linux/arm64,windows/amd64",
    [string]$Out = "",
    [string]$Go = ""
)

$ErrorActionPreference = "Stop"
$projectRoot = Split-Path -Parent $PSScriptRoot
$PrivateKey = [System.IO.Path]::GetFullPath($PrivateKey)
if (-not $Go) {
    $goCommand = Get-Command go -ErrorAction SilentlyContinue
    if ($goCommand) {
        $Go = $goCommand.Source
    } else {
        $Go = Join-Path (Split-Path -Parent $projectRoot) ".tools/go/bin/go.exe"
        if (-not (Test-Path -LiteralPath $Go -PathType Leaf)) {
            throw "Go not found. Pass -Go with the compiler executable path."
        }
    }
}
$arguments = @("run", "./cmd/packager", "build", "-source", $projectRoot,
    "-private-key", $PrivateKey, "-version", $Version, "-host-version", $HostVersion,
    "-key-id", $KeyId, "-targets", $Targets, "-go", $Go)
if ($Out) {
    $arguments += @("-out", $Out)
}
Push-Location $projectRoot
try {
    & $Go @arguments
    if ($LASTEXITCODE -ne 0) {
        throw "Package build failed (exit $LASTEXITCODE)."
    }
} finally {
    Pop-Location
}
