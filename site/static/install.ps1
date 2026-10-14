# Installs the ambit CLI on Windows. https://ambitproject.dev/docs/quick-start
#   irm https://ambitproject.dev/install.ps1 | iex
# $env:AMBIT_VERSION pins a version (default: the latest release). $env:AMBIT_INSTALL_DIR sets the directory
# (default: %LOCALAPPDATA%\ambit\bin). $env:AMBIT_DOWNLOAD_BASE replaces
# https://github.com/craftsman-apps/ambit/releases (for a mirror).
# Output is ASCII only: Windows PowerShell 5.1 can show other characters wrongly when the script comes through iex.

function Install-Ambit {
  $ErrorActionPreference = 'Stop'
  $ProgressPreference = 'SilentlyContinue'
  $base = if ($env:AMBIT_DOWNLOAD_BASE) { $env:AMBIT_DOWNLOAD_BASE } else { 'https://github.com/craftsman-apps/ambit/releases' }
  $url = if ($env:AMBIT_VERSION) { "$base/download/v$($env:AMBIT_VERSION.TrimStart('v'))" } else { "$base/latest/download" }
  $dir = if ($env:AMBIT_INSTALL_DIR) { $env:AMBIT_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA 'ambit\bin' }
  $arch = switch ($env:PROCESSOR_ARCHITECTURE) {
    'AMD64' { 'amd64' }
    'ARM64' { 'arm64' }
    default { throw "there is no ambit build for the processor '$($env:PROCESSOR_ARCHITECTURE)'" }
  }
  $archive = "ambit_windows_$arch.zip"
  $tmp = Join-Path ([IO.Path]::GetTempPath()) ('ambit-' + [Guid]::NewGuid())
  New-Item -ItemType Directory -Path $tmp | Out-Null
  try {
    foreach ($name in @($archive, 'checksums.txt')) {
      try {
        Invoke-WebRequest -UseBasicParsing -Uri "$url/$name" -OutFile (Join-Path $tmp $name)
      } catch {
        # A 404 on a latest download means that no CLI release exists yet.
        if ($_.Exception.Response -and [int]$_.Exception.Response.StatusCode -eq 404 -and -not $env:AMBIT_VERSION) {
          throw 'No Ambit CLI release is published yet (the latest release has no ambit_<os>_<arch> file). See https://github.com/craftsman-apps/ambit/releases'
        }
        throw
      }
    }
    $line = Get-Content (Join-Path $tmp 'checksums.txt') | Where-Object { ($_ -split '\s+')[1] -in @($archive, "*$archive") }
    if (-not $line) { throw "checksums.txt has no line for $archive" }
    $want = (($line | Select-Object -First 1) -split '\s+')[0]
    $got = (Get-FileHash -Algorithm SHA256 -Path (Join-Path $tmp $archive)).Hash.ToLower()
    if ($got -ne $want) { throw "the checksum of $archive does not match checksums.txt (got $got, want $want)" }
    Expand-Archive -Path (Join-Path $tmp $archive) -DestinationPath $tmp -Force
    New-Item -ItemType Directory -Force -Path $dir | Out-Null
    Copy-Item -Path (Join-Path $tmp 'ambit.exe') -Destination (Join-Path $dir 'ambit.exe') -Force
  } finally {
    Remove-Item -Recurse -Force -Path $tmp -ErrorAction SilentlyContinue
  }
  Write-Host "Installed ambit to $(Join-Path $dir 'ambit.exe')"
  if (($env:PATH -split [IO.Path]::PathSeparator) -notcontains $dir) {
    Write-Host "Add the directory to your PATH:"
    Write-Host "  [Environment]::SetEnvironmentVariable('PATH', '$dir;' + [Environment]::GetEnvironmentVariable('PATH', 'User'), 'User')"
  }
  Write-Host 'Next: ambit local up'
}

try {
  Install-Ambit
} catch {
  Write-Host "Error: $($_.Exception.Message)"
  Write-Host '  next: check $env:AMBIT_VERSION and your network, or see https://ambitproject.dev/docs/quick-start'
  # Run as a file (-File): exit 1. Run through iex: do not close the user's PowerShell window.
  if ($MyInvocation.MyCommand.Path) { exit 1 }
}
