# Install mcpload on Windows (PowerShell 5.1 or later):
#
#   irm https://raw.githubusercontent.com/atul121001/mcpload/main/install.ps1 | iex
#
# Downloads the Windows release from GitHub, checks its SHA-256 against the
# release's checksums.txt, unpacks it to %LOCALAPPDATA%\mcpload\<version>\ and
# adds that folder to your user PATH. Running it again upgrades. No admin
# rights needed.
#
# Settings (environment variables):
#   MCPLOAD_VERSION      release to install, e.g. v0.5.0 (default: latest)
#   MCPLOAD_INSTALL_DIR  where releases are unpacked (default: %LOCALAPPDATA%\mcpload)
#   MCPLOAD_NO_PATH=1    don't change PATH (just unpack)
#
# Everything runs inside a function, and errors are reported without `exit`,
# so `irm | iex` never closes your terminal.

function Install-Mcpload {
    Set-StrictMode -Version 2.0
    $ErrorActionPreference = 'Stop'
    $ProgressPreference = 'SilentlyContinue' # Invoke-WebRequest is very slow with the progress bar
    $repo = 'atul121001/mcpload'

    # GitHub needs TLS 1.2; Windows PowerShell 5.1 may default to older versions.
    [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

    $arch = $env:PROCESSOR_ARCHITECTURE
    if ($env:PROCESSOR_ARCHITEW6432) { $arch = $env:PROCESSOR_ARCHITEW6432 }
    if ($arch -eq 'ARM64') {
        Write-Host 'Note: there is no Windows arm64 build yet; installing the amd64 build, which runs under emulation.'
    } elseif ($arch -ne 'AMD64') {
        throw "unsupported CPU $arch (Windows releases are amd64 only)"
    }

    $tag = $env:MCPLOAD_VERSION
    if (-not $tag) {
        try {
            $rel = Invoke-RestMethod -UseBasicParsing -Uri "https://api.github.com/repos/$repo/releases/latest" -Headers @{ 'User-Agent' = 'mcpload-install' }
            $tag = $rel.tag_name
        } catch {
            # API rate-limited: read the /releases/latest redirect instead.
            try {
                $req = [Net.HttpWebRequest]::Create("https://github.com/$repo/releases/latest")
                $req.AllowAutoRedirect = $false
                $req.UserAgent = 'mcpload-install'
                $resp = $req.GetResponse()
                $loc = $resp.Headers['Location']
                $resp.Close()
                if ($loc -match '/tag/([^/]+)$') { $tag = $Matches[1] }
            } catch {
                $tag = $null
            }
        }
        if (-not $tag) {
            throw 'could not find the latest release (GitHub unreachable?); set $env:MCPLOAD_VERSION = "v0.5.0" to pick one'
        }
    }
    if (-not $tag.StartsWith('v')) { $tag = "v$tag" }
    $ver = $tag.Substring(1)
    $name = "mcpload_${ver}_windows_amd64"
    $archive = "$name.zip"
    $base = "https://github.com/$repo/releases/download/$tag"

    $root = $env:MCPLOAD_INSTALL_DIR
    if (-not $root) { $root = Join-Path $env:LOCALAPPDATA 'mcpload' }
    $dest = Join-Path $root $tag

    $tmp = Join-Path ([IO.Path]::GetTempPath()) ("mcpload-install-" + [Guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Force -Path $tmp | Out-Null
    try {
        Write-Host "Installing mcpload $tag (windows/amd64)"
        Write-Host "  downloading $base/$archive"
        $zip = Join-Path $tmp $archive
        $sums = Join-Path $tmp 'checksums.txt'
        try {
            Invoke-WebRequest -UseBasicParsing -Uri "$base/$archive" -OutFile $zip
        } catch {
            throw "download failed: $base/$archive (does release $tag exist? see https://github.com/$repo/releases): $($_.Exception.Message)"
        }
        try {
            Invoke-WebRequest -UseBasicParsing -Uri "$base/checksums.txt" -OutFile $sums
        } catch {
            throw "download failed: $base/checksums.txt: $($_.Exception.Message)"
        }

        $want = $null
        foreach ($line in Get-Content -Path $sums) {
            $parts = $line.Trim() -split '\s+'
            if ($parts.Count -eq 2 -and ($parts[1] -eq $archive -or $parts[1] -eq "*$archive")) { $want = $parts[0].ToLowerInvariant() }
        }
        if (-not $want) { throw "checksums.txt of $tag has no entry for $archive" }
        $got = (Get-FileHash -Algorithm SHA256 -Path $zip).Hash.ToLowerInvariant()
        if ($got -ne $want) {
            throw "checksum mismatch for $archive (expected $want, got $got); the download is corrupt or was tampered with"
        }
        Write-Host "  verified SHA-256 $got"

        New-Item -ItemType Directory -Force -Path $root | Out-Null
        $stage = Join-Path $root (".$name.tmp." + $PID)
        if (Test-Path $stage) { Remove-Item -Recurse -Force $stage }
        Expand-Archive -Path $zip -DestinationPath $stage -Force
        $exe = Join-Path (Join-Path $stage $name) 'mcpload.exe'
        if (-not (Test-Path $exe)) {
            Remove-Item -Recurse -Force $stage
            throw "$archive does not contain $name\mcpload.exe"
        }
        if (Test-Path $dest) {
            try {
                Remove-Item -Recurse -Force $dest
            } catch {
                Remove-Item -Recurse -Force $stage
                throw "could not replace $dest (is mcpload running? close it and try again): $($_.Exception.Message)"
            }
        }
        Move-Item -Path (Join-Path $stage $name) -Destination $dest
        Remove-Item -Recurse -Force $stage
    } finally {
        Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
    }

    # Remove older versions installed by this script.
    foreach ($old in @(Get-ChildItem -Path $root -Directory -Filter 'v*' -ErrorAction SilentlyContinue)) {
        $oldDir = $old.FullName
        if ($oldDir -ne $dest -and (Test-Path (Join-Path $oldDir 'mcpload.exe'))) {
            try { Remove-Item -Recurse -Force $oldDir } catch { Write-Host "  could not remove old version $oldDir (in use?); remove it later" }
        }
    }
    Write-Host "  installed to $dest"

    $pathNote = $null
    if ($env:MCPLOAD_NO_PATH -ne '1') {
        $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
        if (-not $userPath) { $userPath = '' }
        $rootPrefix = $root.TrimEnd('\') + '\'
        $kept = @()
        foreach ($p in ($userPath -split ';')) {
            if (-not $p) { continue }
            # Drop older mcpload versions; keep everything else as is.
            if ($p.StartsWith($rootPrefix, [StringComparison]::OrdinalIgnoreCase) -and $p.TrimEnd('\') -ne $dest) { continue }
            $kept += $p
        }
        $hasDest = $false
        foreach ($p in $kept) { if ($p.TrimEnd('\') -eq $dest) { $hasDest = $true } }
        if (-not $hasDest) { $kept += $dest }
        $newPath = $kept -join ';'
        if ($newPath -ne $userPath) {
            [Environment]::SetEnvironmentVariable('Path', $newPath, 'User')
            Write-Host "  added $dest to your user PATH"
            $pathNote = 'Open a new terminal so it picks up the new PATH (in this window it already works).'
        }
        # Make it work in this session too (irm | iex runs in the current one).
        $sessionKept = @()
        foreach ($p in ($env:Path -split ';')) {
            if (-not $p) { continue }
            if ($p.StartsWith($rootPrefix, [StringComparison]::OrdinalIgnoreCase)) { continue }
            $sessionKept += $p
        }
        $env:Path = (@($dest) + $sessionKept) -join ';'
    }

    Write-Host ''
    & (Join-Path $dest 'mcpload.exe') version
    if ($LASTEXITCODE -ne 0) { throw 'the installed mcpload does not run on this computer' }
    Write-Host ''
    if ($env:MCPLOAD_NO_PATH -eq '1') {
        Write-Host "Done. Run it as: $(Join-Path $dest 'mcpload.exe')"
        return
    }
    Write-Host 'Done. Try it:'
    Write-Host '  mcpload run --url <your MCP server URL>'
    if ($pathNote) { Write-Host $pathNote }
}

try {
    Install-Mcpload
} catch {
    Write-Host "mcpload install: error: $($_.Exception.Message)" -ForegroundColor Red
    # No `exit` (it would close the terminal under irm | iex), but scripts and
    # CI can still see the failure.
    $global:LASTEXITCODE = 1
}
