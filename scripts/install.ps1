# =============================================================================
#  PiPool Nodes -- Windows Node Runner
#  Installs and configures LTC, DOGE, BTC, BCH, PEP daemons on Windows
#  Run as Administrator in PowerShell:
#    Set-ExecutionPolicy Bypass -Scope Process -Force
#    .\install.ps1
# =============================================================================

#Requires -RunAsAdministrator

$ErrorActionPreference = "Stop"

# ?? Colors ????????????????????????????????????????????????????????????????????
function Log   { Write-Host "  [OK] $args" -ForegroundColor Green }
function Warn  { Write-Host "  [!!] $args" -ForegroundColor Yellow }
function Info  { Write-Host "  [..] $args" -ForegroundColor Cyan }
function Step  { Write-Host "`n== $args ==" -ForegroundColor Cyan }
function Err   { Write-Host "  [XX] $args" -ForegroundColor Red; exit 1 }

Write-Host @"
  +------------------------------------------------------+
  |   PiPool Nodes -- Windows Node Runner                |
  |   LTC / DOGE / BTC / BCH / PEP / LCC                |
  +------------------------------------------------------+
"@ -ForegroundColor Cyan

# ?? Config ????????????????????????????????????????????????????????????????????
$NODES_DIR = "C:\PiPoolNodes"
$BIN_DIR   = "$NODES_DIR\bin"

$LTC_VERSION = "0.21.3"
$DOGE_VERSION = "1.14.7"
$BTC_VERSION  = "26.0"
$BCH_VERSION  = "27.1.0"

# ?? Collect config ????????????????????????????????????????????????????????????
Step "Configuration"

$PI_IP = Read-Host "  Pi's IP address (e.g. 192.168.1.110)"
if (-not $PI_IP) { Err "Pi IP is required" }

$LTC_WALLET  = Read-Host "  LTC wallet address"
$DOGE_WALLET = Read-Host "  DOGE wallet address"
$BTC_WALLET  = Read-Host "  BTC wallet address"
$BCH_WALLET  = Read-Host "  BCH wallet address"

$ENABLE_PEP = Read-Host "  Enable PEP? [y/N]"
$PEP_WALLET = ""
if ($ENABLE_PEP -match "^[Yy]") {
    $PEP_WALLET = Read-Host "  PEP wallet address"
}

$ENABLE_LCC = Read-Host "  Enable LCC (Litecoin Cash)? [y/N]"
$LCC_WALLET = ""
if ($ENABLE_LCC -match "^[Yy]") {
    $LCC_WALLET = Read-Host "  LCC wallet address"
}

# Data directories -- default to C:\PiPoolNodes\<coin>
Write-Host "`n  Data directories (press Enter for defaults under $NODES_DIR\data\)" -ForegroundColor Cyan
$LTC_DATADIR  = Read-Host "  LTC  datadir [$NODES_DIR\data\litecoin]"
$DOGE_DATADIR = Read-Host "  DOGE datadir [$NODES_DIR\data\dogecoin]"
$BTC_DATADIR  = Read-Host "  BTC  datadir [$NODES_DIR\data\bitcoin]"
$BCH_DATADIR  = Read-Host "  BCH  datadir [$NODES_DIR\data\bch]"
$PEP_DATADIR  = Read-Host "  PEP  datadir [$NODES_DIR\data\pepecoin]"
$LCC_DATADIR  = Read-Host "  LCC  datadir [$NODES_DIR\data\litecoincash]"

if (-not $LTC_DATADIR)  { $LTC_DATADIR  = "$NODES_DIR\data\litecoin" }
if (-not $DOGE_DATADIR) { $DOGE_DATADIR = "$NODES_DIR\data\dogecoin" }
if (-not $BTC_DATADIR)  { $BTC_DATADIR  = "$NODES_DIR\data\bitcoin" }
if (-not $BCH_DATADIR)  { $BCH_DATADIR  = "$NODES_DIR\data\bch" }
if (-not $PEP_DATADIR)  { $PEP_DATADIR  = "$NODES_DIR\data\pepecoin" }
if (-not $LCC_DATADIR)  { $LCC_DATADIR  = "$NODES_DIR\data\litecoincash" }

# Generate RPC passwords
function New-RpcPass { return [System.Convert]::ToBase64String((1..32 | ForEach-Object { [byte](Get-Random -Max 256) })).Substring(0,32) }
$LTC_RPC_PASS  = New-RpcPass
$DOGE_RPC_PASS = New-RpcPass
$BTC_RPC_PASS  = New-RpcPass
$BCH_RPC_PASS  = New-RpcPass
$PEP_RPC_PASS  = New-RpcPass
$LCC_RPC_PASS  = New-RpcPass

# ?? Create directories ????????????????????????????????????????????????????????
Step "Creating directories"
foreach ($dir in @($NODES_DIR, $BIN_DIR, $LTC_DATADIR, $DOGE_DATADIR, $BTC_DATADIR, $BCH_DATADIR, $PEP_DATADIR, $LCC_DATADIR)) {
    New-Item -ItemType Directory -Force -Path $dir | Out-Null
}
Log "Directories created"

# ?? Download helpers ??????????????????????????????????????????????????????????
function Download-Daemon {
    param($Name, $Url, $Archive, $Dest)
    $tmp = "$env:TEMP\$Archive"
    if (Test-Path $Dest) { Log "$Name already installed"; return }
    Info "Downloading $Name..."
    Invoke-WebRequest -Uri $Url -OutFile $tmp -UseBasicParsing
    Info "Extracting $Name..."
    Expand-Archive -Path $tmp -DestinationPath "$env:TEMP\$Name-extract" -Force
    Remove-Item $tmp
    return "$env:TEMP\$Name-extract"
}

# ?? Install Litecoin ??????????????????????????????????????????????????????????
Step "Installing Litecoin $LTC_VERSION"
$ltcBin = "$BIN_DIR\litecoind.exe"
if (-not (Test-Path $ltcBin)) {
    $url = "https://download.litecoin.org/litecoin-$LTC_VERSION/win/litecoin-$LTC_VERSION-win64.zip"
    $extract = Download-Daemon "litecoin" $url "litecoin.zip" $ltcBin
    Copy-Item "$extract\litecoin-$LTC_VERSION\bin\litecoind.exe" $BIN_DIR
    Copy-Item "$extract\litecoin-$LTC_VERSION\bin\litecoin-cli.exe" $BIN_DIR
    Remove-Item $extract -Recurse -Force
    Log "litecoind.exe installed"
} else { Log "litecoind.exe already present" }

# ?? Install Dogecoin ??????????????????????????????????????????????????????????
Step "Installing Dogecoin $DOGE_VERSION"
$dogeBin = "$BIN_DIR\dogecoind.exe"
if (-not (Test-Path $dogeBin)) {
    $url = "https://github.com/dogecoin/dogecoin/releases/download/v$DOGE_VERSION/dogecoin-$DOGE_VERSION-win64.zip"
    $extract = Download-Daemon "dogecoin" $url "dogecoin.zip" $dogeBin
    Copy-Item "$extract\dogecoin-$DOGE_VERSION\bin\dogecoind.exe" $BIN_DIR
    Copy-Item "$extract\dogecoin-$DOGE_VERSION\bin\dogecoin-cli.exe" $BIN_DIR
    Remove-Item $extract -Recurse -Force
    Log "dogecoind.exe installed"
} else { Log "dogecoind.exe already present" }

# ?? Install Bitcoin ???????????????????????????????????????????????????????????
Step "Installing Bitcoin $BTC_VERSION"
$btcBin = "$BIN_DIR\bitcoind.exe"
if (-not (Test-Path $btcBin)) {
    $url = "https://bitcoincore.org/bin/bitcoin-core-$BTC_VERSION/bitcoin-$BTC_VERSION-win64.zip"
    $extract = Download-Daemon "bitcoin" $url "bitcoin.zip" $btcBin
    Copy-Item "$extract\bitcoin-$BTC_VERSION\bin\bitcoind.exe" $BIN_DIR
    Copy-Item "$extract\bitcoin-$BTC_VERSION\bin\bitcoin-cli.exe" $BIN_DIR
    Remove-Item $extract -Recurse -Force
    Log "bitcoind.exe installed"
} else { Log "bitcoind.exe already present" }

# ?? Install Bitcoin Cash ??????????????????????????????????????????????????????
Step "Installing Bitcoin Cash $BCH_VERSION"
$bchBin = "$BIN_DIR\bitcoind-bch.exe"
if (-not (Test-Path $bchBin)) {
    $url = "https://github.com/bitcoin-cash-node/bitcoin-cash-node/releases/download/v$BCH_VERSION/bitcoin-cash-node-$BCH_VERSION-win64.zip"
    $extract = Download-Daemon "bch" $url "bch.zip" $bchBin
    Copy-Item "$extract\bitcoin-cash-node-$BCH_VERSION\bin\bitcoind.exe" "$BIN_DIR\bitcoind-bch.exe"
    Copy-Item "$extract\bitcoin-cash-node-$BCH_VERSION\bin\bitcoin-cli.exe" "$BIN_DIR\bitcoin-cli-bch.exe"
    Remove-Item $extract -Recurse -Force
    Log "bitcoind-bch.exe installed"
} else { Log "bitcoind-bch.exe already present" }

# ?? Install Litecoin Cash ?????????????????????????????????????????????????????
Step "Installing Litecoin Cash"
$lccBin = "$BIN_DIR\lccashd.exe"
if (-not (Test-Path $lccBin)) {
    $url = "https://github.com/litecoincash-project/litecoincash/releases/download/v0.18.1/litecoincash-0.18.1-win64.zip"
    $extract = Download-Daemon "litecoincash" $url "litecoincash.zip" $lccBin
    Copy-Item "$extract\litecoincash-0.18.1\bin\lccashd.exe" $BIN_DIR
    Copy-Item "$extract\litecoincash-0.18.1\bin\lccash-cli.exe" $BIN_DIR
    Remove-Item $extract -Recurse -Force
    Log "lccashd.exe installed"
} else { Log "lccashd.exe already present" }

# ?? Write daemon configs ??????????????????????????????????????????????????????
Step "Writing daemon configs"

# Helper -- write a config file
function Write-DaemonConfig {
    param($Path, $Content)
    Set-Content -Path $Path -Value $Content -Encoding UTF8
}

Write-DaemonConfig "$LTC_DATADIR\litecoin.conf" @"
server=1
daemon=0
datadir=$LTC_DATADIR
rpcuser=litecoind
rpcpassword=$LTC_RPC_PASS
rpcallowip=127.0.0.1
rpcallowip=$PI_IP
rpcbind=0.0.0.0
rpcport=9332
zmqpubhashblock=tcp://0.0.0.0:28332
dbcache=1024
maxmempool=300
listen=1
"@

Write-DaemonConfig "$DOGE_DATADIR\dogecoin.conf" @"
server=1
daemon=0
datadir=$DOGE_DATADIR
rpcuser=dogecoind
rpcpassword=$DOGE_RPC_PASS
rpcallowip=127.0.0.1
rpcallowip=$PI_IP
rpcbind=0.0.0.0
rpcport=22555
zmqpubhashblock=tcp://0.0.0.0:28333
dbcache=1024
maxmempool=300
listen=1
"@

Write-DaemonConfig "$BTC_DATADIR\bitcoin.conf" @"
server=1
daemon=0
datadir=$BTC_DATADIR
rpcuser=bitcoind
rpcpassword=$BTC_RPC_PASS
rpcallowip=127.0.0.1
rpcallowip=$PI_IP
rpcbind=0.0.0.0
rpcport=8332
zmqpubhashblock=tcp://0.0.0.0:28334
dbcache=2048
maxmempool=300
listen=1
"@

Write-DaemonConfig "$BCH_DATADIR\bitcoin.conf" @"
server=1
daemon=0
datadir=$BCH_DATADIR
rpcuser=bchd
rpcpassword=$BCH_RPC_PASS
rpcallowip=127.0.0.1
rpcallowip=$PI_IP
rpcbind=0.0.0.0
rpcport=8336
zmqpubhashblock=tcp://0.0.0.0:28335
dbcache=1024
maxmempool=300
listen=1
"@

if ($ENABLE_PEP -match "^[Yy]") {
    Write-DaemonConfig "$PEP_DATADIR\pepecoin.conf" @"
server=1
daemon=0
datadir=$PEP_DATADIR
rpcuser=pepd
rpcpassword=$PEP_RPC_PASS
rpcallowip=127.0.0.1
rpcallowip=$PI_IP
rpcbind=0.0.0.0
rpcport=33873
dbcache=256
maxmempool=100
listen=1
"@
}

if ($ENABLE_LCC -match "^[Yy]") {
    Write-DaemonConfig "$LCC_DATADIR\litecoincash.conf" @"
server=1
daemon=0
datadir=$LCC_DATADIR
rpcuser=lccd
rpcpassword=$LCC_RPC_PASS
rpcallowip=127.0.0.1
rpcallowip=$PI_IP
rpcbind=0.0.0.0
rpcport=62457
zmqpubhashblock=tcp://0.0.0.0:28336
dbcache=512
maxmempool=300
listen=1
"@
}

Log "Daemon configs written"

# ?? Write pipool.json snippet ?????????????????????????????????????????????????
Step "Writing pipool-nodes.json (copy values to your Pi's pipool.json)"
$PC_IP = (Get-NetIPAddress -AddressFamily IPv4 | Where-Object { $_.InterfaceAlias -notlike "*Loopback*" } | Select-Object -First 1).IPAddress
$PEP_ENABLED = if ($ENABLE_PEP -match "^[Yy]") { "true" } else { "false" }
$LCC_ENABLED = if ($ENABLE_LCC -match "^[Yy]") { "true" } else { "false" }

$pipoolJson = @"
{
  "NOTE": "Copy the 'coins' section into your Pi's /opt/pipool/configs/pipool.json",
  "windows_pc_ip": "$PC_IP",
  "pi_ip": "$PI_IP",
  "coins": {
    "LTC": {
      "enabled": true,
      "symbol": "LTC",
      "algorithm": "scrypt",
      "merge_parent": "",
      "datadir": "",
      "stratum": {
        "port": 3333,
        "vardiff": { "min_diff": 0.001, "max_diff": 1024, "target_ms": 30000, "retarget_s": 60 },
        "tls": { "enabled": false, "port": 3343, "cert_file": "", "key_file": "" }
      },
      "node": {
        "host": "$PC_IP", "port": 9332,
        "user": "litecoind", "password": "$LTC_RPC_PASS",
        "zmq_pub_hashblock": "tcp://$PC_IP`:28332"
      },
      "wallet": "$LTC_WALLET",
      "block_reward": 6.25
    },
    "DOGE": {
      "enabled": true,
      "symbol": "DOGE",
      "algorithm": "scrypt",
      "merge_parent": "LTC",
      "datadir": "",
      "stratum": {
        "port": 3334,
        "vardiff": { "min_diff": 0.001, "max_diff": 1024, "target_ms": 30000, "retarget_s": 60 },
        "tls": { "enabled": false, "port": 3344, "cert_file": "", "key_file": "" }
      },
      "node": {
        "host": "$PC_IP", "port": 22555,
        "user": "dogecoind", "password": "$DOGE_RPC_PASS",
        "zmq_pub_hashblock": "tcp://$PC_IP`:28333"
      },
      "wallet": "$DOGE_WALLET",
      "block_reward": 10000
    },
    "BTC": {
      "enabled": true,
      "symbol": "BTC",
      "algorithm": "sha256d",
      "merge_parent": "",
      "datadir": "",
      "stratum": {
        "port": 3335,
        "vardiff": { "min_diff": 1, "max_diff": 1048576, "target_ms": 30000, "retarget_s": 60 },
        "tls": { "enabled": false, "port": 3345, "cert_file": "", "key_file": "" }
      },
      "node": {
        "host": "$PC_IP", "port": 8332,
        "user": "bitcoind", "password": "$BTC_RPC_PASS",
        "zmq_pub_hashblock": "tcp://$PC_IP`:28334"
      },
      "wallet": "$BTC_WALLET",
      "block_reward": 3.125
    },
    "BCH": {
      "enabled": true,
      "symbol": "BCH",
      "algorithm": "sha256d",
      "merge_parent": "BTC",
      "datadir": "",
      "stratum": {
        "port": 3336,
        "vardiff": { "min_diff": 1, "max_diff": 1048576, "target_ms": 30000, "retarget_s": 60 },
        "tls": { "enabled": false, "port": 3346, "cert_file": "", "key_file": "" }
      },
      "node": {
        "host": "$PC_IP", "port": 8336,
        "user": "bchd", "password": "$BCH_RPC_PASS",
        "zmq_pub_hashblock": "tcp://$PC_IP`:28335"
      },
      "wallet": "$BCH_WALLET",
      "block_reward": 6.25
    },
    "PEP": {
      "enabled": $PEP_ENABLED,
      "symbol": "PEP",
      "algorithm": "scryptn",
      "merge_parent": "",
      "datadir": "",
      "stratum": {
        "port": 3337,
        "vardiff": { "min_diff": 0.001, "max_diff": 512, "target_ms": 30000, "retarget_s": 60 },
        "tls": { "enabled": false, "port": 3347, "cert_file": "", "key_file": "" }
      },
      "node": {
        "host": "$PC_IP", "port": 33873,
        "user": "pepd", "password": "$PEP_RPC_PASS"
      },
      "wallet": "$PEP_WALLET",
      "block_reward": 50000
    },
    "LCC": {
      "enabled": $LCC_ENABLED,
      "symbol": "LCC",
      "algorithm": "sha256d",
      "merge_parent": "BTC",
      "datadir": "",
      "stratum": {
        "port": 3338,
        "vardiff": { "min_diff": 1, "max_diff": 1048576, "target_ms": 30000, "retarget_s": 60 },
        "tls": { "enabled": false, "port": 3348, "cert_file": "", "key_file": "" }
      },
      "node": {
        "host": "$PC_IP", "port": 62457,
        "user": "lccd", "password": "$LCC_RPC_PASS",
        "zmq_pub_hashblock": "tcp://$PC_IP`:28336"
      },
      "wallet": "$LCC_WALLET",
      "block_reward": 250
    }
  }
}
"@
Set-Content -Path "$NODES_DIR\pipool-nodes.json" -Value $pipoolJson -Encoding UTF8
Log "pipool-nodes.json written to $NODES_DIR\pipool-nodes.json"

# ?? Windows Firewall rules ????????????????????????????????????????????????????
Step "Configuring Windows Firewall"
$ports = @(
    @{ Name="PiPool-LTC-RPC";   Port=9332  },
    @{ Name="PiPool-LTC-ZMQ";   Port=28332 },
    @{ Name="PiPool-DOGE-RPC";  Port=22555 },
    @{ Name="PiPool-DOGE-ZMQ";  Port=28333 },
    @{ Name="PiPool-BTC-RPC";   Port=8332  },
    @{ Name="PiPool-BTC-ZMQ";   Port=28334 },
    @{ Name="PiPool-BCH-RPC";   Port=8336  },
    @{ Name="PiPool-BCH-ZMQ";   Port=28335 },
    @{ Name="PiPool-PEP-RPC";   Port=33873 },
    @{ Name="PiPool-LCC-RPC";   Port=62457 },
    @{ Name="PiPool-LCC-ZMQ";   Port=28336 }
)
foreach ($rule in $ports) {
    $existing = Get-NetFirewallRule -DisplayName $rule.Name -ErrorAction SilentlyContinue
    if ($existing) { Remove-NetFirewallRule -DisplayName $rule.Name }
    New-NetFirewallRule `
        -DisplayName $rule.Name `
        -Direction Inbound `
        -Protocol TCP `
        -LocalPort $rule.Port `
        -RemoteAddress $PI_IP `
        -Action Allow | Out-Null
    Log "Firewall: $($rule.Name) port $($rule.Port) -> $PI_IP only"
}

# ?? Write start/stop/status scripts ??????????????????????????????????????????
Step "Writing management scripts"

# Store config for start/stop scripts
$cfg = @"
`$NODES_DIR    = '$NODES_DIR'
`$BIN_DIR      = '$BIN_DIR'
`$LTC_DATADIR  = '$LTC_DATADIR'
`$DOGE_DATADIR = '$DOGE_DATADIR'
`$BTC_DATADIR  = '$BTC_DATADIR'
`$BCH_DATADIR  = '$BCH_DATADIR'
`$PEP_DATADIR  = '$PEP_DATADIR'
`$ENABLE_PEP   = '$ENABLE_PEP'
`$LCC_DATADIR  = '$LCC_DATADIR'
`$ENABLE_LCC   = '$ENABLE_LCC'
`$PC_IP        = '$PC_IP'
`$PI_IP        = '$PI_IP'
"@
Set-Content "$NODES_DIR\config.ps1" $cfg
Log "config.ps1 saved"

# ?? Summary ???????????????????????????????????????????????????????????????????
Write-Host "`n" 
Write-Host "+------------------------------------------------------+" -ForegroundColor Green
Write-Host "|  [OK] PiPool Nodes Installation Complete!            |" -ForegroundColor Green
Write-Host "+------------------------------------------------------+" -ForegroundColor Green
Write-Host ""
Write-Host "  This PC's IP:  $PC_IP" -ForegroundColor Yellow
Write-Host "  Pi's IP:       $PI_IP" -ForegroundColor Yellow
Write-Host ""
Write-Host "Next steps:" -ForegroundColor Cyan
Write-Host "  1. Start nodes:     .\start-nodes.ps1"
Write-Host "  2. Check sync:      .\status-nodes.ps1"
Write-Host "  3. Update Pi config: copy values from $NODES_DIR\pipool-nodes.json"
Write-Host "     into /opt/pipool/configs/pipool.json on the Pi"
Write-Host "  4. Restart PiPool on Pi: sudo systemctl restart pipool"
Write-Host ""
Write-Host "  Config: $NODES_DIR\pipool-nodes.json" -ForegroundColor Yellow
Write-Host ""
