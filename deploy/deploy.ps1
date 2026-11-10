<#
.SYNOPSIS
  Builds and deploys photo-sync to AWS (Lambda + API Gateway + DynamoDB + S3).

.DESCRIPTION
  1. Shows which AWS account and region it will deploy to.
  2. Creates the two SecureString secrets in SSM Parameter Store if missing:
       <prefix>/api-key-hash    hex SHA-256 of the owner API key
       <prefix>/session-secret  signs browser session cookies
     A new API key is printed ONCE; only its hash is stored in AWS.
  3. Builds the Lambda binary (linux/arm64) into build/lambda/bootstrap.
  4. Runs `sam deploy`, which shows the change set and asks before applying.
  5. Prints the app URL.

  Works in Windows PowerShell 5.1 and PowerShell 7.

.PARAMETER Guided
  Run `sam deploy --guided` (first deploy, or to change saved settings in
  deploy/samconfig.toml). Used automatically when samconfig.toml is missing.

.PARAMETER NewApiKey
  Generate and store a new owner API key, replacing the old one. Running
  Lambda instances keep accepting the old key until they are recycled; the
  deploy that follows replaces them.

.EXAMPLE
  .\deploy\deploy.ps1                 # deploy (guided the first time)
  .\deploy\deploy.ps1 -NewApiKey      # rotate the owner API key and deploy
  .\deploy\deploy.ps1 -AwsProfile photo  # use a named AWS CLI profile
#>
[CmdletBinding()]
param(
    [string]$StackName = 'photosync',
    [string]$ParameterPrefix = '/photosync',
    [string]$Region,
    # Not "$Profile": that would shadow PowerShell's automatic $PROFILE.
    [string]$AwsProfile,
    [switch]$Guided,
    [switch]$NewApiKey
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$RepoRoot = Split-Path $PSScriptRoot -Parent
$BuildDir = Join-Path $RepoRoot 'build\lambda'

if ($AwsProfile) { $env:AWS_PROFILE = $AwsProfile }
if ($Region) { $env:AWS_REGION = $Region; $env:AWS_DEFAULT_REGION = $Region }

# --- helpers -----------------------------------------------------------------

# Invoke-Native runs a native command and returns its stdout. In PowerShell
# 5.1, a native command writing to stderr under ErrorActionPreference=Stop
# aborts the script, so stderr is captured separately and only reported if
# the command fails (or -AllowFailure is set, in which case $null is
# returned on failure).
function Invoke-Native {
    param(
        [Parameter(Mandatory)][string]$Exe,
        [string[]]$Arguments = @(),
        [switch]$AllowFailure
    )
    $errFile = [IO.Path]::GetTempFileName()
    try {
        $ErrorActionPreference = 'Continue'
        $out = & $Exe @Arguments 2> $errFile
        $code = $LASTEXITCODE
    } finally {
        $ErrorActionPreference = 'Stop'
    }
    $err = Get-Content -Raw $errFile -ErrorAction SilentlyContinue
    Remove-Item $errFile -ErrorAction SilentlyContinue
    if ($code -ne 0) {
        if ($AllowFailure) { return $null }
        throw "$Exe $($Arguments -join ' ') failed (exit $code):`n$err"
    }
    return ($out -join "`n")
}

function New-RandomBytes([int]$Count) {
    $bytes = New-Object byte[] $Count
    $rng = [Security.Cryptography.RandomNumberGenerator]::Create()
    try { $rng.GetBytes($bytes) } finally { $rng.Dispose() }
    return , $bytes
}

function Get-Sha256Hex([string]$Text) {
    $sha = [Security.Cryptography.SHA256]::Create()
    try {
        $hash = $sha.ComputeHash([Text.Encoding]::UTF8.GetBytes($Text))
    } finally { $sha.Dispose() }
    return -join ($hash | ForEach-Object { $_.ToString('x2') })
}

function Test-Parameter([string]$Name) {
    $null -ne (Invoke-Native aws @('ssm', 'get-parameter', '--name', $Name, '--output', 'json') -AllowFailure)
}

function Set-SecureParameter([string]$Name, [string]$Value, [string]$Description) {
    # The value starts with a hex or base64 character, never '-', so the CLI
    # can't mistake it for an option.
    Invoke-Native aws @('ssm', 'put-parameter', '--name', $Name, '--type', 'SecureString',
        '--value', $Value, '--description', $Description, '--overwrite') | Out-Null
}

# --- 0. tools and target -------------------------------------------------------

foreach ($tool in 'go', 'aws', 'sam') {
    if (-not (Get-Command $tool -ErrorAction SilentlyContinue)) {
        throw "'$tool' not found on PATH. Install it (see README) and open a new terminal."
    }
}

$identity = Invoke-Native aws @('sts', 'get-caller-identity', '--output', 'json') | ConvertFrom-Json
$targetRegion = Invoke-Native aws @('configure', 'get', 'region') -AllowFailure
if ($Region) { $targetRegion = $Region }
if (-not $targetRegion) {
    throw 'No AWS region configured. Run `aws configure` (or pass -Region).'
}
Write-Host "Account: $($identity.Account)  ($($identity.Arn))"
Write-Host "Region:  $targetRegion"
Write-Host "Stack:   $StackName"
Write-Host ''

# --- 1. secrets ------------------------------------------------------------------

$keyParam = "$ParameterPrefix/api-key-hash"
$secretParam = "$ParameterPrefix/session-secret"

if ($NewApiKey -or -not (Test-Parameter $keyParam)) {
    # 32 random bytes, base64url without padding: 43 URL- and shell-safe chars.
    $apiKey = [Convert]::ToBase64String((New-RandomBytes 32)).TrimEnd('=').Replace('+', '-').Replace('/', '_')
    Set-SecureParameter $keyParam (Get-Sha256Hex $apiKey) 'photo-sync: hex SHA-256 of the owner API key'
    Write-Host '================================================================'
    Write-Host ' New owner API key (shown once; only its hash is stored in AWS):'
    Write-Host ''
    Write-Host "   $apiKey"
    Write-Host ''
    Write-Host ' Save it in your password manager. Use it to sign in to the web'
    Write-Host ' UI and as PHOTOSYNC_API_KEY for the uploader.'
    Write-Host '================================================================'
    Write-Host ''
} else {
    Write-Host "API key hash: $keyParam exists (use -NewApiKey to rotate)"
}

if (-not (Test-Parameter $secretParam)) {
    $secret = [Convert]::ToBase64String((New-RandomBytes 48))
    Set-SecureParameter $secretParam $secret 'photo-sync: session cookie signing secret'
    Write-Host "Session secret: created $secretParam"
} else {
    Write-Host "Session secret: $secretParam exists"
}

# --- 2. build --------------------------------------------------------------------

Write-Host ''
Write-Host 'Building Lambda binary (linux/arm64)...'
New-Item -ItemType Directory -Force $BuildDir | Out-Null
$saved = @{ GOOS = $env:GOOS; GOARCH = $env:GOARCH; CGO_ENABLED = $env:CGO_ENABLED }
try {
    $env:GOOS = 'linux'; $env:GOARCH = 'arm64'; $env:CGO_ENABLED = '0'
    Push-Location $RepoRoot
    try {
        # lambda.norpc: provided.al2023 doesn't need the legacy RPC mode.
        Invoke-Native go @('build', '-tags', 'lambda.norpc', '-trimpath', '-ldflags', '-s -w',
            '-o', (Join-Path $BuildDir 'bootstrap'), './cmd/lambda') | Out-Null
    } finally { Pop-Location }
} finally {
    # Restore the caller's Go environment.
    foreach ($k in $saved.Keys) {
        if ($null -eq $saved[$k]) { Remove-Item "env:$k" -ErrorAction SilentlyContinue }
        else { Set-Item "env:$k" $saved[$k] }
    }
}

# --- 3. deploy -------------------------------------------------------------------

# sam deploy is interactive (guided prompts, change set confirmation), so it
# runs directly rather than through Invoke-Native.
$samArgs = @('deploy', '--stack-name', $StackName,
    '--parameter-overrides', "ParameterPrefix=$ParameterPrefix")
if ($Region) { $samArgs += @('--region', $Region) }
if ($Guided -or -not (Test-Path (Join-Path $PSScriptRoot 'samconfig.toml'))) {
    $samArgs += '--guided'
}
Push-Location $PSScriptRoot
try {
    $ErrorActionPreference = 'Continue'
    & sam @samArgs
    $samExit = $LASTEXITCODE
} finally {
    $ErrorActionPreference = 'Stop'
    Pop-Location
}
if ($samExit -ne 0) { throw "sam deploy failed or was cancelled (exit $samExit)." }

# --- 4. result -------------------------------------------------------------------

$url = Invoke-Native aws @('cloudformation', 'describe-stacks', '--stack-name', $StackName,
    '--query', "Stacks[0].Outputs[?OutputKey=='ApiUrl'].OutputValue", '--output', 'text')
Write-Host ''
Write-Host "Owner UI:  $url"
Write-Host "Uploader:  `$env:PHOTOSYNC_API_URL = '$url'"
