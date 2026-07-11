param(
    [string]$InstallDir = $env:BURNBAN_INSTALL_DIR,
    [string]$DesktopDir = "",
    [string]$StartMenuDir = "",
    [switch]$NoDesktop,
    [switch]$NoPath,
    [switch]$Uninstall
)

$ErrorActionPreference = "Stop"
$Repo = "burnban/burnban"

if ([string]::IsNullOrWhiteSpace($InstallDir)) {
    $InstallDir = Join-Path $env:LOCALAPPDATA "Programs\Burnban"
}
if ([string]::IsNullOrWhiteSpace($DesktopDir)) {
    $DesktopDir = [Environment]::GetFolderPath("Desktop")
}
if ([string]::IsNullOrWhiteSpace($StartMenuDir)) {
    $StartMenuDir = Join-Path ([Environment]::GetFolderPath("ApplicationData")) "Microsoft\Windows\Start Menu\Programs"
}
$Binary = Join-Path $InstallDir "burnban.exe"
$DesktopShortcut = Join-Path $DesktopDir "Burnban.lnk"
$StartShortcut = Join-Path $StartMenuDir "Burnban.lnk"

if ($Uninstall) {
    Remove-Item $DesktopShortcut -Force -ErrorAction SilentlyContinue
    Remove-Item $StartShortcut -Force -ErrorAction SilentlyContinue
    Remove-Item $InstallDir -Recurse -Force -ErrorAction SilentlyContinue
    Write-Host "Burnban removed." -ForegroundColor Green
    exit 0
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
    } else {
        Invoke-WebRequest -UseBasicParsing -Uri ($BaseUrl.TrimEnd('/') + "/" + $Name) -OutFile $Destination
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
    Copy-Item (Join-Path $Extracted "burnban.exe") $Binary -Force
    Unblock-File $Binary -ErrorAction SilentlyContinue

    if (-not $NoPath) {
        $UserPath = [Environment]::GetEnvironmentVariable("Path", "User")
        $Parts = @($UserPath -split ';' | Where-Object { $_ })
        if ($Parts -notcontains $InstallDir) {
            $NewPath = (($Parts + $InstallDir) -join ';')
            [Environment]::SetEnvironmentVariable("Path", $NewPath, "User")
            Write-Host "Added $InstallDir to your user PATH (new terminals)."
        }
        if (($env:Path -split ';') -notcontains $InstallDir) { $env:Path += ";$InstallDir" }
    }

    if (-not $NoDesktop) {
        New-Item -ItemType Directory -Path $DesktopDir -Force | Out-Null
        New-Item -ItemType Directory -Path $StartMenuDir -Force | Out-Null
        $Shell = New-Object -ComObject WScript.Shell
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

    $Version = & $Binary version
    Write-Host "Installed: $Version" -ForegroundColor Green
    Write-Host "Desktop: double-click Burnban"
    Write-Host "Terminal: burnban serve   |   burnban subsidy"
} finally {
    Remove-Item $Temp -Recurse -Force -ErrorAction SilentlyContinue
}
