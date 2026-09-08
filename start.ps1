# Levanta el backend Go del POC en su puerto.
#
#   .\start.ps1              # compila y arranca el servidor
#   .\start.ps1 -Frontend    # el servidor + el frontend Angular en :4200
#   .\start.ps1 -Stop        # para todo lo que este script haya levantado
#
# La salida del backend va a logs\go.log, NO a una ventana de consola: una
# consola interactiva de Windows congela el proceso que escribe en ella si
# alguien hace clic dentro (QuickEdit), y el backend se queda mudo a mitad de un
# handshake. Ademas asi los logs quedan en disco para poder compararlos con un
# diff entre ejecuciones, que es lo que pide el §8 del checklist.
#
#   Get-Content logs\go.log -Wait    # para seguirlo en vivo

param(
    [switch]$Frontend,
    [switch]$Stop
)

$ErrorActionPreference = 'Stop'
$root = $PSScriptRoot
$logs = Join-Path $root 'logs'

if ($Stop) {
    $names = 'wspoc-go', 'node'
    $procs = Get-Process -Name $names -ErrorAction SilentlyContinue
    if ($procs) {
        $procs | Stop-Process -Force
        Write-Host ("Parados: {0}" -f (($procs | ForEach-Object ProcessName | Sort-Object -Unique) -join ', ')) -ForegroundColor Yellow
    } else {
        Write-Host 'No habia nada levantado.' -ForegroundColor Yellow
    }
    return
}

if (-not (Test-Path $logs)) { New-Item -ItemType Directory -Path $logs | Out-Null }

# tools/ tiene su propio node_modules (el paquete `ws`): sin el, probe.js y loadtest.js
# no arrancan en un repo recien clonado.
if (-not (Test-Path (Join-Path $root 'tools\node_modules'))) {
    Write-Host 'Instalando dependencias de tools/ (probe.js y loadtest.js)...' -ForegroundColor DarkGray
    Push-Location (Join-Path $root 'tools')
    try { & npm.cmd install --no-fund --no-audit } finally { Pop-Location }
}

# --- Go (8084) -------------------------------------------------------------
$dir = Join-Path $root 'backend-go'
$exe = Join-Path $dir 'wspoc-go.exe'

# Se para el proceso anterior ANTES de compilar. Windows no deja sobrescribir un
# .exe en ejecucion: sin esto, `go build` falla, el proceso viejo conserva el
# puerto, el sondeo de /health responde OK y te quedas sirviendo codigo antiguo
# sin ningun aviso. Costo un rato de diagnosticar.
$previo = Get-Process -Name 'wspoc-go' -ErrorAction SilentlyContinue
if ($previo) {
    Write-Host 'Parando la instancia anterior...' -ForegroundColor DarkGray
    $previo | Stop-Process -Force
    # Windows tarda un instante en liberar el fichero y el puerto.
    Start-Sleep -Milliseconds 500
}

# Se compila el binario en vez de `go run .`, que deja un proceso hijo con otro
# nombre imposible de parar por el del padre.
Write-Host 'Compilando Go...' -ForegroundColor DarkGray
Push-Location $dir
try { & go build -o 'wspoc-go.exe' . } finally { Pop-Location }
if ($LASTEXITCODE -ne 0) {
    Write-Error 'go build fallo; no se arranca nada. Revisa los errores de compilacion.'
    return
}

if (Test-Path $exe) {
    Write-Host 'Levantando Go (8084)...' -ForegroundColor Cyan
    Start-Process -FilePath $exe -WorkingDirectory $dir `
        -RedirectStandardOutput (Join-Path $logs 'go.log') `
        -RedirectStandardError  (Join-Path $logs 'go.err.log') `
        -WindowStyle Hidden -PassThru | Out-Null
} else {
    Write-Warning 'go build no genero el binario; nada que arrancar.'
}

# --- Frontend Angular (4200) ----------------------------------------------
if ($Frontend) {
    $fdir = Join-Path $root 'frontend-angular'
    # node_modules no se versiona.
    if (-not (Test-Path (Join-Path $fdir 'node_modules'))) {
        Write-Host 'No hay node_modules; ejecutando npm install (tarda un rato)...' -ForegroundColor DarkGray
        Push-Location $fdir
        try { & npm.cmd install --no-fund --no-audit } finally { Pop-Location }
    }
    Write-Host 'Levantando el frontend Angular (4200)...' -ForegroundColor Cyan
    Start-Process -FilePath 'cmd.exe' -ArgumentList '/c', 'npm start' -WorkingDirectory $fdir `
        -RedirectStandardOutput (Join-Path $logs 'frontend.log') `
        -RedirectStandardError  (Join-Path $logs 'frontend.err.log') `
        -WindowStyle Hidden | Out-Null
}

# --- Comprobacion ----------------------------------------------------------
Write-Host ''
Write-Host 'Esperando a que responda...' -ForegroundColor DarkGray

$port = 8084
$envFile = Join-Path $root '.env'
if (Test-Path $envFile) {
    $m = Select-String -Path $envFile -Pattern '^\s*WS_PORT_GO\s*=\s*(\d+)' | Select-Object -First 1
    if ($m) { $port = [int]$m.Matches[0].Groups[1].Value }
}
if ($env:WS_PORT) { $port = [int]$env:WS_PORT }

$ok = $false
$deadline = (Get-Date).AddSeconds(60)
while (-not $ok -and (Get-Date) -lt $deadline) {
    try {
        # 127.0.0.1 y no localhost: el servidor escucha solo en IPv4, y 'localhost'
        # resuelve antes a ::1. Donde el firewall descarta el SYN en vez de
        # rechazarlo, ese primer intento cuesta ~2 s antes de caer a IPv4, justo por
        # encima del plazo, y el backend se daba por caido estando sano.
        Invoke-RestMethod "http://127.0.0.1:$port/health" -TimeoutSec 2 | Out-Null
        $ok = $true
    } catch {
        Start-Sleep -Milliseconds 500
    }
}
if ($ok) {
    Write-Host ("  OK    go       http://localhost:{0}/health" -f $port) -ForegroundColor Green
} else {
    Write-Host '  FALLO go       no respondio; mira logs\go.err.log' -ForegroundColor Red
}

Write-Host ''
Write-Host "Logs en $logs  (Get-Content logs\go.log -Wait para seguirlo)" -ForegroundColor DarkGray
Write-Host 'Para pararlo todo:  .\start.ps1 -Stop' -ForegroundColor DarkGray
if ($Frontend) { Write-Host 'Frontend: http://localhost:4200 (tarda ~20 s la primera vez)' -ForegroundColor DarkGray }
