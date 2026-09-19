# SPDX-FileCopyrightText: 2026 TorrPlay
#
# SPDX-License-Identifier: MIT

<#
.SYNOPSIS
    Builds the Jacklet MSI.

.DESCRIPTION
    Installs the pinned WiX toolset and its extensions, converts the
    licence to the RTF the wizard needs, and builds the package. CI and a
    maintainer run this same script, so a release is reproducible by hand.

    The staging directory is expected to hold what the Windows archive
    holds -- jacklet.exe, README.md, LICENSE and docs\ -- so that the
    installer ships the same binary the archive does rather than one built
    separately.
#>
[CmdletBinding()]
param(
    # The release version, as the tag spells it, e.g. v1.2.3 or v1.2.3-rc.1.
    [Parameter(Mandatory = $true)][string] $Version,
    # The unpacked archive to package.
    [Parameter(Mandatory = $true)][string] $StageDir,
    # Where mkico wrote the icon and the wizard bitmaps.
    [Parameter(Mandatory = $true)][string] $AssetDir,
    [string] $OutFile,
    [ValidateSet('x64', 'arm64')][string] $Arch = 'x64',
    # Pinned, so rebuilding an old tag produces the same package.
    [string] $WixVersion = '6.0.1',
    [string] $RepoUrl = 'https://github.com/torrplay/jacklet'
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

<#
.SYNOPSIS
    Reduces a release version to the three numbers an MSI can carry.

.DESCRIPTION
    ProductVersion is major.minor.build with no pre-release suffix, and
    Windows compares only those numbers when deciding whether one package
    supersedes another. So v1.2.3-rc.1 and v1.2.3-rc.2 are both 1.2.3,
    which is why the package sets AllowSameVersionUpgrades; the file name
    keeps the full version, so nothing is ambiguous to a reader.
#>
function ConvertTo-MsiVersion {
    param([Parameter(Mandatory = $true)][string] $Version)

    $trimmed = $Version.TrimStart('v')
    $trimmed = ($trimmed -split '[-+]', 2)[0]

    if ($trimmed -notmatch '^\d+\.\d+(\.\d+)?$') {
        throw "Cannot derive an installer version from '$Version': expected something like v1.2.3."
    }

    $parts = $trimmed -split '\.'
    $major, $minor = [int]$parts[0], [int]$parts[1]
    $build = if ($parts.Count -gt 2) { [int]$parts[2] } else { 0 }

    # The installer's own limits. Exceeding one silently truncates, which
    # would ship a package that refuses to upgrade its predecessor.
    if ($major -gt 255 -or $minor -gt 255) { throw "Version '$Version': major and minor must each be at most 255." }
    if ($build -gt 65535) { throw "Version '$Version': the patch number must be at most 65535." }

    return "$major.$minor.$build"
}

<#
.SYNOPSIS
    Converts the plain-text licence to the RTF the licence page renders.

.DESCRIPTION
    Derived from the licence the repository already carries rather than
    kept as a second copy, so the installer cannot show a stale one.
#>
function ConvertTo-LicenseRtf {
    param(
        [Parameter(Mandatory = $true)][string] $Source,
        [Parameter(Mandatory = $true)][string] $Destination
    )

    $text = Get-Content -Path $Source -Raw

    # RTF is a 7-bit format; a non-ASCII byte would need escaping, and
    # emitting it raw would render as mojibake in the wizard.
    foreach ($char in $text.ToCharArray()) {
        if ([int]$char -gt 127) {
            throw "The licence contains a non-ASCII character '$char', which this conversion does not escape."
        }
    }

    # Literal replacements rather than -replace, whose pattern is a regex
    # and whose replacement treats a backslash as itself: escaping one
    # there means writing four to get two, and getting it wrong is
    # invisible until a licence contains a backslash.
    $escaped = $text.Replace('\', '\\').Replace('{', '\{').Replace('}', '\}')
    # A blank line is a paragraph break; a single newline is a wrap in the
    # source, and the wizard reflows, so it becomes a space.
    $escaped = $escaped -replace "`r`n", "`n"
    $escaped = $escaped -replace "`n`n+", '\par\par '
    $escaped = $escaped -replace "`n", ' '

    $rtf = '{\rtf1\ansi\deff0{\fonttbl{\f0\fnil\fcharset0 Segoe UI;}}\fs18 ' + $escaped + '}'
    Set-Content -Path $Destination -Value $rtf -Encoding Ascii -NoNewline
}

$msiVersion = ConvertTo-MsiVersion -Version $Version
$repoRoot = Resolve-Path (Join-Path $PSScriptRoot '..' '..')
$here = $PSScriptRoot

if (-not $OutFile) {
    $goarch = if ($Arch -eq 'arm64') { 'arm64' } else { 'amd64' }
    $OutFile = Join-Path (Get-Location) "jacklet_${Version}_windows_${goarch}.msi"
}

Write-Host "Building $OutFile (product version $msiVersion, $Arch)"

ConvertTo-LicenseRtf -Source (Join-Path $repoRoot 'LICENSES' 'MIT.txt') `
                     -Destination (Join-Path $AssetDir 'LICENSE.rtf')

if (-not (Get-Command wix -ErrorAction SilentlyContinue)) {
    dotnet tool install --global wix --version $WixVersion
    if ($LASTEXITCODE -ne 0) { throw 'Installing the WiX toolset failed.' }
}
foreach ($extension in 'WixToolset.UI.wixext', 'WixToolset.Util.wixext', 'WixToolset.Firewall.wixext') {
    wix extension add -g "$extension/$WixVersion"
    if ($LASTEXITCODE -ne 0) { throw "Adding $extension failed." }
}

# No .wixpdb: it is the build's own symbol database, useful only for
# authoring a patch against this exact package, which a project that
# ships whole versions never does. Left on, it lands beside the installer
# and follows it onto the release, where every file is something a user
# downloads and runs, and where it would be the one file the published
# checksums do not cover.
wix build `
    -pdbtype none `
    -arch $Arch `
    -define "ProductVersion=$msiVersion" `
    -define "StageDir=$StageDir" `
    -define "AssetDir=$AssetDir" `
    -define "RepoUrl=$RepoUrl" `
    -ext WixToolset.UI.wixext `
    -ext WixToolset.Util.wixext `
    -ext WixToolset.Firewall.wixext `
    -out $OutFile `
    (Join-Path $here 'jacklet.wxs') `
    (Join-Path $here 'jacklet.ui.wxs')

if ($LASTEXITCODE -ne 0) { throw 'wix build failed.' }
Write-Host "Wrote $OutFile"
