[CmdletBinding()]
param(
    [string]$ServiceName = "UDResellingDynDNS",
    [switch]$RemoveFiles
)

$ErrorActionPreference = "Stop"

if (-not ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole(
    [Security.Principal.WindowsBuiltInRole]::Administrator
)) {
    throw "Run this script from an elevated PowerShell session."
}

$service = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
if ($null -ne $service) {
    if ($service.Status -ne "Stopped") {
        Stop-Service -Name $ServiceName -Force
    }
    & sc.exe delete $ServiceName | Out-Null
}

if ($RemoveFiles) {
    Remove-Item -Recurse -Force -ErrorAction SilentlyContinue `
        (Join-Path $env:ProgramFiles "UDResellingDynDNS"),
        (Join-Path $env:ProgramData "UDResellingDynDNS")
}
