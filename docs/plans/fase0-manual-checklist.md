# Fase 0 — Checklist de pruebas manuales

Complemento de la Fase 0 de [`operability-toolkit.md`](operability-toolkit.md)
(B12 race, B1 validación de config, **A5 rotación de logs + niveles + `unarr logs`**).

**Alcance: SOLO lo que no se puede automatizar en este repo.** Todo lo que un test
de Go o el CI ya cubre queda fuera a propósito. Lo que queda aquí depende de un
service manager real (Task Scheduler, launchd, systemd/journald), de un kernel con
uid/gid ajenos (Docker/NAS) o de un ojo humano (render en terminal). En concreto,
lo que un test **no** puede demostrar:

- Que `RotateNow` (copy-truncate) **encoge de verdad** un fichero cuyo descriptor
  lo sujeta **otro** proceso: `cmd.exe` con `>> unarr.log` en Windows, `launchd`
  con `StandardOutPath` en macOS, el proceso padre en `startDaemonDetached`.
  En un test de Go el escritor y el rotador son el mismo proceso — el caso fácil.
- Que truncar bajo ese descriptor no rompe la **supervisión** del daemon.
- Que los ficheros rotados nacen con el **uid/gid correcto** al bajar privilegios.
- Que bajo systemd no hay **doble logging** (journal + fichero).
- Que `unarr doctor` **se lee** en 80 columnas y sin color.

## Cómo usar este documento

Cada ítem trae **Pasos**, **Resultado esperado** y **Cómo saber que falló**.
Marca `[x]` sólo con evidencia (salida pegada), no de memoria. Pega el bloque
relevante en el PR/commit. Un ítem que no se pueda ejecutar se marca `SKIP` con
el motivo — nunca PASS.

## Rutas y nombres que se citan abajo

| Plataforma | Directorio de datos (`config.DataDir()`) |
|---|---|
| Linux | `$XDG_DATA_HOME/unarr` o `~/.local/share/unarr` |
| macOS | `~/Library/Application Support/unarr` (mismo dir que la config) |
| Windows | `%LOCALAPPDATA%\unarr` |
| Docker | `/data/unarr` (la imagen fija `XDG_DATA_HOME=/data`) |

Dentro de ese directorio: `unarr.log` (vivo), `unarr.log.1 … .N` (rotados),
`unarr.err.log` (sólo macOS/launchd), `daemon.state.json`, `daemon.stopped`
(marca de parada intencionada), y en Windows `unarr-launch.vbs` + `unarr-task.xml`.

## Preparación común (todas las plataformas)

**La rotación está DESACTIVADA por defecto** (`log_max_size_mb = 0`): es opt-in, y
con el valor por defecto ninguna de las pruebas de rotación de este documento
tiene nada que observar — no rota ni `unarr.log`, ni `unarr.err.log`, ni el anillo
del boot log, ni el trim del shim VBScript. **Actívala antes de empezar** en el
`config.toml` del host bajo prueba:

```toml
[daemon]
log_max_size_mb = 1
log_max_files   = 3
log_level       = "info"
log_format      = "text"
```

El barrido del daemon (`logging.Sweep`) comprueba el tamaño **una vez por minuto**,
así que tras cruzar el presupuesto hay que esperar hasta ~60 s antes de declarar un
fallo. Para forzarlo sin esperar existe `unarr logs rotate`.

Restaura el `log_max_size_mb` original (`0`) al terminar cada sección.

En Windows, el trim del boot log vive dentro del shim VBScript y su umbral se
**hornea en `unarr daemon install`**: escribe el `config.toml` de arriba *antes*
de instalar, o el shim saldrá generado sin trim ninguno.

---

# 1. Windows — arranque del daemon con el log rotando

Requiere la VM del harness. **La VM YA EXISTE** (`test/windows/`, Windows 11
instalado en el volumen docker `windows_unarr_win_storage`, arranca en ~1 min).
No descargues ISOs, no crees otra VM, no toques libvirt.
Referencia: [`test/windows/README.md`](../../test/windows/README.md).

Lee además los *gotchas* del README antes de escribir cualquier `.ps1`
(UTF-8 con BOM + CRLF, sin here-strings, config sin BOM, nada de `Get-Date` para
medir tiempos).

## W0 — Arrancar la VM y desplegar binarios

**Pasos**

```bash
cd /home/buryni/Proyectos/torrentclaw/torrentclaw-cli/test/windows
./run.sh                 # compila unarr.exe + unarr-desktop.exe y levanta la VM
docker compose ps        # estado
docker compose logs -f   # progreso de arranque (Ctrl-C cuando esté arriba)
```

Entra por noVNC <http://localhost:8006> o RDP `localhost:3389`
(usuario `tester` / `unarrtest` — VM desechable, nunca credenciales reales).
En el invitado, `\\host.lan\Data` es `./shared/` del host.

Copia el binario a disco local (no lo ejecutes desde el share de red: la tarea
programada arranca antes de que el share esté montado):

```powershell
New-Item -ItemType Directory -Force C:\unarr | Out-Null
Copy-Item \\host.lan\Data\unarr.exe C:\unarr\ -Force
netsh advfirewall firewall add rule name=unarr dir=in action=allow program=C:\unarr\unarr.exe enable=yes
C:\unarr\unarr.exe --version
```

**Resultado esperado.** `docker compose ps` muestra el contenedor `running`; el
escritorio de Windows responde; `unarr.exe --version` imprime una versión.

**Cómo saber que falló.** `./run.sh` tarda >5 min en el arranque (mira
`ls -l /dev/kvm` y la pertenencia al grupo `kvm`: sin KVM cae a TCG y es
inservible). Si Windows pide instalarse de cero es que alguien hizo
`docker compose down -v` — **para y avisa**, no reinstales por tu cuenta.

Al terminar TODA la sección 1: `docker compose down` (conserva el disco).
**Nunca** `down -v`.

## W1 — `startDaemonDetached`: el log se abre y se escribe

Este es el camino sin service manager (el "arráncalo ahora" del asistente):
el padre abre `unarr.log`, se lo pasa al hijo como descriptor heredado y se muere.

**Pasos** (PowerShell en el invitado)

```powershell
$data = "$env:LOCALAPPDATA\unarr"
Remove-Item "$data\unarr.log*" -Force -ErrorAction SilentlyContinue
# Deja log_max_size_mb = 1 en %APPDATA%\unarr\config.toml (ver Preparación común).
C:\unarr\unarr.exe up          # camino del asistente que llama a startDaemonDetached
Start-Sleep -Seconds 5
Get-Item "$data\unarr.log" | Select-Object Length, LastWriteTime
Get-Content "$data\unarr.log" -Tail 20
Get-Content "$data\daemon.state.json" -Raw
```

**Resultado esperado.** `unarr.log` existe, `Length` > 0 y crece entre dos
lecturas. El comando **no** devuelve el error "the daemon exited immediately
after starting" (el padre espera 1500 ms y, si el hijo muere, imprime el tail del
log). `daemon.state.json` nombra un PID que existe (`Get-Process -Id <pid>`).

**Cómo saber que falló.**
- `unarr.log` no aparece o se queda a 0 bytes → el descriptor no llegó al hijo.
- Sale "the daemon exited immediately after starting (…)" con un tail vacío → el
  log se abrió pero el hijo no escribió nada; mira el `exit status` que acompaña.
- El PID de `daemon.state.json` no existe → el daemon murió tras registrarse (ojo:
  el fichero de estado sobrevive a la muerte, así que "existe el fichero" NO es
  "el daemon está vivo" — hay que casar el PID).

## W2 — Instalación de la tarea programada y arranque en el logon

**Pasos**

```powershell
C:\unarr\unarr.exe stop
C:\unarr\unarr.exe daemon install
schtasks /query /tn unarr /xml
Get-Content "$env:LOCALAPPDATA\unarr\unarr-launch.vbs" -Encoding Unicode -TotalCount 5
Format-Hex "$env:LOCALAPPDATA\unarr\unarr-launch.vbs" -Count 4
```

Después: **cerrar sesión y volver a entrar** (no `schtasks /run`; el objetivo es
el disparador de logon real).

```powershell
Start-Sleep -Seconds 45
schtasks /query /tn unarr /v /fo LIST | Select-String "Status|Last Result"
Get-Process wscript -ErrorAction SilentlyContinue
Get-Content "$env:LOCALAPPDATA\unarr\unarr.log" -Tail 20
```

**Resultado esperado.** El XML lleva `<Delay>PT20S</Delay>`, `<RestartOnFailure>`
y `<StartWhenAvailable>true`. El `.vbs` empieza por los bytes `FF FE` (UTF-16LE +
BOM) y se lee legible con `-Encoding Unicode`. Tras el logon: la tarea en
`Status: Running`, un proceso `wscript` vivo, un `unarr.exe` vivo, `unarr.log`
creciendo y **ninguna ventana de consola negra** en pantalla.

**Cómo saber que falló.**
- El `.vbs` sale como mojibake o el hex no empieza por `FF FE` → se escribió en
  UTF-8; con un usuario Windows no-ASCII el daemon no arrancaría nunca en el logon.
- Tras el logon la tarea queda en `Status: Ready` con `Last Result: 1` y no hay
  `wscript` → el shim salió; mira el final de `unarr.log` para ver por qué.
- Aparece una ventana de consola (aunque parpadee) → regresión de supresión de
  consola, no del log.

## W3 — La rotación funciona con `cmd.exe` sujetando el redirect (el caso crítico)

El shim lanza `cmd /c ""unarr.exe" start >> "…\unarr.log" 2>&1"`. Ese `cmd.exe`
es el dueño del descriptor durante toda la sesión, así que renombrar el fichero
no encogería nada: la rotación tiene que ser **copy-truncate**, hecha desde fuera
por el janitor del propio daemon. **Esto es lo único que sólo se puede comprobar
aquí.**

**Pasos** (con el daemon corriendo bajo la tarea, tras W2)

```powershell
$data = "$env:LOCALAPPDATA\unarr"
# 1. Engorda el log por encima del presupuesto SIN tocar el escritor: escribe
#    desde otro proceso en modo append, igual que hace cmd.exe.
$fs = [System.IO.File]::Open("$data\unarr.log",'Append','Write','ReadWrite')
$sw = New-Object System.IO.StreamWriter($fs)
1..20000 | ForEach-Object { $sw.WriteLine("filler line $_ ................................................") }
$sw.Close(); $fs.Close()
Get-Item "$data\unarr.log" | Select-Object Length      # debe superar 1 MB

# 2. Espera al barrido del daemon (1 min) — NO uses Get-Date para medir.
$sw2 = [System.Diagnostics.Stopwatch]::StartNew()
while ($sw2.Elapsed.TotalSeconds -lt 150 -and (Get-Item "$data\unarr.log").Length -ge 1MB) { Start-Sleep 5 }
$sw2.Elapsed.TotalSeconds

# 3. Estado del anillo
Get-ChildItem "$data\unarr.log*" | Select-Object Name, Length
```

**Resultado esperado.** En menos de ~150 s: `unarr.log` ha bajado por debajo de
1 MB, existe `unarr.log.1` con el contenido anterior, y `unarr.log` **sigue
creciendo** después (el `cmd.exe` del shim reabre el offset en 0 porque el
redirect es `>>`, modo append). Con más vueltas aparecen `.2` y `.3`, y **nunca**
un `.4` (`log_max_files = 3`).

**Cómo saber que falló.**
- `unarr.log` no baja de tamaño y no aparece `.1` → el truncado falló. Causa más
  probable en Windows: **violación de compartición** al truncar un fichero que
  `cmd.exe` tiene abierto. Confírmalo dejando el daemon parado y ejecutando
  `unarr logs rotate` a mano: si con el daemon parado sí rota y con el daemon
  vivo no, es exactamente ese fallo.
- `unarr.log.1` aparece pero `unarr.log` se queda **congelado a 0 bytes** para
  siempre → el escritor conservó el offset viejo (o el rename ganó al
  copy-truncate): el daemon está escribiendo en un agujero y perdiste todo el log
  a partir de ese momento. Es el fallo más grave posible de esta fase.
- Aparece `unarr.log.4` o el anillo crece sin límite → `log_max_files` no se está
  respetando en este camino.

## W4 — La rotación no rompe la supervisión del daemon

**Pasos** (encadenado con W3, con el log ya rotando)

```powershell
$data = "$env:LOCALAPPDATA\unarr"
$before = (Get-Content "$data\daemon.state.json" -Raw)
Get-Process unarr | Stop-Process -Force          # muerte simulada, NO `unarr stop`
$sw3 = [System.Diagnostics.Stopwatch]::StartNew()
while ($sw3.Elapsed.TotalSeconds -lt 180 -and -not (Get-Process unarr -ErrorAction SilentlyContinue)) { Start-Sleep 5 }
$sw3.Elapsed.TotalSeconds
Get-Process unarr | Select-Object Id, StartTime
(Get-Content "$data\daemon.state.json" -Raw)     # el PID debe ser OTRO
Get-Content "$data\unarr.log" -Tail 30
Get-ChildItem "$data\unarr.log*" | Select-Object Name, Length
```

**Resultado esperado.** El daemon vuelve solo en ~15 s (primer reintento del
backoff del shim: 15 s, 30 s, 60 s, 120 s, tope 120 s). El PID de
`daemon.state.json` cambia respecto al de antes. El log **nuevo** se sigue
escribiendo en el mismo `unarr.log`, con el anillo intacto. La tarea sigue en
`Status: Running` y `wscript` nunca desapareció.

**Cómo saber que falló.**
- No vuelve en 3 minutos y `wscript` ya no está → el shim salió; comprueba si
  existe `%LOCALAPPDATA%\unarr\daemon.stopped` (si existe sin que nadie pidiera
  parar, la marca de intención está mal gestionada).
- Vuelve pero deja de escribir en `unarr.log` → el relanzamiento perdió el
  redirect; suele delatarlo el hecho de que el log se quedó en el tamaño exacto
  del momento del kill.
- Comparar sólo "existe `daemon.state.json`" en vez del PID da falsos fallos: ese
  fichero sobrevive a la muerte del proceso.

## W5 — Parada intencionada con el log rotando

**Pasos**

```powershell
C:\unarr\unarr.exe stop
Start-Sleep -Seconds 60
Get-Process unarr -ErrorAction SilentlyContinue      # vacío
Test-Path "$env:LOCALAPPDATA\unarr\daemon.stopped"   # True
schtasks /query /tn unarr /v /fo LIST | Select-String "Status|Last Result"
```

**Resultado esperado.** El daemon **no** vuelve. Existe `daemon.stopped`. La tarea
termina con `Last Result: 0` (parada deliberada, no fallo).

**Cómo saber que falló.** El daemon reaparece a los 15-120 s → el shim no vio la
marca; o `Last Result` distinto de 0 → una parada pedida se está reportando como
fallo y contaminará el historial de la tarea.

## W6 — `unarr logs` lee el anillo completo tras rotar

**Pasos**

```powershell
C:\unarr\unarr.exe logs -n 5
C:\unarr\unarr.exe logs -n 100000 | Measure-Object -Line     # debe cruzar a .1/.2
C:\unarr\unarr.exe daemon logs -n 5                          # alias histórico
C:\unarr\unarr.exe logs --level warn -n 50
C:\unarr\unarr.exe logs --since 30m -n 50
C:\unarr\unarr.exe logs --grep "daemon|start" -n 50
C:\unarr\unarr.exe logs rotate
```

**Resultado esperado.** `unarr logs` y `unarr daemon logs` producen exactamente la
misma salida. Con `-n` muy grande el número de líneas supera lo que cabe en el
`unarr.log` vivo (está leyendo los rotados). `--level`, `--since` y `--grep`
recortan la salida sin error. `logs rotate` no falla aunque no haya nada que rotar.

**Cómo saber que falló.**
- El alias `daemon logs` da otra salida o le falta alguna flag → los dos comandos
  se han desincronizado.
- Con `-n 100000` el conteo se detiene justo en el tamaño del fichero vivo → no
  está entrando en los rotados.
- Sale "no daemon log yet at …" pese a existir `unarr.log.1` → la comprobación de
  existencia del anillo no mira los rotados.

## W7 — Desinstalación limpia

**Pasos**

```powershell
C:\unarr\unarr.exe daemon uninstall
schtasks /query /tn unarr
Get-ChildItem "$env:LOCALAPPDATA\unarr\"
```

**Resultado esperado.** `schtasks /query` responde que la tarea no existe; el
`.vbs` ya no está. Los logs **sí** siguen ahí (desinstalar el servicio no es
`unarr clean`).

**Cómo saber que falló.** La tarea sigue registrada, o desaparecieron los logs
(borrar historial de diagnóstico en un uninstall es un fallo, no una limpieza).

---

# 2. macOS — rutas del log y alias `unarr daemon logs`

No hay VM de macOS en este repo: hace falta un Mac real. Marcar `SKIP` si no lo hay.

## M1 — El log vive en `~/Library`, nunca en `~/.local/share`

En macOS `DataDir()` devuelve el **mismo** directorio que la config
(`~/Library/Application Support/unarr`), a diferencia de Linux. Un `unarr.log`
apareciendo en `~/.local/share/unarr` significa que alguien resolvió la ruta con
la lógica de Linux.

**Pasos**

```bash
rm -f ~/.local/share/unarr/unarr.log*        # asegura que partimos sin ese fichero
unarr up
sleep 5
ls -l "$HOME/Library/Application Support/unarr/"
ls -l "$HOME/.local/share/unarr/" 2>/dev/null
unarr logs -n 5
```

**Resultado esperado.** `unarr.log` (creciendo) en
`~/Library/Application Support/unarr/`. `~/.local/share/unarr/` no existe o no
contiene ningún `unarr.log*`. `unarr logs` imprime esas mismas líneas.

**Cómo saber que falló.** Aparece `unarr.log` bajo `~/.local/share/unarr` → hay un
camino que no usa `config.DataDir()`. Aún peor si existen **los dos**: el daemon
escribe en uno y `unarr logs` lee el otro, y el usuario ve un log vacío mientras el
disco se llena por el otro lado.

## M2 — Rotación con launchd sujetando `StandardOutPath`

El plist apunta `StandardOutPath` a `<LogDir>/unarr.log`; **launchd** abre y
retiene ese descriptor. Mismo caso crítico que W3, distinto dueño del fd.

**Pasos**

```bash
unarr stop
unarr daemon install
launchctl list | grep com.torrentclaw.unarr
D="$HOME/Library/Application Support/unarr"
# Engorda el log desde fuera, en modo append
yes "filler line ................................................" | head -20000 >> "$D/unarr.log"
ls -l "$D"/unarr.log*
sleep 90
ls -l "$D"/unarr.log*
tail -5 "$D/unarr.log"
```

**Resultado esperado.** Tras el barrido: `unarr.log` por debajo de 1 MB, existe
`unarr.log.1`, y `unarr.log` **vuelve a crecer** con líneas nuevas del daemon.

**Cómo saber que falló.** `unarr.log` no encoge, o encoge y se queda a 0 para
siempre (launchd conservó el offset viejo). Compruébalo con
`lsof "$D/unarr.log"`: si el offset del descriptor de launchd sigue apuntando muy
por encima del tamaño del fichero, el log está roto aunque el `.1` exista.

## M3 — `unarr daemon logs` es el mismo comando que `unarr logs`

**Pasos**

```bash
unarr logs -n 20 > /tmp/a.txt
unarr daemon logs -n 20 > /tmp/b.txt
diff /tmp/a.txt /tmp/b.txt && echo IDENTICO
unarr daemon logs --level warn --since 1h --grep 'daemon' -n 10
unarr daemon logs -f      # Ctrl-C tras ver una línea nueva
```

**Resultado esperado.** `diff` vacío. El alias acepta las mismas flags
(`-f/--follow`, `-n/--lines`, `--since`, `--level`, `--grep`). `-f` transmite
líneas nuevas y **Ctrl-C sale con código 0**, no con un error.

**Cómo saber que falló.** El alias rechaza alguna flag ("unknown flag") → el alias
dejó de compartir el binding de flags. Ctrl-C en `-f` devuelve un error o un stack
trace → la cancelación por contexto se está tratando como fallo.

## M4 — `unarr.err.log` queda FUERA del anillo (limitación conocida)

El plist manda stderr a un fichero aparte (`unarr.err.log`) que la rotación
**no** cubre — sólo `unarr clean` lo borra.

**Pasos**

```bash
D="$HOME/Library/Application Support/unarr"
ls -l "$D/unarr.err.log" 2>/dev/null
unarr logs -n 50 | grep -i "panic\|fatal" ; echo "exit=$?"
```

**Resultado esperado / a documentar.** Si `unarr.err.log` existe y crece, anota su
tamaño: es crecimiento **no acotado** y contenido que `unarr logs` **no** muestra.
Reporta la medida; no lo des por bueno en silencio.

**Cómo saber que falló.** `unarr.err.log` con cientos de MB en una instalación
normal → la rotación no está acotando el disco de verdad en macOS, y la Fase 0
está incompleta.

---

# 3. Docker / NAS — la rotación respeta PUID/PGID y no deja ficheros root

El entrypoint baja privilegios a `PUID:PGID` con `gosu`. Los ficheros rotados los
crea el **proceso ya sin privilegios**, así que deben heredar ese uid/gid. Un
`unarr.log.1` propiedad de root en un NAS es un fichero que el usuario no puede
borrar ni desde el gestor de ficheros ni por SSH.

## D1 — Rotación con PUID/PGID típicos de NAS

**Pasos** (host Linux con docker; usa un uid/gid que NO sea el tuyo, p. ej. unRAID 99:100)

```bash
mkdir -p /tmp/unarr-cfg /tmp/unarr-dl
printf '[daemon]\nlog_max_size_mb = 1\nlog_max_files = 3\n' > /tmp/unarr-cfg/config.toml
docker run -d --name unarr-rot \
  -e PUID=99 -e PGID=100 \
  -e UNARR_API_KEY="$UNARR_API_KEY" \
  -v /tmp/unarr-cfg:/config -v /tmp/unarr-dl:/downloads -v unarr-rot-data:/data \
  unarr/cli:latest start

sleep 20
docker exec unarr-rot sh -c 'ls -ln /data/unarr/'
# Engorda el log desde dentro, en append, sin ser el escritor
docker exec unarr-rot sh -c 'i=0; while [ $i -lt 20000 ]; do echo "filler ........................................"; i=$((i+1)); done >> /data/unarr/unarr.log'
docker exec unarr-rot sh -c 'ls -ln /data/unarr/'
sleep 90
docker exec unarr-rot sh -c 'ls -ln /data/unarr/'
docker exec unarr-rot sh -c 'id'
```

**Resultado esperado.** `ls -ln` muestra **`99 100`** en `unarr.log` y en **todos**
los `unarr.log.N`, con modo `-rw-r--r--`. `id` dentro del contenedor dice
`uid=99 gid=100`. `unarr.log` ha encogido y existe `unarr.log.1`.

**Cómo saber que falló.**
- Cualquier `unarr.log.N` con owner `0 0` → un fichero rotado creado como root:
  el usuario del NAS no podrá borrarlo. Es exactamente el callejón sin salida que
  `PUID/PGID` existe para evitar.
- El fichero vivo es `99 100` pero el rotado no → el rotador corre en otro
  contexto de privilegio que el escritor.
- Modo distinto de `0644` → revisa el `umask` de la imagen; un `0600` rompe la
  lectura del log desde el host.

## D2 — Con volumen bind en el host (el caso real del NAS)

**Pasos**

```bash
docker rm -f unarr-rot
mkdir -p /tmp/unarr-data && sudo chown 99:100 /tmp/unarr-data
docker run -d --name unarr-rot2 -e PUID=99 -e PGID=100 \
  -e UNARR_API_KEY="$UNARR_API_KEY" \
  -v /tmp/unarr-cfg:/config -v /tmp/unarr-dl:/downloads -v /tmp/unarr-data:/data \
  unarr/cli:latest start
sleep 20
docker exec unarr-rot2 sh -c 'i=0; while [ $i -lt 20000 ]; do echo "filler ........................................"; i=$((i+1)); done >> /data/unarr/unarr.log'
sleep 90
ls -ln /tmp/unarr-data/unarr/          # visto DESDE EL HOST
rm -f /tmp/unarr-data/unarr/unarr.log.1 ; echo "borrado_sin_sudo=$?"
```

**Resultado esperado.** Desde el host, todos los ficheros del anillo pertenecen a
`99:100`. Si tu usuario del host es ese uid, puedes borrar `unarr.log.1` **sin
sudo**.

**Cómo saber que falló.** `rm` sin sudo devuelve "Operation not permitted" sobre un
fichero rotado → hay root en el camino de rotación. Es el fallo que este ítem busca.

## D3 — `PUID=0` sigue siendo posible, pero avisado

**Pasos**

```bash
docker run --rm -e PUID=0 -e UNARR_API_KEY=x unarr/cli:latest --version 2>&1 | head -3
```

**Resultado esperado.** Sale por stderr `unarr: PUID=0 — running as root by
explicit request.` y el binario funciona.

**Cómo saber que falló.** No hay aviso (root silencioso), o el contenedor se niega
a arrancar pese a pedirse root explícitamente.

**Limpieza:** `docker rm -f unarr-rot unarr-rot2; docker volume rm unarr-rot-data`.

---

# 4. systemd — rotación propia vs journald (doble logging)

Bajo systemd la unidad **no** declara `StandardOutput=`, así que la salida va al
**journal** y no existe ningún `unarr.log` que rotar. `unarr logs` lo detecta
(`usesJournald()`: Linux + unidad instalada en disco) y delega en `journalctl`.
Lo que hay que demostrar manualmente es que **no se escriben las dos cosas a la
vez** y que un `unarr.log` viejo no ensombrece al journal.

## S1 — Con la unidad instalada, sólo escribe el journal

**Pasos**

```bash
unarr stop
unarr daemon install
systemctl --user is-active unarr
D="${XDG_DATA_HOME:-$HOME/.local/share}/unarr"
ls -l "$D"/unarr.log* 2>/dev/null; echo "---"
journalctl --user -u unarr -n 20 --no-pager
sleep 60
ls -l "$D"/unarr.log* 2>/dev/null
journalctl --user -u unarr -n 5 --no-pager
```

**Resultado esperado.** El journal recibe líneas nuevas. **No** se crea ningún
`unarr.log` nuevo, y si existía uno de antes su tamaño **no cambia**. Nada de
doble logging: cada línea aparece una sola vez, en el journal.

**Cómo saber que falló.**
- `unarr.log` crece **a la vez** que el journal → doble escritura: el disco paga
  dos veces y el journal y el fichero divergen.
- Ni el journal ni el fichero crecen → la salida se está perdiendo.

## S2 — Un `unarr.log` obsoleto no tapa al journal

Caso real: el usuario probó `unarr up` antes de instalar el servicio, y quedó un
`unarr.log` viejo en el directorio de datos.

**Pasos**

```bash
D="${XDG_DATA_HOME:-$HOME/.local/share}/unarr"
echo "LINEA-FANTASMA-DE-HACE-SEMANAS" >> "$D/unarr.log"
unarr logs -n 20
unarr logs -n 20 | grep -c "LINEA-FANTASMA-DE-HACE-SEMANAS"
```

**Resultado esperado.** `unarr logs` muestra el **journal**, y el contador de la
línea fantasma es **0**.

**Cómo saber que falló.** Aparece `LINEA-FANTASMA-DE-HACE-SEMANAS` → el lector de
ficheros ganó al journal y el usuario está diagnosticando con un log muerto.

## S3 — Sin unidad instalada, vuelve al fichero

**Pasos**

```bash
unarr stop
unarr daemon uninstall
ls -l ~/.config/systemd/user/unarr.service 2>/dev/null   # no debe existir
unarr up
sleep 10
unarr logs -n 20
ls -l "${XDG_DATA_HOME:-$HOME/.local/share}/unarr"/unarr.log*
```

**Resultado esperado.** Sin fichero de unidad, `unarr logs` lee `unarr.log` (no
`journalctl`) y ese fichero crece.

**Cómo saber que falló.** `unarr logs` sigue llamando a `journalctl` sin unidad
instalada (saldría vacío o con "No entries") → la detección se hace por otra vía
que el artefacto en disco, y queda pegada al estado anterior.

## S4 — `unarr logs rotate` bajo systemd es un no-op honesto

**Pasos**

```bash
unarr daemon install
unarr logs rotate; echo "exit=$?"
```

**Resultado esperado.** Sale con código **0** y sin ruido: no hay fichero que
rotar porque todo va al journal.

**Cómo saber que falló.** Devuelve error, o crea un `unarr.log` vacío para poder
rotarlo (efecto secundario indeseado).

## S5 — La retención del journal la manda journald, no unarr

**Pasos**

```bash
journalctl --user -u unarr --disk-usage
grep -E "SystemMaxUse|MaxRetentionSec" /etc/systemd/journald.conf
```

**Resultado esperado / a documentar.** Bajo systemd, `log_max_size_mb` y
`log_max_files` **no aplican**. Anota el uso en disco y confirma que journald
tiene su propio tope. Documenta explícitamente en la nota del PR que en systemd la
retención es de journald — si no, un usuario creerá que `log_max_size_mb = 1`
acota su disco y no acota nada.

**Cómo saber que falló.** Que alguien afirme en docs o en la salida del CLI que
`log_max_size_mb` limita el log en un sistema systemd.

---

# 5. `unarr doctor` — legibilidad en terminal estrecha y sin color

`doctor` imprime `  + Nombre — mensaje` con el mensaje libre (rutas absolutas,
URLs, versiones). En 80 columnas esas líneas se parten, y sin color desaparece la
única señal de gravedad si los marcadores `+ / ! / x` no bastan por sí solos.

## T1 — 80 columnas

**Pasos**

```bash
printf '\e[8;40;80t'          # redimensiona el terminal a 80x40 (xterm/gnome-terminal)
stty size                      # confirma "40 80"
unarr doctor
```

**Resultado esperado.** Ninguna línea se corta a mitad de una palabra de forma
que el estado quede ilegible. Los marcadores `+`, `!`, `x` quedan siempre al
principio de línea (nunca arrastrados a la continuación). Las cabeceras de sección
(`Config`, etc.) siguen distinguiéndose. El resumen final
(`N passed, N failed, N warnings`) cabe en una línea.

**Cómo saber que falló.** Una ruta larga envuelve y la continuación empieza con
algo que parece un nuevo check; o el resumen se parte en dos y se lee como dos
resultados distintos.

## T2 — 60 columnas (caso pesimista: consola de NAS, tmux partido)

**Pasos**

```bash
printf '\e[8;40;60t'; stty size
unarr doctor
unarr doctor --fix --dry-run
```

**Resultado esperado.** Sigue siendo posible saber, de un vistazo, cuántos checks
pasaron y **cuáles** fallaron. El plan de `--fix --dry-run` se lee sin adivinanzas.

**Cómo saber que falló.** Hay que hacer scroll horizontal, o el texto envuelto
hace imposible asociar un mensaje con su check.

## T3 — Sin color (`--no-color` y `NO_COLOR`)

**Pasos**

```bash
unarr doctor --no-color
NO_COLOR=1 unarr doctor
unarr doctor --no-color | cat -v | grep -c $'\033'      # debe dar 0
unarr doctor --no-color > /tmp/doc.txt; grep -c $'\033\[' /tmp/doc.txt   # 0
unarr doctor | cat                                       # pipe: sin TTY
```

**Resultado esperado.** Cero secuencias ANSI en la salida con `--no-color` y con
`NO_COLOR=1`. Aun así se distingue PASS de WARN de FAIL **sólo por el texto**
(`+` / `!` / `x` y el resumen final). Redirigido a fichero, la salida es texto
plano legible — pegable en un issue de GitHub.

**Cómo saber que falló.**
- `grep -c $'\033\['` devuelve algo distinto de 0 → hay un `Print` que no pasa por
  el paquete de color y se salta `--no-color`.
- Sin color, dos líneas de estado distinto son indistinguibles → la información
  estaba **sólo** en el color; hay que reforzar el marcador textual.
- `NO_COLOR=1` no surte efecto pero `--no-color` sí (o al revés) → uno de los dos
  caminos no llega a `color.NoColor`.

## T4 — Sin TTY (CI, `docker logs`, bundle de soporte)

**Pasos**

```bash
docker exec unarr-rot unarr doctor 2>&1 | head -40
unarr doctor 2>&1 | tee /tmp/doctor-notty.txt | tail -5
```

**Resultado esperado.** Salida limpia, sin ANSI ni caracteres de control, y
completa (no truncada por detección de terminal).

**Cómo saber que falló.** Basura `[0m` incrustada en el fichero, o secciones que
desaparecen cuando no hay TTY.

---

# Registro de resultados

| ID | Ítem | Plataforma | Resultado | Evidencia / notas |
|---|---|---|---|---|
| W0 | VM arriba, binarios desplegados | Windows | | |
| W1 | `startDaemonDetached` abre y escribe el log | Windows | | |
| W2 | Tarea programada + arranque en logon | Windows | | |
| W3 | **Rotación con `cmd.exe` sujetando el redirect** | Windows | | |
| W4 | La rotación no rompe la supervisión | Windows | | |
| W5 | Parada intencionada con el log rotando | Windows | | |
| W6 | `unarr logs` lee el anillo completo | Windows | | |
| W7 | Desinstalación limpia | Windows | | |
| M1 | Log en `~/Library`, no en `~/.local/share` | macOS | | |
| M2 | Rotación con launchd sujetando el fd | macOS | | |
| M3 | Alias `unarr daemon logs` idéntico | macOS | | |
| M4 | `unarr.err.log` fuera del anillo (medir) | macOS | | |
| D1 | Rotados con PUID/PGID correctos | Docker | | |
| D2 | Bind mount: borrables sin sudo | Docker/NAS | | |
| D3 | `PUID=0` avisado | Docker | | |
| S1 | Sólo journal, sin doble logging | systemd | | |
| S2 | `unarr.log` viejo no tapa el journal | systemd | | |
| S3 | Sin unidad → vuelve al fichero | systemd | | |
| S4 | `logs rotate` no-op con exit 0 | systemd | | |
| S5 | Retención = journald (documentar) | systemd | | |
| T1 | `doctor` legible en 80 columnas | cualquiera | | |
| T2 | `doctor` legible en 60 columnas | cualquiera | | |
| T3 | `doctor --no-color` / `NO_COLOR` sin ANSI | cualquiera | | |
| T4 | `doctor` sin TTY | cualquiera | | |

Cerrar la Fase 0 exige: W1-W7 y S1-S4 en PASS; D1-D2 en PASS; T1-T4 en PASS.
M1-M4 pueden quedar `SKIP` si no hay Mac disponible, pero el SKIP debe constar en
las notas de la release — la rotación bajo launchd es el único camino de macOS y
queda sin verificar.
