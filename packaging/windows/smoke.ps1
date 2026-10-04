# SPDX-FileCopyrightText: 2026 TorrPlay
#
# SPDX-License-Identifier: MIT

<#
.SYNOPSIS
    Installs the Jacklet MSI, checks what it did, and removes it again.

.DESCRIPTION
    The package can only really be verified where it can be installed, so
    this runs on a Windows runner against the package just built. It
    covers what the authoring is most likely to get quietly wrong: the
    service account's permissions, the settings reaching the service,
    neither credential surviving in a form that could be read back, an
    upgrade preserving an operator's edit, and an uninstall leaving the
    operator's data behind.

    It needs an elevated session, which is what a CI runner gives it.
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string] $MsiPath,
    # A build of a later version, to exercise a real upgrade. Reinstalling
    # the same package does not: an upgrade removes the installed product
    # first, and that removal is what a package has to survive.
    [string] $UpgradeMsiPath,
    # A later one again, for the upgrade that declines the firewall rule.
    # It needs its own version for the same reason the first does.
    [string] $DeclineMsiPath,
    [string] $ApiKey = 'ci-smoke-key',
    [string] $AdminPassword = 'ci-smoke-password',
    [int] $Port = 9117
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$failures = [System.Collections.Generic.List[string]]::new()

function Test-That {
    param([Parameter(Mandatory = $true)][string] $Name, [Parameter(Mandatory = $true)][bool] $Condition)
    if ($Condition) {
        Write-Host "  ok    $Name"
    } else {
        Write-Host "  FAIL  $Name"
        $failures.Add($Name)
    }
}

function Invoke-Msi {
    param([Parameter(Mandatory = $true)][string[]] $Arguments, [string] $LogPath)
    if ($LogPath) { $Arguments += @('/l*v', $LogPath) }
    $process = Start-Process msiexec.exe -ArgumentList $Arguments -Wait -PassThru
    return $process.ExitCode
}

# The settings an installed service runs with, as NAME=value strings.
function Get-ServiceSettings {
    $key = 'HKLM:\SOFTWARE\Jacklet\Settings'
    if (-not (Test-Path $key)) { return @() }
    $item = Get-Item $key
    return @($item.GetValueNames() | Where-Object { $_ } | ForEach-Object { "$_=$($item.GetValue($_))" })
}

# Nothing configured reads the same as nothing found, and every check
# below that looks for an absent value would pass either way. This is what
# separates them.
function Assert-Configured {
    param([Parameter(Mandatory = $true)][string[]] $Settings)
    Test-That 'the service has settings at all' ($Settings.Count -gt 0)
    if ($Settings.Count -eq 0) {
        Write-Host '  (the checks that follow cannot mean anything without them)'
    }
}

function Wait-ForHealth {
    param([int] $Port, [int] $TimeoutSeconds = 60)
    $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
    while ((Get-Date) -lt $deadline) {
        try {
            $response = Invoke-WebRequest "http://127.0.0.1:$Port/healthz" -UseBasicParsing -TimeoutSec 5
            if ($response.StatusCode -eq 200) { return $true }
        } catch {
            Start-Sleep -Milliseconds 500
        }
    }
    return $false
}

$log = Join-Path $env:TEMP 'jacklet-install.log'
$programData = Join-Path $env:ProgramData 'Jacklet'
$programFiles = Join-Path $env:ProgramFiles 'Jacklet'

# First, while nothing is installed: the launch condition also passes for
# a maintenance install, so once the product is present this proves
# nothing.
Write-Host 'Checking an unconfigured silent install is refused'
$code = Invoke-Msi -Arguments @('/i', "`"$MsiPath`"", '/qn')
Test-That 'installing with no API key fails' ($code -ne 0)
Test-That 'nothing was installed' (-not (Test-Path (Join-Path $programFiles 'jacklet.exe')))

Write-Host 'Installing'
$code = Invoke-Msi -LogPath $log -Arguments @(
    '/i', "`"$MsiPath`"", '/qn',
    "JACKLETAPIKEY=$ApiKey",
    "JACKLETADMINPASSWORD=$AdminPassword",
    "JACKLETPORT=$Port",
    'ADDFIREWALLRULE=1'
)
if ($code -ne 0) {
    Get-Content $log -Tail 120
    throw "msiexec returned $code"
}

Write-Host 'Checking the installation'
Test-That 'the executable is installed' (Test-Path (Join-Path $programFiles 'jacklet.exe'))
Test-That 'the documentation is installed' (Test-Path (Join-Path $programFiles 'docs'))
Test-That 'the state directories exist' (
    (Test-Path (Join-Path $programData 'definitions')) -and
    (Test-Path (Join-Path $programData 'config')) -and
    (Test-Path (Join-Path $programData 'logs')))

# An executable with no icon means the resource object was not generated
# before the build, which nothing else would notice.
$fileVersion = (Get-Item (Join-Path $programFiles 'jacklet.exe')).VersionInfo
Test-That 'the executable carries its version details' ($fileVersion.ProductName -eq 'Jacklet')

Write-Host 'Checking the service'
$service = Get-Service Jacklet -ErrorAction SilentlyContinue
Test-That 'the service is installed' ($null -ne $service)
Test-That 'the service starts automatically' ((Get-CimInstance Win32_Service -Filter "Name='Jacklet'").StartMode -eq 'Auto')
Test-That 'the service runs as the unprivileged built-in account' (
    (Get-CimInstance Win32_Service -Filter "Name='Jacklet'").StartName -eq 'NT AUTHORITY\LocalService')
Test-That 'the service is running' ($null -ne $service -and $service.Status -eq 'Running')
Test-That 'recovery includes stops that return an error' (
    (Get-ItemPropertyValue -Path 'HKLM:\SYSTEM\CurrentControlSet\Services\Jacklet' `
                           -Name FailureActionsOnNonCrashFailures -ErrorAction SilentlyContinue) -eq 1)

Write-Host 'Checking the configuration reached the service'
$environment = Get-ServiceSettings
Assert-Configured -Settings $environment
Test-That 'the API key is configured' ($environment -contains "ApiKey=$ApiKey")
Test-That 'the database path is absolute' (
    $environment -contains "DatabasePath=$(Join-Path $programData 'jacklet.db')")
Test-That 'the log file is configured' (
    $environment -contains "LogFile=$(Join-Path $programData 'logs\jacklet.log')")

Write-Host 'Checking the admin password was hashed and the plaintext removed'
Test-That 'the bootstrap password is gone' (
    $null -eq (Get-ItemProperty -Path 'HKLM:\SOFTWARE\Jacklet\Setup' -Name AdminPassword -ErrorAction SilentlyContinue))
$hash = ($environment | Where-Object { $_ -like 'AdminPasswordHash=*' }) -replace '^[^=]+=', ''
Test-That 'a password hash was stored' (-not [string]::IsNullOrEmpty($hash))
Test-That 'the stored value is not the password' ($hash -ne $AdminPassword)
Test-That 'the password is not among the settings' (
    $environment.Count -gt 0 -and -not (($environment -join "`n") -match [regex]::Escape($AdminPassword)))
Test-That 'the hash is among them instead' (
    ($environment -join "`n") -match 'AdminPasswordHash=.')

# Both credentials are written to the registry by the installer, and a
# verbose log records every registry write with the value it wrote, so
# both are in it by construction. Marking a property hidden omits it from
# the property dump and changes nothing else. What can be checked, and is
# the property the design actually rests on, is that neither credential
# survives anywhere afterwards in a form that could be read back.
Write-Host 'Checking no credential is left in plaintext'
# The whole subtree, not one key: the point is that the password is
# nowhere, which a check aimed at a single key could not establish.
$stored = (Get-ChildItem 'HKLM:\SOFTWARE\Jacklet' -Recurse -ErrorAction SilentlyContinue) +
          @(Get-Item 'HKLM:\SOFTWARE\Jacklet' -ErrorAction SilentlyContinue) |
    ForEach-Object {
        $key = $_
        $key.GetValueNames() | ForEach-Object { "$($key.Name)\$_=$($key.GetValue($_))" }
    }
Test-That 'the password is nowhere under Jacklet' (
    -not (($stored -join "`n") -match [regex]::Escape($AdminPassword)))
Test-That 'the setup key holds nothing' (
    $null -eq (Get-Item 'HKLM:\SOFTWARE\Jacklet\Setup' -ErrorAction SilentlyContinue) -or
    0 -eq (Get-Item 'HKLM:\SOFTWARE\Jacklet\Setup').ValueCount)

Write-Host 'Checking permissions'
$serviceSid = 'NT AUTHORITY\LOCAL SERVICE'
Test-That 'the service can write its configuration directory' (
    @((Get-Acl (Join-Path $programData 'config')).Access |
        Where-Object { $_.IdentityReference -eq $serviceSid -and $_.FileSystemRights -match 'Write|Modify|FullControl' }).Count -gt 0)
Test-That 'the service can write its log directory' (
    @((Get-Acl (Join-Path $programData 'logs')).Access |
        Where-Object { $_.IdentityReference -eq $serviceSid -and $_.FileSystemRights -match 'Write|Modify|FullControl' }).Count -gt 0)
# The negative matters more than the positive: a service that can write
# its own service key can rewrite its ImagePath.
Test-That 'the service cannot write its own service key' (
    @((Get-Acl 'HKLM:\SYSTEM\CurrentControlSet\Services\Jacklet').Access |
        Where-Object { $_.IdentityReference -eq $serviceSid -and $_.RegistryRights -match 'WriteKey|SetValue|FullControl' }).Count -eq 0)

Write-Host 'Checking it serves'
Test-That 'the health endpoint answers' (Wait-ForHealth -Port $Port)
Test-That 'the log file was written' (Test-Path (Join-Path $programData 'logs\jacklet.log'))
Test-That 'a lifecycle event was recorded' (
    $null -ne (Get-WinEvent -LogName Application -MaxEvents 20 -ErrorAction SilentlyContinue |
        Where-Object { $_.ProviderName -eq 'Jacklet' } | Select-Object -First 1))

# Read straight from the registry: "service config" prints only the names
# in the settings schema, so it says nothing about where a value sits.
Test-That 'the settings key holds no installer bookkeeping' (
    -not ((Get-ServiceSettings) | Where-Object { $_ -like 'FirewallRule=*' }))
Test-That 'the firewall rule is recorded under the installer key instead' (
    $null -ne (Get-ItemProperty -Path 'HKLM:\SOFTWARE\Jacklet\Installer' -Name FirewallRule -ErrorAction SilentlyContinue))

Write-Host 'Checking the firewall rule was created'
Test-That 'a firewall rule exists for the port' (
    $null -ne (Get-NetFirewallRule -DisplayName 'Jacklet' -ErrorAction SilentlyContinue))

# The port the service should be answering on from here on. The upgrade
# check moves it to prove an operator's edit survives, and every later
# check reads it, so it is settled before anything optional runs: without
# a value here, skipping the upgrade leaves the decline check reading a
# variable nothing assigned, which Set-StrictMode makes fatal.
$configuredPort = $Port

if ($UpgradeMsiPath) {
    Write-Host 'Checking an upgrade keeps what the operator configured'
    $configuredPort = $Port + 1
    $movedDb = Join-Path $programData 'moved.db'
    & (Join-Path $programFiles 'jacklet.exe') service config set "Port=$configuredPort" | Out-Null
    & (Join-Path $programFiles 'jacklet.exe') service config set "DatabasePath=$movedDb" | Out-Null
    $edited = Get-ServiceSettings
    Test-That 'the edits were stored' (
        $edited -contains "Port=$configuredPort" -and $edited -contains "DatabasePath=$movedDb")

    # No properties at all: an upgrade has to carry forward what is
    # already configured rather than be told it again.
    $upgradeLog = Join-Path $env:TEMP 'jacklet-upgrade.log'
    $code = Invoke-Msi -LogPath $upgradeLog -Arguments @('/i', "`"$UpgradeMsiPath`"", '/qn')

    if ($code -ne 0) { Get-Content $upgradeLog -Tail 60 }
    Test-That 'the upgrade succeeded without being given an API key' ($code -eq 0)

    $upgraded = Get-ServiceSettings
    Assert-Configured -Settings $upgraded
    Test-That 'the edited port survived the upgrade' ($upgraded -contains "Port=$configuredPort")
    Test-That 'the API key survived the upgrade' ($upgraded -contains "ApiKey=$ApiKey")
    Test-That 'the admin password hash survived the upgrade' (
        ($upgraded -join "`n") -match 'AdminPasswordHash=.')
    Test-That 'the relocated database path survived the upgrade' (
        $upgraded -contains "DatabasePath=$movedDb")
    Test-That 'the firewall rule survived the upgrade' (
        $null -ne (Get-NetFirewallRule -DisplayName 'Jacklet' -ErrorAction SilentlyContinue))
    Test-That 'the service is running after the upgrade' (
        (Get-Service Jacklet -ErrorAction SilentlyContinue).Status -eq 'Running')
    Test-That 'it serves on the port the operator chose' (Wait-ForHealth -Port $configuredPort)

    # Uninstalling the upgrade is uninstalling the product.
    $MsiPath = $UpgradeMsiPath
} else {
    Write-Host 'Skipping the upgrade checks: no second package was given'
}

# An explicit decline has to outrank what was configured before, or the
# recovery is not a default but an override. It is done as another
# upgrade, not a reinstall: an upgrade removes the installed product and
# with it the rule, leaving the replacing one to decide whether to create
# one, which is the decision being tested.
if ($DeclineMsiPath) {
    Write-Host 'Checking an upgrade can be told to drop the firewall rule'
    $code = Invoke-Msi -Arguments @('/i', "`"$DeclineMsiPath`"", '/qn', 'ADDFIREWALLRULE=0')
    Test-That 'the upgrade succeeded' ($code -eq 0)

    $declined = Get-ServiceSettings
    Assert-Configured -Settings $declined
    # What the installer recorded, which says whether the component was
    # installed at all, and so tells a declined rule apart from one that
    # was asked for and then not removed.
    Test-That 'no firewall rule was recorded' (
        $null -eq (Get-ItemProperty -Path 'HKLM:\SOFTWARE\Jacklet\Installer' -Name FirewallRule -ErrorAction SilentlyContinue))
    Test-That 'the firewall rule is gone' (
        $null -eq (Get-NetFirewallRule -DisplayName 'Jacklet' -ErrorAction SilentlyContinue))
    Test-That 'declining the rule left the other settings alone' (
        $declined -contains "Port=$configuredPort" -and $declined -contains "ApiKey=$ApiKey")

    $MsiPath = $DeclineMsiPath
} else {
    Write-Host 'Skipping the decline check: no third package was given'
}

Write-Host 'Uninstalling'
$code = Invoke-Msi -Arguments @('/x', "`"$MsiPath`"", '/qn')
Test-That 'the uninstall succeeded' ($code -eq 0)
Test-That 'the service is gone' ($null -eq (Get-Service Jacklet -ErrorAction SilentlyContinue))
Test-That 'the program is gone' (-not (Test-Path (Join-Path $programFiles 'jacklet.exe')))
# The database, the definitions and the tracker credentials are the
# operator's, the way removing the Debian package leaves /var/lib/jacklet.
Test-That 'the state directory remains' (Test-Path $programData)
# Settings are not data: leaving them would let a later install keep an
# API key that its wizard appeared to replace.
Test-That 'the settings are gone' (
    $null -eq (Get-Item 'HKLM:\SOFTWARE\Jacklet\Settings' -ErrorAction SilentlyContinue))

if ($failures.Count -gt 0) {
    Write-Host ''
    Write-Host "$($failures.Count) check(s) failed:"
    $failures | ForEach-Object { Write-Host "  - $_" }
    exit 1
}
Write-Host ''
Write-Host 'All checks passed.'
