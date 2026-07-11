$ErrorActionPreference = "Stop"
$Root = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$Temp = Join-Path ([IO.Path]::GetTempPath()) ("burnban-smoke-" + [Guid]::NewGuid().ToString("N"))
$Release = Join-Path $Temp "release"
$Install = Join-Path $Temp "install"
$Desktop = Join-Path $Temp "desktop"
$StartMenu = Join-Path $Temp "start-menu"
New-Item -ItemType Directory -Path $Release, $Desktop, $StartMenu -Force | Out-Null

try {
    $Architecture = $env:PROCESSOR_ARCHITECTURE
    if ($env:PROCESSOR_ARCHITEW6432) { $Architecture = $env:PROCESSOR_ARCHITEW6432 }
    switch ($Architecture.ToUpperInvariant()) {
        "AMD64" { $Arch = "amd64" }
        "ARM64" { $Arch = "arm64" }
        default { throw "Unsupported Windows architecture: $Architecture" }
    }
    $Archive = "burnban_windows_$Arch.zip"
    $env:CGO_ENABLED = "0"
    Push-Location $Root
    try { & go build -trimpath -o (Join-Path $Release "burnban.exe") . } finally { Pop-Location }
    Compress-Archive -Path (Join-Path $Release "burnban.exe") -DestinationPath (Join-Path $Release $Archive)
    $Hash = (Get-FileHash -Algorithm SHA256 (Join-Path $Release $Archive)).Hash.ToLowerInvariant()
    "$Hash  $Archive" | Set-Content (Join-Path $Release "checksums.txt") -Encoding ascii

    $env:BURNBAN_DOWNLOAD_BASE_URL = $Release
    & (Join-Path $Root "install.ps1") -InstallDir $Install -DesktopDir $Desktop -StartMenuDir $StartMenu -NoPath
    if (-not (Test-Path (Join-Path $Install "burnban.exe"))) { throw "binary was not installed" }
    if (-not (Test-Path (Join-Path $Desktop "Burnban.lnk"))) { throw "desktop shortcut was not created" }
    if (-not (Test-Path (Join-Path $StartMenu "Burnban.lnk"))) { throw "Start Menu shortcut was not created" }
    $Version = & (Join-Path $Install "burnban.exe") version
    if ($Version -notmatch '^burnban ') { throw "installed binary did not run" }

    & (Join-Path $Root "install.ps1") -InstallDir $Install -DesktopDir $Desktop -StartMenuDir $StartMenu -Uninstall
    if (Test-Path $Install) { throw "uninstall left the installation directory" }
    Write-Host "installer smoke test passed for windows/$Arch"
} finally {
    Remove-Item Env:BURNBAN_DOWNLOAD_BASE_URL -ErrorAction SilentlyContinue
    Remove-Item $Temp -Recurse -Force -ErrorAction SilentlyContinue
}
