# bgraph installer for Windows:
#   irm https://raw.githubusercontent.com/qxbao/bgraph/main/scripts/install.ps1 | iex
#
# Downloads the release archive for this architecture, checks its SHA-256 against checksums.txt, installs bgraph.exe
# and adds its directory to the user PATH when it is not there yet.
#
# Environment:
#   BGRAPH_VERSION      release to install, e.g. 0.2.0 (default: the latest)
#   BGRAPH_INSTALL_DIR  directory for bgraph.exe (default: %LOCALAPPDATA%\Programs\bgraph)
#   BGRAPH_NO_MODIFY_PATH=1  leave the user PATH alone

& {
    $ErrorActionPreference = 'Stop'
    $ProgressPreference = 'SilentlyContinue' # Invoke-WebRequest is much slower with the progress bar
    [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

    $repo = 'https://github.com/qxbao/bgraph'
    $dir = if ($env:BGRAPH_INSTALL_DIR) { $env:BGRAPH_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA 'Programs\bgraph' }

    $arch = switch ($env:PROCESSOR_ARCHITECTURE) {
        'AMD64' { 'amd64' }
        'ARM64' { 'arm64' }
        default { throw "bgraph-install: unsupported architecture $env:PROCESSOR_ARCHITECTURE" }
    }
    # A 32-bit PowerShell on a 64-bit Windows reports x86 here and the real architecture in PROCESSOR_ARCHITEW6432.
    if ($env:PROCESSOR_ARCHITEW6432 -eq 'ARM64') { $arch = 'arm64' } elseif ($env:PROCESSOR_ARCHITEW6432 -eq 'AMD64') { $arch = 'amd64' }

    $v = "$env:BGRAPH_VERSION".TrimStart('v')
    if (-not $v) {
        $req = [Net.HttpWebRequest]::Create("$repo/releases/latest")
        $req.AllowAutoRedirect = $false
        $req.Method = 'HEAD'
        $resp = $req.GetResponse()
        $loc = $resp.Headers['Location']
        $resp.Close()
        if ($loc -notmatch '/tag/v([^/]+)$') { throw 'bgraph-install: could not find the latest release' }
        $v = $Matches[1]
    }

    $asset = "bgraph_${v}_windows_${arch}.zip"
    $tmp = Join-Path ([IO.Path]::GetTempPath()) ("bgraph-install-" + [Guid]::NewGuid())
    New-Item -ItemType Directory -Path $tmp | Out-Null
    try {
        Write-Host "bgraph-install: downloading bgraph $v (windows/$arch)"
        Invoke-WebRequest -UseBasicParsing "$repo/releases/download/v$v/$asset" -OutFile (Join-Path $tmp $asset)
        Invoke-WebRequest -UseBasicParsing "$repo/releases/download/v$v/checksums.txt" -OutFile (Join-Path $tmp 'checksums.txt')

        $want = $null
        foreach ($line in Get-Content (Join-Path $tmp 'checksums.txt')) {
            $f = $line -split '\s+'
            if ($f.Count -ge 2 -and $f[1] -eq $asset) { $want = $f[0] }
        }
        if (-not $want) { throw "bgraph-install: checksums.txt has no entry for $asset" }
        $got = (Get-FileHash -Algorithm SHA256 (Join-Path $tmp $asset)).Hash
        if ($got -ne $want) { throw "bgraph-install: SHA-256 mismatch for $asset" }

        Expand-Archive (Join-Path $tmp $asset) -DestinationPath (Join-Path $tmp 'x') -Force
        New-Item -ItemType Directory -Path $dir -Force | Out-Null
        $exe = Join-Path $dir 'bgraph.exe'
        # A running bgraph.exe cannot be overwritten but can be renamed.
        if (Test-Path $exe) {
            Remove-Item "$exe.old" -Force -ErrorAction SilentlyContinue
            Move-Item $exe "$exe.old" -Force
        }
        Copy-Item (Join-Path $tmp 'x\bgraph.exe') $exe -Force
        Remove-Item "$exe.old" -Force -ErrorAction SilentlyContinue
        Write-Host "bgraph-install: installed $exe"
    } finally {
        Remove-Item $tmp -Recurse -Force -ErrorAction SilentlyContinue
    }

    if ($env:BGRAPH_NO_MODIFY_PATH -ne '1') {
        $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
        $parts = @("$userPath" -split ';' | Where-Object { $_ })
        if ($parts -notcontains $dir) {
            [Environment]::SetEnvironmentVariable('Path', (($parts + $dir) -join ';'), 'User')
            Write-Host "bgraph-install: added $dir to the user PATH (new terminals pick it up)"
        }
        if (@($env:Path -split ';') -notcontains $dir) { $env:Path = "$env:Path;$dir" }
    }

    & $exe version
}
