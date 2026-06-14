[CmdletBinding()]
param(
    [string]$ServiceName = "UDResellingDynDNS",
    [string]$ExecutablePath = (Join-Path $PSScriptRoot "..\bin\windows-amd64\ud-reselling-dyndns.exe"),
    [string]$ConfigPath = (Join-Path $PSScriptRoot "..\config.json"),
    [string]$InstallDirectory = (Join-Path $env:ProgramFiles "UDResellingDynDNS"),
    [string]$DataDirectory = (Join-Path $env:ProgramData "UDResellingDynDNS")
)

$ErrorActionPreference = "Stop"

if (-not ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole(
    [Security.Principal.WindowsBuiltInRole]::Administrator
)) {
    throw "Run this script from an elevated PowerShell session."
}

$ExecutablePath = (Resolve-Path $ExecutablePath).Path
$ConfigPath = (Resolve-Path $ConfigPath).Path
$installedExecutable = Join-Path $InstallDirectory "ud-reselling-dyndns.exe"
$installedConfig = Join-Path $DataDirectory "config.json"
$logPath = Join-Path $DataDirectory "service.log"

New-Item -ItemType Directory -Force -Path $InstallDirectory, $DataDirectory | Out-Null
Copy-Item -Force $ExecutablePath $installedExecutable
Copy-Item -Force $ConfigPath $installedConfig

# Remove inherited access and permit only administrators, SYSTEM, and the
# LocalService account used by the service.
& icacls.exe $DataDirectory /inheritance:r | Out-Null
& icacls.exe $DataDirectory /grant:r `
    "*S-1-5-32-544:(OI)(CI)F" `
    "*S-1-5-18:(OI)(CI)F" `
    "*S-1-5-19:(OI)(CI)M" | Out-Null

$existingService = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
if ($null -ne $existingService) {
    if ($existingService.Status -ne "Stopped") {
        Stop-Service -Name $ServiceName -Force
    }
    & sc.exe delete $ServiceName | Out-Null
    do {
        Start-Sleep -Milliseconds 250
        $existingService = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
    } while ($null -ne $existingService)
}

$binaryPath = "`"$installedExecutable`" -service-name `"$ServiceName`" -config `"$installedConfig`" -log `"$logPath`""
New-Service `
    -Name $ServiceName `
    -BinaryPathName $binaryPath `
    -DisplayName "United Domains Reselling DynDNS" `
    -Description "Updates United Domains Reselling DNS zones when public IP addresses change." `
    -StartupType Automatic | Out-Null

& sc.exe config $ServiceName obj= "NT AUTHORITY\LocalService" password= "" | Out-Null
& sc.exe failure $ServiceName reset= 86400 actions= restart/10000/restart/30000/restart/60000 | Out-Null
& sc.exe failureflag $ServiceName 1 | Out-Null

Start-Service -Name $ServiceName
Get-Service -Name $ServiceName
