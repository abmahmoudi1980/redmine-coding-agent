<#
.SYNOPSIS
    Installs RedmineCodingAgent as a Windows service.

.DESCRIPTION
    R9 deployment hardening: the worker (and every agent it spawns) runs under a
    dedicated service account that can write only the data root, the workspace
    root, and the log directory. The script uses sc.exe directly, so it needs no
    installer framework and works on Windows Server 2019+ / Windows 10+.

    The service is a plain console service: `serve` already handles SIGTERM
    equivalents (`sc stop` sends a stop control), cancels the active run, drains
    event streams, and checkpoints the SQLite WAL before exiting 0.

.PARAMETER BinaryPath
    Full path to redmine-agent.exe.

.PARAMETER ConfigPath
    Full path to config.yaml. It is passed with --config.

.PARAMETER DataRoot
    Directory that holds the database, workspaces, mirrors, and logs.

.PARAMETER ServiceAccount
    Dedicated account in DOMAIN\name or .\name form. Default: NT AUTHORITY\LocalService
    is supported but NOT recommended on a host shared with other workloads,
    because LocalService is not isolated from other LocalService processes.

.PARAMETER Password
    Password for the service account when it is a domain or local user.
    Omit for built-in accounts.

.EXAMPLE
    .\install-service.ps1 `
        -BinaryPath 'C:\Program Files\RedmineCodingAgent\redmine-agent.exe' `
        -ConfigPath 'C:\ProgramData\RedmineCodingAgent\config.yaml' `
        -DataRoot   'C:\ProgramData\RedmineCodingAgent\data' `
        -ServiceAccount 'DOMAIN\svc-redmine-agent' -Password (Read-Host -AsSecureString)
#>

[CmdletBinding(SupportsShouldProcess = $true)]
param(
    [Parameter(Mandatory = $true)][string] $BinaryPath,
    [Parameter(Mandatory = $true)][string] $ConfigPath,
    [Parameter(Mandatory = $true)][string] $DataRoot,
    [string] $ServiceName = 'RedmineCodingAgent',
    [string] $DisplayName = 'RedmineCodingAgent',
    [string] $ServiceAccount = 'NT AUTHORITY\LocalService',
    [securestring] $Password
)

$ErrorActionPreference = 'Stop'

if (-not (Test-Path -LiteralPath $BinaryPath)) { throw "binary not found: $BinaryPath" }
if (-not (Test-Path -LiteralPath $ConfigPath)) { throw "config not found: $ConfigPath" }

$logsRoot = Join-Path $DataRoot 'logs'
$workspaces = Join-Path $DataRoot 'workspaces'
$databaseDir = Join-Path (Split-Path -Parent $ConfigPath) 'data'
foreach ($directory in @($DataRoot, $logsRoot, $workspaces, $databaseDir)) {
    New-Item -ItemType Directory -Force -Path $directory | Out-Null
}

# ACLs: the service account writes only the data root; secrets stay readable by
# administrators and the account, and nobody else. R9 residual risk: a
# deliberately malicious agent running as this account still has the account's
# rights, so keep those rights minimal.
$account = $ServiceAccount
$readOnlyConfig = (Resolve-Path -LiteralPath $ConfigPath).Path
& icacls $DataRoot /inheritance:r /grant:r "$account:(OI)(CI)M" 'Administrators:(OI)(CI)F' | Out-Null
& icacls $readOnlyConfig /inheritance:r /grant:r "$account:R" 'Administrators:F' | Out-Null

$binPathArgument = '"{0}" serve --config "{1}"' -f $BinaryPath, $readOnlyConfig

if (Get-Service -Name $ServiceName -ErrorAction SilentlyContinue) {
    if ($PSCmdlet.ShouldProcess($ServiceName, 'update existing service')) {
        & sc.exe config $ServiceName binPath= $binPathArgument start= auto | Out-Null
        & sc.exe description $ServiceName $DisplayName | Out-Null
    }
} else {
    if ($PSCmdlet.ShouldProcess($ServiceName, 'create service')) {
        if ($Password) {
            $plain = [Runtime.InteropServices.Marshal]::PtrToStringAuto(
                [Runtime.InteropServices.Marshal]::SecureStringToBSTR($Password))
            try {
                & sc.exe create $ServiceName binPath= $binPathArgument start= auto `
                    obj= $ServiceAccount password= $plain DisplayName= $DisplayName | Out-Null
            } finally {
                $plain = $null
            }
        } else {
            & sc.exe create $ServiceName binPath= $binPathArgument start= auto `
                obj= $ServiceAccount DisplayName= $DisplayName | Out-Null
        }
        & sc.exe description $ServiceName $DisplayName | Out-Null
    }
}

# Restart twice on failure, then stop and leave the failure visible.
& sc.exe failure $ServiceName reset= 86400 actions= restart/5000/restart/15000// | Out-Null
& sc.exe start $ServiceName | Out-Null

Write-Host "Service '$ServiceName' is installed and started."
Write-Host "  binary:  $BinaryPath"
Write-Host "  config:  $readOnlyConfig (read-only to the service account)"
Write-Host "  data:    $DataRoot (full control to $account)"
Write-Host "  logs:    $logsRoot"
Write-Host ""
Write-Host "Run 'redmine-agent doctor --config `"$readOnlyConfig`" --all' as the service account before the first real run."
Write-Host "To remove: sc.exe stop $ServiceName; sc.exe delete $ServiceName"
