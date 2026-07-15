param(
    [string]$InstallDir = $env:BURNBAN_INSTALL_DIR,
    [string]$DesktopDir = "",
    [string]$StartMenuDir = "",
    [string]$StartupDir = "",
    [switch]$NoDesktop,
    [switch]$NoAutostart,
    [switch]$NoLaunch,
    [switch]$NoPath,
    [switch]$Uninstall,
    [switch]$Purge
)

$ErrorActionPreference = "Stop"
$Repo = "burnban/burnban"
$Temp = $null
$StagedBinary = $null
$BackupBinary = $null

if ($env:BURNBAN_CREATE_AUTOSTART -eq "0") { $NoAutostart = $true }
if ($env:BURNBAN_LAUNCH_AFTER_INSTALL -eq "0") { $NoLaunch = $true }

if ($Purge -and -not $Uninstall) {
    throw "-Purge is only valid with -Uninstall"
}
if ([string]::IsNullOrWhiteSpace($InstallDir)) {
    $InstallDir = Join-Path $env:LOCALAPPDATA "Programs\Burnban"
}
if ([string]::IsNullOrWhiteSpace($DesktopDir)) {
    $DesktopDir = [Environment]::GetFolderPath("Desktop")
}
if ([string]::IsNullOrWhiteSpace($StartMenuDir)) {
    $StartMenuDir = Join-Path ([Environment]::GetFolderPath("ApplicationData")) "Microsoft\Windows\Start Menu\Programs"
}
if ([string]::IsNullOrWhiteSpace($StartupDir)) {
    $StartupDir = Join-Path ([Environment]::GetFolderPath("ApplicationData")) "Microsoft\Windows\Start Menu\Programs\Startup"
}

$StateDir = $env:BURNBAN_INSTALL_STATE_DIR
if ([string]::IsNullOrWhiteSpace($StateDir)) {
    $StateDir = Join-Path $env:LOCALAPPDATA "Burnban"
}
$ManifestPath = Join-Path $StateDir "install-manifest.json"
$DataDir = $env:BURNBAN_PURGE_DIR
if ([string]::IsNullOrWhiteSpace($DataDir)) {
    $DataDir = Join-Path ([Environment]::GetFolderPath("UserProfile")) ".burnban"
}
$DataMarker = Join-Path $DataDir ".burnban-installer-data"

$Binary = Join-Path $InstallDir "burnban.exe"
$DesktopShortcut = Join-Path $DesktopDir "Burnban.lnk"
$StartShortcut = Join-Path $StartMenuDir "Burnban.lnk"
$StartupShortcut = Join-Path $StartupDir "Burnban Meter.lnk"

function Test-BurnbanBinary([string]$Path) {
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) { return $false }
    try {
        $Output = & $Path version 2>$null
        return ($LASTEXITCODE -eq 0 -and "$Output" -match '^burnban\s')
    } catch {
        Write-Warning "Could not launch Burnban binary at ${Path}: $($_.Exception.Message)"
        return $false
    }
}

function Get-ShortcutTarget([string]$Path) {
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) { return $null }
    try {
        $Shell = New-Object -ComObject WScript.Shell
        return $Shell.CreateShortcut($Path).TargetPath
    } catch {
        return $null
    }
}

function Test-SamePath([string]$Left, [string]$Right) {
    if ([string]::IsNullOrWhiteSpace($Left) -or [string]::IsNullOrWhiteSpace($Right)) { return $false }
    try {
        $L = [IO.Path]::GetFullPath($Left).TrimEnd('\')
        $R = [IO.Path]::GetFullPath($Right).TrimEnd('\')
        return [StringComparer]::OrdinalIgnoreCase.Equals($L, $R)
    } catch {
        return $false
    }
}

function Remove-ManagedShortcut([string]$Path, [string]$ExpectedTarget) {
    if ([string]::IsNullOrWhiteSpace($Path)) { return }
    if (-not (Test-Path -LiteralPath $Path)) { return }
    $Target = Get-ShortcutTarget $Path
    if (Test-SamePath $Target $ExpectedTarget) {
        Remove-Item -LiteralPath $Path -Force
    } else {
        Write-Warning "Leaving shortcut that is not owned by this Burnban install: $Path"
    }
}

function Remove-ManagedUserPath([string]$Path, [bool]$WasAdded) {
    if (-not $WasAdded -or [string]::IsNullOrWhiteSpace($Path)) { return }
    $UserPath = [Environment]::GetEnvironmentVariable("Path", "User")
    $Parts = @($UserPath -split ';' | Where-Object {
        -not [string]::IsNullOrWhiteSpace($_) -and -not (Test-SamePath $_ $Path)
    })
    [Environment]::SetEnvironmentVariable("Path", ($Parts -join ';'), "User")
    Write-Host "Removed the managed PATH entry for $Path."
}

function Read-InstallManifest {
    if (-not (Test-Path -LiteralPath $ManifestPath -PathType Leaf)) { return $null }
    try {
        $Manifest = Get-Content -LiteralPath $ManifestPath -Raw | ConvertFrom-Json
        if ($Manifest.format -ne 1) { throw "unsupported format" }
        return $Manifest
    } catch {
        throw "Cannot read Burnban install manifest at ${ManifestPath}: $($_.Exception.Message)"
    }
}

function Remove-EmptyDirectory([string]$Path) {
    if (-not (Test-Path -LiteralPath $Path -PathType Container)) { return }
    if (@(Get-ChildItem -LiteralPath $Path -Force).Count -eq 0) {
        Remove-Item -LiteralPath $Path -Force
    }
}

function Remove-BurnbanData {
    $Running = Get-Process -Name "burnban" -ErrorAction SilentlyContinue
    if ($Running) {
        throw "Stop the running Burnban meter before using -Purge"
    }
    if ((Split-Path -Leaf $DataDir) -ne ".burnban") {
        throw "Refusing to purge unexpected data directory: $DataDir"
    }
    if (-not (Test-Path -LiteralPath $DataMarker -PathType Leaf) -or
        (Get-Content -LiteralPath $DataMarker -Raw).Trim() -ne "burnban-installer-data-v1") {
        throw "Refusing to purge unmarked data directory: $DataDir"
    }
    Remove-Item -LiteralPath $DataDir -Recurse -Force
    Write-Host "Data purged: $DataDir"
}

function Write-InstallManifest(
    [string]$OwnedBinary,
    [string]$OwnedInstallDir,
    [string]$OwnedDesktopShortcut,
    [string]$OwnedStartShortcut,
    [string]$OwnedStartupShortcut,
    [bool]$OwnedPathAdded
) {
    New-Item -ItemType Directory -Path $StateDir, $DataDir -Force | Out-Null
    "burnban-installer-data-v1" | Set-Content -LiteralPath $DataMarker -Encoding ascii
    $Manifest = [ordered]@{
        format = 1
        binary = $OwnedBinary
        install_dir = $OwnedInstallDir
        desktop_shortcut = $OwnedDesktopShortcut
        start_shortcut = $OwnedStartShortcut
        startup_shortcut = $OwnedStartupShortcut
        path_added = $OwnedPathAdded
    }
    $Manifest | ConvertTo-Json | Set-Content -LiteralPath $ManifestPath -Encoding utf8
}

if ($Uninstall) {
    $Manifest = Read-InstallManifest
    $RecordedBinary = $Binary
    $RecordedInstallDir = $InstallDir
    $RecordedDesktop = $DesktopShortcut
    $RecordedStart = $StartShortcut
    $RecordedStartup = $StartupShortcut
    $PathWasAdded = $false

    if ($null -ne $Manifest) {
        $RecordedBinary = [string]$Manifest.binary
        $RecordedInstallDir = [string]$Manifest.install_dir
        $RecordedDesktop = [string]$Manifest.desktop_shortcut
        $RecordedStart = [string]$Manifest.start_shortcut
        $RecordedStartup = [string]$Manifest.startup_shortcut
        $PathWasAdded = [bool]$Manifest.path_added
    } else {
        Write-Warning "No install manifest found; using conservative legacy cleanup."
    }

    $Incomplete = $false
    $ValidBinary = $false
    if (Test-Path -LiteralPath $RecordedBinary) {
        if (Test-BurnbanBinary $RecordedBinary) {
            $ValidBinary = $true
        } else {
            Write-Warning "Refusing to remove a file that is not a Burnban binary: $RecordedBinary"
            $Incomplete = $true
        }
    }

    if ($ValidBinary) {
        & $RecordedBinary stop *> $null
    }
    if ($Purge -and (Get-Process -Name "burnban" -ErrorAction SilentlyContinue)) {
        throw "Stop the running Burnban meter before using -Purge"
    }
    Remove-ManagedShortcut $RecordedStartup $RecordedBinary

    if ($ValidBinary) {
        try {
            Remove-Item -LiteralPath $RecordedBinary -Force
        } catch {
            Write-Warning "Could not remove ${RecordedBinary}: $($_.Exception.Message)"
            $Incomplete = $true
        }
    }

    Remove-ManagedShortcut $RecordedDesktop $RecordedBinary
    Remove-ManagedShortcut $RecordedStart $RecordedBinary
    Remove-ManagedUserPath $RecordedInstallDir $PathWasAdded
    Remove-EmptyDirectory $RecordedInstallDir

    if ($Incomplete) {
        throw "Uninstall is incomplete; the install manifest was retained at $ManifestPath"
    }

    if ($Purge) {
        Remove-BurnbanData
    } else {
        Write-Host "Data retained: $DataDir (use -Uninstall -Purge to remove it)."
    }
    Remove-Item -LiteralPath $ManifestPath -Force -ErrorAction SilentlyContinue
    Remove-EmptyDirectory $StateDir
    Write-Host "Burnban removed." -ForegroundColor Green
    exit 0
}

$ExistingManifest = Read-InstallManifest
if ($null -ne $ExistingManifest -and -not (Test-SamePath ([string]$ExistingManifest.binary) $Binary)) {
    throw "Burnban is already recorded at $($ExistingManifest.binary). Uninstall it before changing InstallDir."
}
if ($null -ne $ExistingManifest) {
    if (-not [string]::IsNullOrWhiteSpace([string]$ExistingManifest.desktop_shortcut)) {
        $DesktopShortcut = [string]$ExistingManifest.desktop_shortcut
    }
    if (-not [string]::IsNullOrWhiteSpace([string]$ExistingManifest.start_shortcut)) {
        $StartShortcut = [string]$ExistingManifest.start_shortcut
    }
    if (-not [string]::IsNullOrWhiteSpace([string]$ExistingManifest.startup_shortcut)) {
        $StartupShortcut = [string]$ExistingManifest.startup_shortcut
    }
}
if ((Test-Path -LiteralPath $Binary) -and -not (Test-BurnbanBinary $Binary)) {
    throw "Refusing to overwrite a non-Burnban file: $Binary"
}
if (-not $NoDesktop) {
    foreach ($ShortcutPath in @($DesktopShortcut, $StartShortcut)) {
        if (Test-Path -LiteralPath $ShortcutPath) {
            $ExistingTarget = Get-ShortcutTarget $ShortcutPath
            if (-not (Test-SamePath $ExistingTarget $Binary)) {
                throw "Refusing to overwrite an unrecognized shortcut: $ShortcutPath"
            }
        }
    }
}
if (-not $NoAutostart -and (Test-Path -LiteralPath $StartupShortcut)) {
    $ExistingTarget = Get-ShortcutTarget $StartupShortcut
    if (-not (Test-SamePath $ExistingTarget $Binary)) {
        throw "Refusing to overwrite an unrecognized login-start shortcut: $StartupShortcut"
    }
}

$Architecture = $env:PROCESSOR_ARCHITECTURE
if ($env:PROCESSOR_ARCHITEW6432) { $Architecture = $env:PROCESSOR_ARCHITEW6432 }
switch ($Architecture.ToUpperInvariant()) {
    "AMD64" { $Arch = "amd64" }
    "ARM64" { $Arch = "arm64" }
    default { throw "Unsupported Windows architecture: $Architecture" }
}

$Archive = "burnban_windows_$Arch.zip"
$BaseUrl = $env:BURNBAN_DOWNLOAD_BASE_URL
if ([string]::IsNullOrWhiteSpace($BaseUrl)) {
    $BaseUrl = "https://github.com/$Repo/releases/latest/download"
}
$Temp = Join-Path ([IO.Path]::GetTempPath()) ("burnban-install-" + [Guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Path $Temp | Out-Null

function Get-BurnbanArtifact([string]$Name, [string]$Destination) {
    if (Test-Path -LiteralPath $BaseUrl -PathType Container) {
        Copy-Item (Join-Path $BaseUrl $Name) $Destination
        return
    }
    $Uri = [Uri]($BaseUrl.TrimEnd('/') + "/" + $Name)
    if ($Uri.Scheme -ne "https") { throw "Remote release downloads must use HTTPS: $Uri" }
    $Response = Invoke-WebRequest -UseBasicParsing -Uri $Uri -OutFile $Destination -PassThru
    # Windows PowerShell exposes ResponseUri; PowerShell 7 exposes the final
    # RequestUri. Validate the redirect destination as well as the initial URL.
    $FinalUri = $null
    if ($null -ne $Response.BaseResponse.ResponseUri) {
        $FinalUri = [Uri]$Response.BaseResponse.ResponseUri
    } elseif ($null -ne $Response.BaseResponse.RequestMessage) {
        $FinalUri = [Uri]$Response.BaseResponse.RequestMessage.RequestUri
    }
    if ($null -eq $FinalUri -or $FinalUri.Scheme -ne "https") {
        throw "Remote release redirect did not end on HTTPS: $FinalUri"
    }
}

try {
    Write-Host "Downloading Burnban for Windows/$Arch..." -ForegroundColor Yellow
    $Zip = Join-Path $Temp $Archive
    $Checksums = Join-Path $Temp "checksums.txt"
    Get-BurnbanArtifact $Archive $Zip
    Get-BurnbanArtifact "checksums.txt" $Checksums

    $ChecksumLine = Get-Content $Checksums | Where-Object { $_ -match ("\s" + [Regex]::Escape($Archive) + "$") } | Select-Object -First 1
    if (-not $ChecksumLine) { throw "$Archive is missing from release checksums" }
    $Expected = ($ChecksumLine -split '\s+')[0].ToLowerInvariant()
    $Actual = (Get-FileHash -Algorithm SHA256 $Zip).Hash.ToLowerInvariant()
    if ($Actual -ne $Expected) { throw "Checksum verification failed for $Archive" }

    $Extracted = Join-Path $Temp "extracted"
    Expand-Archive -Path $Zip -DestinationPath $Extracted -Force
    New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
    $StagedBinary = Join-Path $InstallDir (".burnban-stage-" + [Guid]::NewGuid().ToString("N") + ".exe")
    Copy-Item (Join-Path $Extracted "burnban.exe") $StagedBinary
    # This happens only after the archive matches the published SHA-256.
    Unblock-File $StagedBinary -ErrorAction SilentlyContinue
    if (-not (Test-BurnbanBinary $StagedBinary)) {
        throw "Downloaded binary did not pass its version check; the existing install was retained"
    }
    # Recheck immediately before replacement, then use same-directory file
    # replacement so a failed upgrade cannot truncate a working executable.
    if ((Test-Path -LiteralPath $Binary) -and -not (Test-BurnbanBinary $Binary)) {
        throw "Refusing to overwrite a non-Burnban file: $Binary"
    }
    if (Test-Path -LiteralPath $Binary) {
        $BackupBinary = Join-Path $InstallDir (".burnban-backup-" + [Guid]::NewGuid().ToString("N") + ".exe")
        [IO.File]::Replace($StagedBinary, $Binary, $BackupBinary, $true)
    } else {
        [IO.File]::Move($StagedBinary, $Binary)
    }
    $StagedBinary = $null
    if (-not (Test-BurnbanBinary $Binary)) {
        if ($null -ne $BackupBinary -and (Test-Path -LiteralPath $BackupBinary)) {
            try {
                [IO.File]::Replace($BackupBinary, $Binary, $null, $true)
            } catch {
                $RetainedBackup = $BackupBinary
                $BackupBinary = $null
                throw "Installed binary validation failed and rollback failed; the preceding version remains at ${RetainedBackup}: $($_.Exception.Message)"
            }
            $BackupBinary = $null
            throw "Installed binary did not pass its version check; the preceding version was restored"
        }
        Remove-Item -LiteralPath $Binary -Force -ErrorAction SilentlyContinue
        throw "Installed binary did not pass its version check; the incomplete fresh install was removed"
    }
    if ($null -ne $BackupBinary) {
        Remove-Item -LiteralPath $BackupBinary -Force
        $BackupBinary = $null
    }

    $PathAdded = $false
    if ($null -ne $ExistingManifest) { $PathAdded = [bool]$ExistingManifest.path_added }
    $CreatedDesktop = ""
    $CreatedStart = ""
    $CreatedStartup = ""
    if ($null -ne $ExistingManifest) {
        $CreatedDesktop = [string]$ExistingManifest.desktop_shortcut
        $CreatedStart = [string]$ExistingManifest.start_shortcut
        $CreatedStartup = [string]$ExistingManifest.startup_shortcut
    }
    Write-InstallManifest $Binary $InstallDir $CreatedDesktop $CreatedStart $CreatedStartup $PathAdded
    if (-not $NoPath) {
        $UserPath = [Environment]::GetEnvironmentVariable("Path", "User")
        $Parts = @($UserPath -split ';' | Where-Object { -not [string]::IsNullOrWhiteSpace($_) })
        if (-not ($Parts | Where-Object { Test-SamePath $_ $InstallDir })) {
            [Environment]::SetEnvironmentVariable("Path", (($Parts + $InstallDir) -join ';'), "User")
            $PathAdded = $true
            Write-Host "Added $InstallDir to your user PATH (new terminals)."
        }
        if (-not (($env:Path -split ';') | Where-Object { Test-SamePath $_ $InstallDir })) {
            $env:Path += ";$InstallDir"
        }
    }
    Write-InstallManifest $Binary $InstallDir $CreatedDesktop $CreatedStart $CreatedStartup $PathAdded

    if (-not $NoDesktop) {
        New-Item -ItemType Directory -Path (Split-Path -Parent $DesktopShortcut) -Force | Out-Null
        New-Item -ItemType Directory -Path (Split-Path -Parent $StartShortcut) -Force | Out-Null
        $Shell = New-Object -ComObject WScript.Shell
        $CreatedDesktop = $DesktopShortcut
        $CreatedStart = $StartShortcut
        Write-InstallManifest $Binary $InstallDir $CreatedDesktop $CreatedStart $CreatedStartup $PathAdded
        foreach ($ShortcutPath in @($DesktopShortcut, $StartShortcut)) {
            $Shortcut = $Shell.CreateShortcut($ShortcutPath)
            $Shortcut.TargetPath = $Binary
            $Shortcut.Arguments = "desktop"
            $Shortcut.WorkingDirectory = $InstallDir
            $Shortcut.IconLocation = "$Binary,0"
            $Shortcut.Description = "Open the Burnban agent usage dashboard"
            $Shortcut.Save()
        }
    }

    if (-not $NoAutostart) {
        New-Item -ItemType Directory -Path (Split-Path -Parent $StartupShortcut) -Force | Out-Null
        $Shell = New-Object -ComObject WScript.Shell
        $CreatedStartup = $StartupShortcut
        Write-InstallManifest $Binary $InstallDir $CreatedDesktop $CreatedStart $CreatedStartup $PathAdded
        $Shortcut = $Shell.CreateShortcut($StartupShortcut)
        $Shortcut.TargetPath = $Binary
        $Shortcut.Arguments = "serve"
        $Shortcut.WorkingDirectory = $InstallDir
        $Shortcut.IconLocation = "$Binary,0"
        $Shortcut.Description = "Start the Burnban meter at login"
        $Shortcut.WindowStyle = 7
        $Shortcut.Save()
        Write-Host "Starts at login: $StartupShortcut"
    }

    Write-InstallManifest $Binary $InstallDir $CreatedDesktop $CreatedStart $CreatedStartup $PathAdded

    $Version = & $Binary version
    Write-Host "Installed: $Version" -ForegroundColor Green
    Write-Host ""
    $InteractiveSetup = -not [Console]::IsInputRedirected -and -not [Console]::IsOutputRedirected
    if ($InteractiveSetup) {
        & $Binary setup --if-needed --no-launch
        $SetupExitCode = $LASTEXITCODE
        if ($SetupExitCode -ne 0) {
            Write-Warning "Guided setup paused; finish later with: burnban setup"
        } elseif (-not $NoLaunch) {
            Write-Host "Starting Burnban..."
            & $Binary status *> $null
            $MeterReady = $LASTEXITCODE -eq 0
            if (-not $MeterReady) {
                $StartupOut = Join-Path $StateDir "startup.out.log"
                $StartupErr = Join-Path $StateDir "startup.err.log"
                Start-Process -FilePath $Binary -ArgumentList @("serve") -WindowStyle Hidden `
                    -RedirectStandardOutput $StartupOut -RedirectStandardError $StartupErr | Out-Null
                for ($Attempt = 0; $Attempt -lt 50 -and -not $MeterReady; $Attempt++) {
                    Start-Sleep -Milliseconds 100
                    & $Binary status *> $null
                    $MeterReady = $LASTEXITCODE -eq 0
                }
            }
            if ($MeterReady) {
                Write-Host "Meter running: http://localhost:4141"
                Start-Process -FilePath $Binary -WindowStyle Hidden | Out-Null
            } else {
                Write-Warning "The meter did not become healthy; inspect $StateDir\startup.err.log"
            }
        }
    } else {
        Write-Host "Get started:"
        Write-Host "  burnban setup   guided setup"
        Write-Host "  burnban guide   what Burnban does, in plain language"
    }
} finally {
    if ($null -ne $StagedBinary) {
        Remove-Item -LiteralPath $StagedBinary -Force -ErrorAction SilentlyContinue
    }
    if ($null -ne $BackupBinary -and (Test-Path -LiteralPath $BackupBinary)) {
        Write-Warning "An interrupted upgrade left the preceding executable at $BackupBinary"
    }
    if ($null -ne $Temp) {
        Remove-Item $Temp -Recurse -Force -ErrorAction SilentlyContinue
    }
}
