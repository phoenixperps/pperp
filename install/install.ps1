# Installs pperp, the Phoenix Perpetuals command line, for the current user (no administrator rights):
#
#   irm https://docs.phoenixperpsfunding.com/install.ps1 | iex
#
# $env:PPERP_VERSION = '1.2.0'      that version instead of the latest
# $env:PPERP_INSTALL_DIR = 'C:\...'  where it goes (default %LOCALAPPDATA%\Programs\pperp)
# $env:PPERP_NO_MODIFY_PATH = '1'    leave the user PATH as it is
#
# The archive is checked against the release's SHA256SUMS (pperp upgrade also checks the signatures). Nothing runs
# until the whole script is here: Install-Pperp is called on the last line.

function Install-Pperp {
    $ErrorActionPreference = 'Stop'
    [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
    $base = if ($env:PPERP_RELEASES_URL) { $env:PPERP_RELEASES_URL } else { 'https://docs.phoenixperpsfunding.com/downloads/cli' }
    if (-not ($base.StartsWith('https://') -or $base.StartsWith('http://127.0.0.1:'))) { throw 'pperp: the release address must be https' }
    $arch = 'amd64'
    if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64' -or $env:PROCESSOR_ARCHITEW6432 -eq 'ARM64') { $arch = 'arm64' }

    $version = $env:PPERP_VERSION
    if (-not $version) { $version = (Invoke-RestMethod -UseBasicParsing "$base/latest.json").version }
    if (-not $version) { throw 'pperp: could not read the latest version' }
    $version = $version.TrimStart('v')

    $tmp = Join-Path ([IO.Path]::GetTempPath()) ("pperp-" + [Guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $tmp | Out-Null
    try {
        $file = "pperp_${version}_windows_${arch}.zip"
        Invoke-WebRequest -UseBasicParsing "$base/v$version/$file" -OutFile (Join-Path $tmp $file)
        Invoke-WebRequest -UseBasicParsing "$base/v$version/SHA256SUMS" -OutFile (Join-Path $tmp 'SHA256SUMS')
        $want = $null
        foreach ($line in Get-Content (Join-Path $tmp 'SHA256SUMS')) {
            $parts = $line -split '\s+'
            if ($parts.Count -eq 2 -and $parts[1].TrimStart('*') -eq $file) { $want = $parts[0].ToLower() }
        }
        if (-not $want) { throw "pperp: version $version has no build for windows/$arch" }
        $got = (Get-FileHash -Algorithm SHA256 (Join-Path $tmp $file)).Hash.ToLower()
        if ($got -ne $want) { throw 'pperp: the download does not match its checksum: nothing was installed' }

        Expand-Archive -Path (Join-Path $tmp $file) -DestinationPath (Join-Path $tmp 'x') -Force
        $new = Join-Path $tmp 'x\pperp.exe'
        & $new version | Out-Null
        if ($LASTEXITCODE -ne 0) { throw 'pperp: the new pperp does not run on this computer' }

        $dir = if ($env:PPERP_INSTALL_DIR) { $env:PPERP_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA 'Programs\pperp' }
        New-Item -ItemType Directory -Force -Path $dir | Out-Null
        $exe = Join-Path $dir 'pperp.exe'
        if (Test-Path $exe) {
            # a running pperp cannot be overwritten, but it can be moved aside (removed at its next start)
            Remove-Item -Force -ErrorAction SilentlyContinue "$exe.old"
            Move-Item -Force $exe "$exe.old"
        }
        Copy-Item $new $exe

        $config = Join-Path $env:APPDATA 'pperp'
        New-Item -ItemType Directory -Force -Path $config | Out-Null
        [IO.File]::WriteAllText((Join-Path $config 'install.json'), (@{ installed_by = 'install.ps1'; version = $version; path = $exe } | ConvertTo-Json -Compress))

        $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
        if (-not $env:PPERP_NO_MODIFY_PATH -and -not (($userPath -split ';') -contains $dir)) {
            [Environment]::SetEnvironmentVariable('Path', ($userPath.TrimEnd(';') + ';' + $dir).TrimStart(';'), 'User')
            $env:Path = $env:Path + ';' + $dir
            Write-Host "pperp $version is installed in $dir, now on your PATH (new terminals see it)."
        } else {
            Write-Host "pperp $version is installed in $dir."
        }
        Write-Host 'Next: pperp login --sandbox'
    } finally {
        Remove-Item -Recurse -Force -ErrorAction SilentlyContinue $tmp
    }
}

Install-Pperp
