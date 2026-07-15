$ErrorActionPreference = "Stop"
$Root = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path

if ([string]::IsNullOrWhiteSpace($env:BURNBAN_RELEASE_DIR)) {
    if (-not (Get-Command goreleaser -ErrorAction SilentlyContinue)) {
        throw "Set BURNBAN_RELEASE_DIR or install goreleaser"
    }
    Push-Location $Root
    try { & goreleaser release --snapshot --clean } finally { Pop-Location }
    $Release = Join-Path $Root "dist"
} else {
    $Release = (Resolve-Path $env:BURNBAN_RELEASE_DIR).Path
}

$Temp = Join-Path ([IO.Path]::GetTempPath()) ("burnban-smoke-" + [Guid]::NewGuid().ToString("N"))
$Install = Join-Path $Temp "install"
$Desktop = Join-Path $Temp "desktop"
$StartMenu = Join-Path $Temp "start-menu"
$Startup = Join-Path $Temp "startup"
$LocalAppData = Join-Path $Temp "local-app-data"
$DataDir = Join-Path (Join-Path $Temp "home") ".burnban"
$OriginalUserPath = [Environment]::GetEnvironmentVariable("Path", "User")
$OriginalLocalAppData = $env:LOCALAPPDATA
$OriginalStateDir = $env:BURNBAN_INSTALL_STATE_DIR
$OriginalPurgeDir = $env:BURNBAN_PURGE_DIR
$OriginalReleaseDir = $env:BURNBAN_RELEASE_DIR
$ServeProcess = $null

New-Item -ItemType Directory -Path $Install, $Desktop, $StartMenu, $Startup, $LocalAppData, $DataDir -Force | Out-Null
"keep" | Set-Content (Join-Path $Install "unrelated.keep") -Encoding ascii

$InstallerSource = Get-Content (Join-Path $Root "install.ps1") -Raw
if ($InstallerSource -match '\.burnban-install-') {
    throw "Windows installer uses an elevation-prone staging filename"
}
if ($InstallerSource -notmatch '\.burnban-stage-') {
    throw "Windows installer is missing the neutral staging filename"
}

try {
    $Architecture = $env:PROCESSOR_ARCHITECTURE
    if ($env:PROCESSOR_ARCHITEW6432) { $Architecture = $env:PROCESSOR_ARCHITEW6432 }
    switch ($Architecture.ToUpperInvariant()) {
        "AMD64" { $Arch = "amd64" }
        "ARM64" { $Arch = "arm64" }
        default { throw "Unsupported Windows architecture: $Architecture" }
    }
    $Archive = "burnban_windows_$Arch.zip"
    if (-not (Test-Path (Join-Path $Release $Archive))) { throw "Missing release artifact: $Archive" }
    if (-not (Test-Path (Join-Path $Release "checksums.txt"))) { throw "Missing checksums.txt" }

    $Inspect = Join-Path $Temp "archive"
    Expand-Archive -Path (Join-Path $Release $Archive) -DestinationPath $Inspect
    foreach ($Required in @("burnban.exe", "LICENSE", "DATA_AND_PRIVACY.md", "EXTERNAL_POLICY.md", "SECURITY.md", "THIRD_PARTY_NOTICES.md", "docs/dashboard.png")) {
        if (-not (Test-Path (Join-Path $Inspect $Required))) { throw "Archive is missing $Required" }
    }
    if (-not (Get-ChildItem (Join-Path $Inspect "third_party_licenses") -Recurse -Filter LICENSE -ErrorAction SilentlyContinue)) {
        throw "Archive is missing generated third-party license texts"
    }

    $env:LOCALAPPDATA = $LocalAppData
    $env:BURNBAN_INSTALL_STATE_DIR = Join-Path $LocalAppData "Burnban"
    $env:BURNBAN_PURGE_DIR = $DataDir
    $env:BURNBAN_DOWNLOAD_BASE_URL = $Release

    & (Join-Path $Root "install.ps1") -InstallDir $Install -DesktopDir $Desktop -StartMenuDir $StartMenu -StartupDir $Startup -NoLaunch
    $Binary = Join-Path $Install "burnban.exe"
    if (-not (Test-Path $Binary)) { throw "Binary was not installed" }
    if (-not (Test-Path (Join-Path $Desktop "Burnban.lnk"))) { throw "Desktop shortcut was not created" }
    if (-not (Test-Path (Join-Path $StartMenu "Burnban.lnk"))) { throw "Start Menu shortcut was not created" }
    if (-not (Test-Path (Join-Path $Startup "Burnban Meter.lnk"))) { throw "Login-start shortcut was not created" }
    if (-not (Test-Path (Join-Path $env:BURNBAN_INSTALL_STATE_DIR "install-manifest.json"))) { throw "Install manifest was not created" }
    if (-not (Test-Path (Join-Path $DataDir ".burnban-installer-data"))) { throw "Data marker was not created" }
    if ((& $Binary version) -notmatch '^burnban ') { throw "Installed binary did not run" }

    # An upgrade with integrations disabled must not orphan integrations from
    # the preceding install; the manifest must continue to own them.
    & (Join-Path $Root "install.ps1") -InstallDir $Install -DesktopDir $Desktop -StartMenuDir $StartMenu -StartupDir $Startup -NoDesktop -NoAutostart -NoPath -NoLaunch
    if (-not (Test-Path (Join-Path $Desktop "Burnban.lnk"))) { throw "Reinstall orphaned the Desktop shortcut" }
    if (-not (Test-Path (Join-Path $StartMenu "Burnban.lnk"))) { throw "Reinstall orphaned the Start Menu shortcut" }
    if (-not (Test-Path (Join-Path $Startup "Burnban Meter.lnk"))) { throw "Reinstall orphaned the login-start shortcut" }
    $ManagedPathEntries = @([Environment]::GetEnvironmentVariable("Path", "User") -split ';' | Where-Object {
        [StringComparer]::OrdinalIgnoreCase.Equals($_.TrimEnd('\'), $Install.TrimEnd('\'))
    })
    if ($ManagedPathEntries.Count -ne 1) { throw "Reinstall lost or duplicated the managed PATH entry" }
    if (Get-ChildItem -LiteralPath $Install -Filter ".burnban-*.exe" -ErrorAction SilentlyContinue) {
        throw "Atomic reinstall left a staging or backup executable behind"
    }

    # Exercise checksum-valid staging validation: an invalid replacement must
    # fail without changing the preceding executable or leaving recovery files.
    $InvalidUpgrade = Join-Path $Temp "invalid-upgrade-release"
    $InvalidPayload = Join-Path $Temp "invalid-upgrade-payload"
    New-Item -ItemType Directory -Path $InvalidUpgrade, $InvalidPayload | Out-Null
    "not a Burnban executable" | Set-Content (Join-Path $InvalidPayload "burnban.exe") -Encoding ascii
    $InvalidArchive = Join-Path $InvalidUpgrade $Archive
    Compress-Archive -Path (Join-Path $InvalidPayload "*") -DestinationPath $InvalidArchive
    $InvalidHash = (Get-FileHash -Algorithm SHA256 $InvalidArchive).Hash.ToLowerInvariant()
    "$InvalidHash  $Archive" | Set-Content (Join-Path $InvalidUpgrade "checksums.txt") -Encoding ascii
    $VersionBeforeInvalidUpgrade = (& $Binary version | Out-String).Trim()
    $env:BURNBAN_DOWNLOAD_BASE_URL = $InvalidUpgrade
    $InvalidUpgradeRefused = $false
    try {
        & (Join-Path $Root "install.ps1") -InstallDir $Install -DesktopDir $Desktop -StartMenuDir $StartMenu -StartupDir $Startup -NoDesktop -NoAutostart -NoPath -NoLaunch
    } catch {
        if ($_.Exception.Message -notmatch 'existing install was retained') { throw }
        $InvalidUpgradeRefused = $true
    } finally {
        $env:BURNBAN_DOWNLOAD_BASE_URL = $Release
    }
    if (-not $InvalidUpgradeRefused) { throw "Invalid checksum-valid upgrade unexpectedly installed" }
    if ((& $Binary version | Out-String).Trim() -ne $VersionBeforeInvalidUpgrade) {
        throw "Invalid upgrade changed the preceding executable"
    }
    if (Get-ChildItem -LiteralPath $Install -Filter ".burnban-*.exe" -ErrorAction SilentlyContinue) {
        throw "Refused invalid upgrade left a staging or backup executable behind"
    }

    # Exercise the installed Windows executable, the OS-assigned port path,
    # private lifecycle state, status control request, and graceful stop.
    $RuntimeDb = Join-Path $Temp "runtime.db"
    $RuntimeState = "$RuntimeDb.server.json"
    $RuntimeSentinel = Join-Path $Temp "runtime-unrelated.keep"
    $ServeOut = Join-Path $Temp "serve.out.log"
    $ServeErr = Join-Path $Temp "serve.err.log"
    "keep" | Set-Content $RuntimeSentinel -Encoding ascii
    $ServeProcess = Start-Process -FilePath $Binary `
        -ArgumentList @("serve", "--port", "0", "--db", "`"$RuntimeDb`"") `
        -RedirectStandardOutput $ServeOut -RedirectStandardError $ServeErr -PassThru
    for ($Attempt = 0; $Attempt -lt 100 -and -not (Test-Path $RuntimeState); $Attempt++) {
        if ($ServeProcess.HasExited) {
            $Details = ((Get-Content $ServeOut, $ServeErr -ErrorAction SilentlyContinue) -join "`n")
            throw "Installed burnban serve exited before publishing lifecycle state: $Details"
        }
        Start-Sleep -Milliseconds 100
    }
    if (-not (Test-Path $RuntimeState)) { throw "Installed burnban serve did not publish lifecycle state" }
    $Runtime = Get-Content $RuntimeState -Raw | ConvertFrom-Json
    if ([int]$Runtime.pid -ne $ServeProcess.Id) { throw "Lifecycle state PID does not match the installed process" }
    $RuntimeUri = [Uri]$Runtime.url
    if ($RuntimeUri.Port -le 0 -or $Runtime.url -match ':0(?:/|$)') { throw "Port 0 was not replaced by the bound port: $($Runtime.url)" }
    $Health = Invoke-RestMethod -UseBasicParsing -Uri ($Runtime.url.TrimEnd('/') + "/health")
    if (-not $Health.ok) { throw "Installed server health check failed" }
    $StatusOutput = (& $Binary status --db $RuntimeDb 2>&1 | Out-String)
    if ($LASTEXITCODE -ne 0 -or $StatusOutput -notmatch 'is running' -or $StatusOutput -notmatch [Regex]::Escape($Runtime.url)) {
        throw "Installed binary status failed: $StatusOutput"
    }
    $RunningPurgeRefused = $false
    try {
        & (Join-Path $Root "install.ps1") -InstallDir $Install -DesktopDir $Desktop -StartMenuDir $StartMenu -StartupDir $Startup -Uninstall -Purge
    } catch {
        if ($_.Exception.Message -notmatch 'Stop the running Burnban meter') { throw }
        $RunningPurgeRefused = $true
    }
    if (-not $RunningPurgeRefused) { throw "Purge unexpectedly removed a running meter" }
    if (-not (Test-Path $Binary)) { throw "Refused running purge removed the executable" }
    if (-not (Test-Path (Join-Path $env:BURNBAN_INSTALL_STATE_DIR "install-manifest.json"))) {
        throw "Refused running purge removed the install manifest"
    }
    $StopOutput = (& $Binary stop --db $RuntimeDb 2>&1 | Out-String)
    if ($LASTEXITCODE -ne 0 -or $StopOutput -notmatch 'stopped') { throw "Installed binary stop failed: $StopOutput" }
    if (-not $ServeProcess.WaitForExit(15000)) { throw "Installed server did not exit after stop" }
    if (Test-Path $RuntimeState) { throw "Graceful stop left lifecycle state behind" }
    if (-not (Test-Path $RuntimeSentinel)) { throw "Lifecycle commands removed an unrelated sentinel" }

    "ledger" | Set-Content (Join-Path $DataDir "unrelated-data.keep") -Encoding ascii
    & (Join-Path $Root "install.ps1") -InstallDir $Install -DesktopDir $Desktop -StartMenuDir $StartMenu -StartupDir $Startup -Uninstall
    if (Test-Path $Binary) { throw "Uninstall left the binary" }
    if (-not (Test-Path (Join-Path $Install "unrelated.keep"))) { throw "Uninstall removed an unrelated sentinel" }
    if (-not (Test-Path (Join-Path $DataDir "unrelated-data.keep"))) { throw "Normal uninstall removed user data" }
    if (([Environment]::GetEnvironmentVariable("Path", "User") -split ';') -contains $Install) { throw "Uninstall left the managed PATH entry" }
    if (Test-Path (Join-Path $Startup "Burnban Meter.lnk")) { throw "Uninstall left the login-start shortcut" }

    & (Join-Path $Root "install.ps1") -InstallDir $Install -DesktopDir $Desktop -StartMenuDir $StartMenu -StartupDir $Startup -NoDesktop -NoLaunch
    & (Join-Path $Root "install.ps1") -InstallDir $Install -DesktopDir $Desktop -StartMenuDir $StartMenu -StartupDir $Startup -Uninstall -Purge
    if (Test-Path $DataDir) { throw "Purge left the Burnban data directory" }
    if (-not (Test-Path (Join-Path $Install "unrelated.keep"))) { throw "Purge removed an unrelated install sentinel" }

    $Corrupt = Join-Path $Temp "corrupt-release"
    $CorruptInstall = Join-Path $Temp "corrupt-install"
    New-Item -ItemType Directory -Path $Corrupt, $CorruptInstall | Out-Null
    Copy-Item (Join-Path $Release $Archive) (Join-Path $Corrupt $Archive)
    Copy-Item (Join-Path $Release "checksums.txt") (Join-Path $Corrupt "checksums.txt")
    [IO.File]::AppendAllText((Join-Path $Corrupt $Archive), "corrupt")
    $env:BURNBAN_DOWNLOAD_BASE_URL = $Corrupt
    $Failed = $false
    try {
        & (Join-Path $Root "install.ps1") -InstallDir $CorruptInstall -NoDesktop -NoAutostart -NoPath -NoLaunch
    } catch {
        $Failed = $true
    }
    if (-not $Failed) { throw "Corrupt artifact unexpectedly installed" }
    if (Test-Path (Join-Path $CorruptInstall "burnban.exe")) { throw "Corrupt artifact left a binary" }

    Write-Host "installer artifact smoke test passed for windows/$Arch"
} finally {
    if ($null -ne $ServeProcess -and -not $ServeProcess.HasExited) {
        Stop-Process -Id $ServeProcess.Id -Force -ErrorAction SilentlyContinue
        $ServeProcess.WaitForExit(5000) | Out-Null
    }
    [Environment]::SetEnvironmentVariable("Path", $OriginalUserPath, "User")
    $env:LOCALAPPDATA = $OriginalLocalAppData
    $env:BURNBAN_INSTALL_STATE_DIR = $OriginalStateDir
    $env:BURNBAN_PURGE_DIR = $OriginalPurgeDir
    Remove-Item Env:BURNBAN_DOWNLOAD_BASE_URL -ErrorAction SilentlyContinue
    $env:BURNBAN_RELEASE_DIR = $OriginalReleaseDir
    Remove-Item $Temp -Recurse -Force -ErrorAction SilentlyContinue
}
