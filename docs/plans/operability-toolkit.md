# Plan: Operability toolkit — diagnóstico, observabilidad y control

Status: **propuesto** (2026-08-03) — sin código.
Owner: CLI
Alcance: Parte A (troubleshooting toolkit) + Parte B (10 mejoras de operabilidad).
Fuera de alcance en este plan: shim qBittorrent, indexer Torznab, shim SABnzbd,
multi-viewer, relay propio para el funnel. (Sesión aparte.)

## Por qué

El agente hace mucho en automático (elige método, decide direct-play vs transcode,
dimensiona readahead, tonemapea, quema subs, limpia la biblioteca) y expone muy poco
de *por qué*. Cuando algo falla, el usuario sólo tiene `unarr doctor` — que comprueba
**estado de configuración**, no la **cadena de reproducción** — y un log plano sin
rotación. Cada ticket empieza en cero.

Este plan cierra tres huecos: **diagnosticar** (A1–A3), **explicar** (A4, A5) y
**observar/controlar** (B).

## Restricción de arquitectura

Todo fichero nuevo por debajo de 500 líneas, una responsabilidad, `make arch` verde
(ver [CLAUDE.md](../../CLAUDE.md)). Los splits de fichero van especificados por
adelantado en cada ítem — no "lo partimos luego". `internal/cmd/` ya tiene ~17k líneas;
la lógica nueva vive en paquetes `internal/*` y `internal/cmd/` sólo cablea Cobra.

---

# Parte A — Troubleshooting toolkit

## A1. `unarr doctor` — cerrar los huecos actuales

**Problema.** [doctor.go](../../internal/cmd/doctor.go) tiene 12 checks y ninguno
cubre las causas de fallo más frecuentes.

### A1.1 ffmpeg / ffprobe — **el hueco grave**

Cero menciones de `ffmpeg` en `doctor.go`. Todo el path de streaming (HLS, thumbnails,
trickplay, `library stats --quality`, burn-in de subs) depende de él. Un host sin
ffmpeg pasa `doctor` con *"All checks passed"* y luego no reproduce nada.

Checks a añadir, bloque nuevo **`Media`**:

| Check | Resultado | Severidad si falla |
|---|---|---|
| `ffmpeg` presente | ruta + versión | **FAIL** |
| `ffprobe` presente | ruta + versión | **FAIL** |
| encoders requeridos | `libx264`, `aac` | **FAIL** |
| `zscale` disponible | sí/no | **WARN** (sin él no hay tonemap HDR→SDR) |
| hwaccel | resumen de `probe-hwaccel` (1 línea) | **WARN** si ninguno |
| techo de transcode | resultado cacheado de `unarr bench` (ver B2) | informativo |

- Reusar `FFmpegSupportsZscale` y el detector de hwaccel de
  [hwaccel.go](../../internal/engine/hwaccel.go) — **no** re-implementar.
- Nuevo fichero `internal/cmd/doctor_media.go` (~120 líneas).
- `doctor --fix` ya contempla "ffmpeg fetch" como TODO de guidance; aquí sólo se
  detecta y se guía. La descarga automática queda fuera de este plan.

### A1.2 Validación de config — claves desconocidas

Ver **B1**. El check en doctor consume el resultado de `config.Load`.

### A1.3 Puerto de stream bindable + alcanzable

`stream_port` (11818) y `https_stream_port` (11819) no se comprueban nunca.

- Bind test en `127.0.0.1:<port>` → si `EADDRINUSE`, decir **qué proceso** lo tiene
  (reusar el lookup de PID de [internal/winproc/](../../internal/winproc/) en Windows,
  `/proc` en Linux).
- Si el daemon está vivo: `GET http://<lan-ip>:<port>/health` desde la propia máquina
  usando la IP de LAN (no loopback) → detecta firewall local, que es el fallo real
  cuando el broker web no alcanza al agente.
- `WARN` (no FAIL) si el daemon no corre: no hay nada que bindear.

### A1.4 Desfase de reloj (clock skew)

Ya anotado como riesgo en
[unarr-agent-roadmap.md](../../Docs/plans/unarr-agent-roadmap.md): el token HLS lo
mintea el web y lo verifica el agente, con relojes distintos y sin tolerancia.

- Comparar el reloj local con el header `Date` de la respuesta del API (ya se hace una
  petición en el check "API reachable" — reusarla, no añadir round-trip).
- `WARN` a partir de 30 s, `FAIL` a partir de 5 min con remedio ("sincroniza NTP").
- Complemento (fuera de este plan, apuntado): dar ~60 s de gracia en
  `verifyStreamToken`.

### A1.5 Permisos de la biblioteca, no sólo del download dir

Hoy se comprueba `download_dir` (existe / escribible / espacio). Los dirs de
biblioteca (`movies_dir`, `tv_shows_dir`) **no se tocan**, y son donde vive el fallo
de uid-mapping NFS/SMB que `makeReadable` ya sabe detectar.

- Por cada dir configurado: existe, escribible, y **un fichero de prueba se puede
  reabrir tras `chmod`** (la comprobación exacta que hace `makeReadable`).
- Espacio libre por dir — hoy sólo se mira el del download dir, que puede estar en
  otro filesystem.

### A1.6 Conectividad de los métodos activos

`par2` se comprueba; la conexión no. Por cada método en `MethodOrder()`:

- **usenet** → login NNTP contra el servidor configurado, 1 conexión, timeout 10 s.
  Reporta servidor + slots. No descarga nada.
- **debrid** → el agente no guarda credenciales debrid (las resuelve el web). El check
  útil es: "¿el web reporta debrid configurado para esta cuenta?" vía el endpoint de
  registro/perfil que ya se llama en el check "Agent registration".
- **torrent** → puerto `listen_port` bindable + una consulta DHT bootstrap.

### A1.7 `doctor --json`

Sin JSON no hay panel de salud en la web, ni `HEALTHCHECK` de Docker (**B4**), ni
`support-bundle` (**A3**) que valga. Esto **bloquea a A3 y B4** — va primero.

Requiere refactor: hoy `runDoctor` imprime dentro del closure `check()`. Extraer a:

```
internal/doctor/            (paquete nuevo)
  check.go      — tipo Check{Name, Group, Status, Message, Remedy}, Runner
  registry.go   — lista de checks (pura, sin I/O de presentación)
  render_text.go— render de consola (lo de hoy)
  render_json.go— render JSON
```

`internal/cmd/doctor.go` queda como cableado Cobra. Los checks concretos siguen en
`internal/cmd/` si dependen de helpers de ahí, o migran al paquete nuevo — decidir
fichero a fichero para no arrastrar dependencias circulares.

**Esfuerzo A1**: medio (el refactor JSON es el grueso). **Valor**: alto.
**Tests**: tabla por check con fakes (ffmpeg ausente/presente, puerto ocupado, skew).

---

## A2. `unarr selftest` — E2E de la cadena completa

**Problema.** `doctor` valida estado. Nadie valida que resolver → descargar → probar →
transcodificar → servir → reproducir funcione de punta a punta. Es la diferencia entre
"todo verde pero no me va" y un ticket accionable.

**Superficie:**

```bash
unarr selftest                    # cadena completa (~60 s), limpia tras de sí
unarr selftest --stage hls        # sólo una etapa
unarr selftest --keep             # no borrar los artefactos (depuración)
unarr selftest --json
```

**Etapas** (cada una con OK/FAIL + ms + el detalle del fallo):

| # | Etapa | Qué prueba | Fallo típico que caza |
|---|---|---|---|
| 1 | `api` | search/resolve contra el API | mirror caído, clave inválida |
| 2 | `download` | descarga ~20 MB de contenido público conocido | tracker/DHT bloqueado, dir no escribible |
| 3 | `probe` | ffprobe del fichero | ffmpeg roto / codec sin soporte |
| 4 | `directplay` | decisión direct-play sobre un perfil de dispositivo sintético | lógica de negociación rota |
| 5 | `transcode` | 5 s de HLS real (con hwaccel si hay) | **el `Invalid Level` anamórfico, nvenc mal configurado, zscale ausente** |
| 6 | `serve` | arranca el listener, mintea token, `GET /stream` + `GET /hls/…`, lee bytes | token roto, puerto, TLS |
| 7 | `cleanup` | borra todo lo creado | — |

**Decisiones de diseño:**

- **Fixture**: contenido de dominio público con infohash fijado y tamaño pequeño.
  Debe poderse sustituir por `--fixture <magnet|path>` para entornos sin salida a
  internet, y la etapa 2 se salta con `--stage` cuando no hay red.
- **Aislado**: todo en un subdir temporal propio, nunca en el download dir del
  usuario. `--keep` lo conserva.
- **No requiere daemon parado**: usa un puerto efímero, no el `stream_port` real.
- **Salida del fallo**: el comando ffmpeg **exacto** + últimas líneas de su stderr.
  Eso es el 80% del valor de la etapa 5.

**Ficheros**: paquete nuevo `internal/selftest/` (`runner.go`, `stages_net.go`,
`stages_media.go`, `stages_serve.go`, `report.go`), + `internal/cmd/selftest.go`
(cableado). Cada uno < 500 líneas.

**Doble uso**: sirve de smoke de release multiplataforma. El harness de
[test/windows/](../../test/windows/README.md) sólo cubre Windows; `selftest` corre en
macOS/Linux/NAS/Docker sin infraestructura.

**Esfuerzo**: alto (es el ítem más grande del plan). **Valor**: muy alto.

---

## A3. `unarr support-bundle`

**Depende de A1.7 (`doctor --json`).**

**Superficie:**

```bash
unarr support-bundle                  # genera ./unarr-support-<ts>.tar.gz
unarr support-bundle --out <path>
unarr support-bundle --log-lines 2000 # default 500
unarr support-bundle --print          # lista el contenido sin escribir el fichero
```

**Contenido:**

- `doctor.json` (A1.7)
- `config.redacted.toml`
- `unarr.log` (últimas N líneas) + `unarr.err.log`
- `version.txt` — versión, OS, arch, commit
- `ffmpeg.txt` — `ffmpeg -version`, encoders relevantes, probe hwaccel, resultado bench
- `system.txt` — CPU, RAM, `df` de los dirs configurados, PUID/PGID en Docker
- `tasks.json` — tareas activas (títulos truncados) + `active-tasks.json`
- `network.txt` — puertos en escucha del agente, estado del funnel, estado VPN

### Seguridad — redacción por allowlist, no por denylist

**Requisito duro.** El bundle NO puede filtrar: `auth.api_key`, `agent.agent_hash`,
`stream_secret`, la clave privada WireGuard, credenciales NNTP, `webdav_password`,
ni tokens `?t=` incrustados en URLs de log.

- La redacción se implementa **serializando desde una estructura allowlist** (campos
  explícitamente marcados como publicables), nunca filtrando una copia del `Config`.
  Un campo nuevo en `config.go` debe quedar fuera del bundle por defecto.
- Test obligatorio: rellenar `Config` con marcadores únicos en **todos** los campos
  secretos y assertar que ninguno aparece en la salida. Un campo nuevo sin marcar
  rompe el test.
- Sobre los logs se pasa un scrubber de patrones (`api_key=`, `?t=`, `Bearer `,
  claves base64 de 44 chars).
- El bundle **se genera en local y nunca se sube solo**. Sin telemetría, sin upload
  automático, sin "¿lo mando?". El usuario decide adjuntarlo.
- Fichero con permisos `0600`.

**Ficheros**: `internal/support/` (`bundle.go`, `redact.go`, `collect_system.go`,
`collect_logs.go`) + `internal/cmd/support_bundle.go`.

**Cablear**: enlace en
[.github/ISSUE_TEMPLATE/bug_report.md](../../.github/ISSUE_TEMPLATE/bug_report.md) y
en el mensaje de error de `doctor` cuando hay FAILs.

**Esfuerzo**: medio. **Valor**: alto (corta el coste de soporte de raíz).

---

## A4. `unarr why` — trazador de decisiones

**Problema.** El agente descarta opciones constantemente y no lo cuenta. "¿Por qué
transcodifica si el fichero es h264?" / "¿Por qué fue a torrent si tengo debrid?" hoy
sólo se responde leyendo el log con ojo experto, si es que quedó rastro.

**Superficie:**

```bash
unarr why <task-id>          # decisiones de una descarga/sesión
unarr why --last             # la última tarea
unarr why --stream <id>      # decisiones de una sesión de streaming
unarr why --json
```

**Salida** — cadena de decisión con lo descartado y su motivo:

```
Task 3f2a… "Dune Part Two (2024) 2160p"

  Método         debrid                    elegido
    torrent      descartado — preferred_methods = ["debrid","usenet"]
    usenet       descartado — no probado (debrid resolvió primero)

  Reproducción   HLS transcode             elegido
    direct-play  descartado — audio EAC3 no soportado por el perfil (Chrome/Linux)
    remux fMP4   descartado — mismo motivo (el audio necesita re-encode)

  Transcode      h264 1080p, nvenc
    altura       1080 — techo del host (bench: nvenc → 2160; perfil pide 1080)
    nivel H.264  4.2 — 2586×1080 = 11016 MBs excede el MaxFS de 4.1
    tonemap      activo — fuente HDR10 + zscale disponible
```

**Diseño — lo crítico:** esto NO es una feature nueva, es exponer lo que ya se
calcula. Requiere un mecanismo de traza ligero:

- Tipo `decision.Trace` con `Record(area, chosen, []rejected{option, reason})`.
- Se inyecta en los puntos de decisión que ya existen: `MethodOrder`/`resolve.go`,
  `decideStreamPlan`, `DecideAction`, `H264LevelForFrame`, `tonemap.go`,
  `dynamicReadahead`.
- **Coste cero cuando no se traza**: el `Trace` es una interfaz con implementación
  nil-op por defecto; sólo el daemon con `[daemon] decision_trace = true` (default
  **on**, es barato) guarda las últimas N trazas en memoria + un anillo en disco
  (`decisions.jsonl`, rotado por tamaño).
- Retención por defecto: últimas 50 tareas / 7 días.

**Riesgo**: tocar puntos de decisión repartidos por `engine/` invita a regresiones.
Mitigación: la traza es **sólo escritura de metadatos**, nunca influye en el retorno;
un test por punto de decisión que assertea que el resultado no cambia con y sin traza.

**Ficheros**: `internal/decision/` (`trace.go`, `store.go`, `render.go`) +
`internal/cmd/why.go` + inyecciones puntuales en `engine/`.

**Esfuerzo**: medio-alto (por el número de puntos a instrumentar).
**Valor**: alto y **diferenciador** — Plex/Jellyfin son cajas negras aquí.

---

## A5. Logs — rotación, niveles, comando de primer nivel

**Problema.** [daemon_control.go:323](../../internal/cmd/daemon_control.go#L323) abre
`unarr.log` en `O_CREATE|O_WRONLY|O_APPEND` y ya. Sin rotación, sin techo. En un NAS
24/7 crece indefinidamente; el único freno es `unarr clean`, que es manual. Bajo
systemd journald amortigua, pero el arranque detached, Windows y Docker escriben a ese
fichero directamente.

**Cambios:**

1. **Rotación por tamaño** — writer propio (no hace falta dependencia: rotate-on-write
   con `Stat` cacheado son ~80 líneas). Config:
   ```toml
   [daemon]
     log_max_size_mb = 20   # default; 0 = sin rotación
     log_max_files   = 3    # unarr.log, .1, .2
   ```
   Aplicar en **todos** los paths de apertura del log (detached, servicio, Windows
   task), no sólo uno — es el error fácil aquí.

2. **Niveles** — `[daemon] log_level = "info"` (`debug|info|warn|error`) + flag
   `--log-level` que lo pisa. Hoy no hay forma de subir verbosidad sin recompilar.

3. **`--log-format=json`** — para quien lo mete en Loki/journald estructurado.

4. **`unarr logs` de primer nivel** (hoy sólo `unarr daemon logs`), con
   `--follow --since --level --grep --lines`. Mantener `unarr daemon logs` como alias
   para no romper a nadie.

**Ficheros**: `internal/logging/` (`writer.go` rotación, `level.go`, `format.go`) +
`internal/cmd/logs.go`. `daemon_control.go` pasa a usar el writer.

**Esfuerzo**: bajo-medio. **Valor**: alto (es un bug latente en NAS, no una mejora).

---

# Parte B — Operabilidad

## B1. Validación de config — claves desconocidas silenciadas

**Problema confirmado.** [config.go:614](../../internal/config/config.go#L614) hace
`meta, err := toml.Decode(...)` y usa `meta` **sólo** para `IsDefined` en
`applyDefaults`. `meta.Undecoded()` no se llama nunca. Consecuencia: cualquier clave
mal escrita o mal ubicada se ignora en silencio.

```toml
[downloads]
  min_free_disk_gb = 10     # es _mb → ignorado, sin aviso
  seed_ratio = 2.0          # correcto aquí
[library]
  seed_ratio = 2.0          # sección equivocada → ignorado, sin aviso
```

Es el bug de soporte clásico: el usuario jura haberlo configurado y el agente jura que
no. Coste del fix: mínimo.

**Cambios:**

1. `config.Load` devuelve las claves no decodificadas (nuevo campo en el resultado o
   un `Load` que retorne `(Config, []UnknownKey, error)` — preferible lo segundo, y
   adaptar los ~N call-sites).
2. **No fallar el arranque** por una clave desconocida (rompería configs existentes
   tras un rename futuro). El daemon emite `WARN` al log en el arranque, una línea por
   clave, con sugerencia por distancia de Levenshtein contra las claves válidas:
   `unknown key "downloads.min_free_disk_gb" — ¿querías "downloads.min_free_disk_mb"?`
3. Check nuevo en `doctor`: **WARN** con la lista.
4. `unarr config check` — valida y sale con código ≠ 0 si hay claves desconocidas o
   valores fuera de rango (útil en CI/Ansible).

**Ficheros**: `internal/config/validate.go` (nuevo) + wiring en doctor y daemon.
**Tests**: tabla de configs con typos → claves esperadas + sugerencia.

**Esfuerzo**: bajo. **Valor**: alto. **Va primero de toda la Parte B.**

---

## B2. `unarr bench` — exponer el benchmark que ya existe

**Problema.** `BenchmarkMaxTranscodeHeight` está implementado y comentado a fondo en
[encode_benchmark.go](../../internal/engine/encode_benchmark.go), pero **no hay forma
de que el usuario lo ejecute ni vea el resultado**. Ajustar expectativas antes de la
primera queja es más barato que explicarlo después.

**Superficie:**

```bash
unarr bench              # encode + disco + red, tabla resumen
unarr bench --encode     # sólo transcode
unarr bench --disk       # throughput secuencial en el download dir
unarr bench --net        # reusa el handler /speedtest ya existente
unarr bench --json
```

**Salida:**

```
  Encode      software x264   → 720p sostenible (1.9× realtime a 1080p, umbral 2.0×)
              hwaccel  nvenc  → 2160p
              veredicto: 4K vía NVENC OK; sin hwaccel, 4K va a player externo

  Disco       download dir  412 MB/s escritura secuencial
  Red         API 34 ms · descarga 92 Mbps
```

- **Cachear** el resultado del encode bench en el data dir con la versión de ffmpeg y
  la CPU como clave; `doctor` (A1.1) lo lee sin re-ejecutar. Invalidar si cambia
  ffmpeg o el hardware.
- `--net` reusa `speedtestHandler`
  ([stream_server.go:420](../../internal/engine/stream_server.go#L420)) — no
  reimplementar.

**Ficheros**: `internal/cmd/bench.go` + `internal/engine/bench_disk.go`.
**Esfuerzo**: bajo. **Valor**: medio-alto (y alimenta A1 y A3).

---

## B3. Notificaciones fuera del escritorio

**Problema.** [internal/notify/](../../internal/notify/) sólo hace `notify-send` /
toast de Windows / AppleScript. En un NAS headless — que es el despliegue objetivo —
**no notifica a nadie**. Y `NotificationsConfig` es literalmente
`struct { Enabled bool }`.

Los call-sites ya están centralizados y son pocos: `manager.go` (complete, failed,
storage unavailable) y `daemon.go` (blocked, disconnected). Toda la fontanería está.

**Cambios:**

```toml
[notifications]
  enabled = true
  desktop = true              # lo de hoy
  # Eventos: download_complete, download_failed, disk_full, agent_blocked,
  #          agent_disconnected, upgrade_applied
  events = ["download_failed", "disk_full", "agent_blocked"]

  [[notifications.targets]]
    type = "webhook"          # POST JSON genérico
    url  = "https://…"
    # headers = { Authorization = "Bearer …" }

  [[notifications.targets]]
    type = "ntfy"
    url  = "https://ntfy.sh/mi-topic"

  [[notifications.targets]]
    type = "discord"          # webhook de Discord
    url  = "https://discord.com/api/webhooks/…"

  [[notifications.targets]]
    type = "telegram"
    token = "…"
    chat_id = "…"

  [[notifications.targets]]
    type = "apprise"          # delega en un Apprise self-hosted → cubre el resto
    url = "http://apprise:8000/notify"
```

**Diseño:**

- `notify.Send/SendUrgent` mantienen su firma → **cero cambios en los call-sites**.
  Detrás, un `Dispatcher` con N sinks; el sink desktop es uno más.
- Envío **asíncrono con timeout corto (5 s) y sin reintentos infinitos**: una
  notificación no puede bloquear ni retrasar una descarga. Fallo → línea en el log,
  nada más.
- Filtro por evento: sin `events` configurado, se mandan todos.
- `unarr notify test [--target N]` para probar la config sin esperar a un evento real.

**Seguridad**: las URLs de webhook llevan secretos en el path → quedan **fuera** del
support-bundle (allowlist de A3 lo cubre por defecto) y se redactan en el log.

**Ficheros**: `internal/notify/dispatch.go`, `sink_desktop.go` (mover lo actual),
`sink_webhook.go`, `sink_ntfy.go`, `sink_discord.go`, `sink_telegram.go`,
`sink_apprise.go`, `config.go`. Cada sink ~60 líneas, ninguno cerca del límite.

**Esfuerzo**: medio. **Valor**: alto — es expectativa estándar del público homelab.

---

## B4. `HEALTHCHECK` en el Dockerfile

**Problema confirmado.** Cero `HEALTHCHECK` en el [Dockerfile](../../Dockerfile).
Docker, Portainer, Swarm, k8s y `docker compose --wait` dependen de él; sin healthcheck
un contenedor con el daemon muerto figura como "running".

**Depende de A1.7.**

```dockerfile
HEALTHCHECK --interval=60s --timeout=15s --start-period=90s --retries=3 \
  CMD unarr doctor --json --quick || exit 1
```

- `--quick` (nuevo flag): sólo los checks locales y baratos — daemon vivo, config
  válida, download dir escribible, espacio. **Sin llamadas al API**: un healthcheck que
  depende de internet marca el contenedor unhealthy cada vez que hay un blip de red y
  provoca reinicios en cascada. Esto es un requisito, no un detalle.
- `start-period` generoso: el primer arranque hace registro y scan.
- Código de salida: 0 = healthy, 1 = unhealthy. Los WARN no marcan unhealthy.

Documentar en [DOCKERHUB.md](../../DOCKERHUB.md) y en el
[docker-compose.yml](../../docker-compose.yml) de ejemplo.

**Esfuerzo**: muy bajo (una vez existe A1.7). **Valor**: medio-alto.

---

## B5. `unarr library organize --dry-run`

**Problema.** [organize.go](../../internal/engine/organize.go) decide rutas y nombres
con lógica no trivial (limpieza de título, año, detección de temporada,
`versionDistinctPath`, tags HDR, reemplazo de ficheros con backup). Sólo se ejecuta
**después** de una descarga, sobre un fichero, sin previsualización. Activar
`[organize] enabled = true` sobre una biblioteca existente da miedo, y con razón.

**Superficie** — subcomando nuevo junto a `library clean` / `library stats`:

```bash
unarr library organize                    # DRY-RUN por defecto (como library clean)
unarr library organize --apply
unarr library organize --path <dir>       # limitar el alcance
unarr library organize --json
```

**Salida:** tabla `origen → destino` + motivo, y una sección de **conflictos**
(destino ya existe, dos orígenes al mismo destino, título no parseable) que es
justamente lo que hay que ver antes de aplicar.

**Diseño:**

- Reusar las funciones **puras** ya existentes (`cleanTitle`, `detectSeason`,
  `resolveYear`, `sanitizePath`, `versionTag`, `versionDistinctPath`). Si alguna
  necesita un `*Task`/`*Result` que aquí no hay, extraer la parte pura — **sin
  duplicar** (regla del gate).
- Dry-run por defecto, igual que `library clean`. `--apply` mueve, y **nunca**
  sobrescribe: un destino existente es un conflicto reportado, no un reemplazo.
- Confinado a los dirs configurados (misma guarda que `clean`/`stats`).

**Ficheros**: `internal/library/organize_preview.go` +
`internal/cmd/library_organize.go`.

**Esfuerzo**: medio. **Valor**: medio-alto (desbloquea que la gente active organize).

---

## B6. `unarr monitor` — vigilancia de series

**Problema.** Está en el "Coming Soon" del README y es **la razón por la que la gente
tiene Sonarr**. Sin monitor, la promesa de `unarr migrate` ("replacing your entire *arr
stack") no es cierta: importas la biblioteca y a partir de ahí no llega nada nuevo.

**Superficie:**

```bash
unarr monitor add <serie> [--quality 1080p] [--from S03E01] [--lang es]
unarr monitor list
unarr monitor remove <id>
unarr monitor pause <id> / resume <id>
unarr monitor check          # forzar comprobación ya (sin esperar al tick)
```

**Diseño — la pregunta de arquitectura clave: ¿dónde vive el estado?**

Dos opciones, hay que decidir **antes de escribir código**:

| | Estado en el agente | Estado en el servidor (web) |
|---|---|---|
| Pros | funciona offline; sin cambios de API | 1 sola lista para N agentes; visible/editable en la web; el servidor ya sabe qué hay nuevo en el catálogo |
| Contras | invisible en la web; N agentes = N listas divergentes | requiere trabajo en torrentclaw-web; el agente no puede monitorizar solo |

**Recomendación: estado en el servidor**, agente como ejecutor. Razones: (a) el
servidor ya tiene el catálogo y sabe cuándo aparece un episodio — el agente tendría que
hacer polling de búsqueda, que es caro y peor; (b) la web es donde el usuario ya
gestiona su biblioteca; (c) evita que dos agentes de la misma cuenta descarguen lo
mismo. Implica un tramo de trabajo en `torrentclaw-web` que **este plan no cubre** —
hay que dimensionarlo aparte antes de comprometerse.

Mientras tanto, el CLI puede exponer los comandos como fachada sobre la API del
servidor (thin client), que es poco código en este repo.

**Riesgos:** desambiguación de series (mismo nombre, distinto año/país), calidad
"upgrade" vs "nuevo", y no re-descargar lo que ya está. La reconciliación con la
biblioteca local ya existe (`internal/library/reconcile*.go`) — reusarla.

**Esfuerzo**: alto (y cruza de repo). **Valor**: muy alto estratégicamente.
**Nota**: candidato a plan propio. Aquí queda especificado el alcance y la decisión
pendiente, no la implementación.

---

## B7. Endpoint `/metrics` (Prometheus)

**Problema.** El público NAS/homelab vive en Grafana. Hoy no hay nada que raspar, y de
paso es la vista longitudinal que el troubleshooting no tiene: "esto va lento **desde
el martes**" no se contesta con `doctor`.

**Diseño:**

- Handler nuevo en el mux que ya existe
  ([stream_server.go:418](../../internal/engine/stream_server.go#L418)).
- **Sin dependencia de `prometheus/client_golang`** (no está en `go.mod` y arrastra
  bastante): el formato de exposición es texto plano; un expositor propio son ~100
  líneas y evita meter una dep grande en un binario que ya pesa 36 MB.
- **Opt-in y sólo LAN por defecto**, con la misma guarda que WebDAV
  (`webdavGuard` → LAN/Tailscale/loopback; todo lo demás 404):
  ```toml
  [downloads]
    metrics_enabled = false     # opt-in
    metrics_allow_wan = false
  ```
  Las métricas filtran títulos y hábitos de consumo → **nunca** expuestas a WAN por
  defecto, y los labels no llevan títulos (sólo IDs/hashes truncados).

**Métricas iniciales:**

```
unarr_build_info{version,os,arch}
unarr_tasks_active{method,status}
unarr_task_bytes_total{method}
unarr_task_duration_seconds (histograma)
unarr_download_speed_bytes            # instantáneo, agregado
unarr_stream_sessions_active
unarr_transcode_sessions_active{hwaccel}
unarr_hls_cache_hits_total / _misses_total / _bytes
unarr_hls_segment_latency_seconds     # histograma
unarr_disk_free_bytes{dir}
unarr_errors_total{area}
unarr_vpn_up / unarr_funnel_up
```

**Ficheros**: `internal/metrics/` (`registry.go`, `expose.go`, `collect.go`) + handler.
**Esfuerzo**: medio-bajo. **Valor**: medio-alto.

---

## B9. Programación de ancho de banda

**Problema.** `max_download_speed` / `max_upload_speed` existen pero se parsean **una
sola vez al arrancar el daemon**
([daemon.go:351](../../internal/cmd/daemon.go#L351)) y se pasan al cliente torrent. No
hay horario. qBittorrent lo tiene desde siempre y es petición recurrente: "a plena
velocidad de noche, frenado de día".

**Superficie:**

```toml
[downloads.schedule]
  enabled = true
  # Ventanas con límites propios. Fuera de toda ventana → los límites base.
  [[downloads.schedule.windows]]
    days  = ["mon","tue","wed","thu","fri"]
    from  = "02:00"
    to    = "08:00"
    max_download_speed = "0"      # ilimitado de madrugada
    max_upload_speed   = "5MB"

  [[downloads.schedule.windows]]
    days = ["sat","sun"]
    from = "00:00"
    to   = "24:00"
    max_download_speed = "0"
```

```bash
unarr schedule status     # ventana activa ahora y límites vigentes
unarr schedule test       # simula 24 h y tabula los límites resultantes
```

**Lo difícil no es el horario, es aplicarlo en caliente.** Hoy el límite es un valor
de arranque. Hace falta:

- Un `rate.Limiter` compartido cuyo `SetLimit` se pueda cambiar en vivo, aplicado a
  los tres métodos — **torrent, debrid (HTTP Range) y usenet (NNTP)**. Hoy sólo el
  cliente torrent recibe límite; si el horario sólo frena torrent, el usuario verá que
  "no funciona" cuando descargue por debrid.
- Ticker de 1 min en el daemon que evalúa la ventana activa y ajusta.
- Zona horaria local, y manejo explícito del cambio de hora (DST): una ventana que
  cruza medianoche o una hora repetida no debe dejar el límite pegado.

**Extensión natural (no en el MVP):** pausar/reanudar descargas por ventana, no sólo
limitar.

**Ficheros**: `internal/config/schedule.go` (parseo + evaluación pura, muy testeable),
`internal/engine/ratelimit.go` (limiter compartido), wiring en `daemon.go`,
`internal/cmd/schedule.go`.

**Tests**: tabla de (config, instante) → límites esperados, incluyendo cruce de
medianoche, solapes de ventanas y DST.

**Esfuerzo**: medio (el limiter compartido entre métodos es el grueso).
**Valor**: medio-alto.

---

## B10. `unarr top` — dashboard TUI

**Problema.** `unarr status` es una foto. Para ver qué está pasando *ahora* hay que
repetirlo o leer el log. Y es la herramienta de diagnóstico que falta entre "doctor
dice que todo bien" y "vamos a leer 4000 líneas de log".

**Superficie:**

```bash
unarr top                 # TUI a pantalla completa
unarr top --once          # una foto y salir (scripts)
unarr top --refresh 2s
```

**Paneles:**

```
┌ Agente ──────────────────────────────────────────────────────┐
│ v1.8.2 · conectado (SSE) · VPN ↑ nl-3 · funnel ↑ · disco 412G │
├ Descargas ───────────────────────────────────────────────────┤
│ Dune Part Two          debrid    ███████░░░  71%  48 MB/s  2m │
│ Severance S02E04       torrent   ██░░░░░░░░  19%   6 MB/s 34m │
│   peers 23/61 · seeds 12 · readahead 73 MiB                   │
├ Streams ─────────────────────────────────────────────────────┤
│ Chrome/Linux  HLS nvenc 1080p  seg 4.2s  cache HIT 87%        │
├ Últimos eventos ─────────────────────────────────────────────┤
│ 14:02 organize → /media/Movies/Dune Part Two (2024)/          │
└───────────────────────────────────────────────────────────────┘
```

**Diseño:**

- Consume el **mismo estado que `status`** vía el socket/endpoint local del daemon
  — no reimplementar recolección. Si hace falta, un `/state` local (loopback-only).
- **Dependencia nueva**: `bubbletea` + `lipgloss`. `charmbracelet/huh` ya está en
  `go.mod` y arrastra parte del árbol, así que el coste incremental es moderado — pero
  **hay que medir el impacto en el tamaño del binario** antes de comprometerse (36 MB
  ya es mucho para un NAS). Alternativa si el coste no compensa: redibujado manual con
  códigos ANSI, suficiente para este layout.
- Degradar bien: sin TTY → equivale a `--once`.

**Esfuerzo**: medio. **Valor**: medio (deleite alto, utilidad real de diagnóstico).
**Prioridad**: el último de la Parte B. Es el que menos duele si se corta.

---

## B12. Race `manager` ↔ `reporter` (corregir de verdad)

**Problema — localizado con precisión.** `Task.ToStatusUpdate()`
([task.go:245](../../internal/engine/task.go#L245)) lee **bajo `RLock`**,
correctamente. El fallo está en el **lado escritor**: `manager.go` escribe campos del
`Task` **sin tomar el lock**:

| Línea | Escritura sin lock |
|---|---|
| [manager.go:314](../../internal/engine/manager.go#L314) | `task.ErrorMessage = "cancelled by user"` |
| [manager.go:364](../../internal/engine/manager.go#L364) | `task.ErrorMessage = "cancelled by user"` |
| [manager.go:638](../../internal/engine/manager.go#L638) | `task.FilePath = finalPath` |
| [manager.go:648](../../internal/engine/manager.go#L648) | `task.FilePath = task.ReplacePath` |
| [manager.go:669](../../internal/engine/manager.go#L669) | `task.ErrorMessage = msg` |
| [manager.go:697](../../internal/engine/manager.go#L697) | `task.ErrorMessage = "VPN tunnel down…"` |

`ProgressReporter` llama a `ToStatusUpdate()` desde otra goroutine
([progress.go:173](../../internal/engine/progress.go#L173), `:191`, `:258`) → data race
real sobre `string`. En Go una escritura de `string` no es atómica (puntero + longitud):
el reporter puede leer un `string` desgarrado y mandar basura, o petar.

Está anotado como "flake de CI" en el roadmap y en la memoria de proyecto. **Un flake
tolerado acaba tapando un fallo real** — y este no es un flake de test, es un bug de
producción que el test sólo destapa.

**Fix:**

1. Añadir setters con lock en `task.go`, en la línea de los que ya existen
   (`SetStreamURL`, `SetResolvedMethod`, `UpdateProgress`):
   `SetError(msg string)`, `SetFilePath(p string)`, `SetFileName(n string)`.
2. Sustituir las 6 escrituras directas.
3. **Auditar el resto de campos** leídos por `ToStatusUpdate` (`TotalBytes`,
   `DownloadedBytes`, `SpeedBps`, `ETA`, `FileName`) buscando otros escritores sin
   lock fuera de `UpdateProgress`.
4. Hacer los campos no exportados sería el fix estructural definitivo (imposible
   escribir sin setter), pero rompe call-sites en cascada → **fase 2**, medida aparte.
5. De paso, lo apuntado en el roadmap: `task.ID[:8]` paniquea con IDs < 8 chars en
   `progress.go`/`torrent.go` → extraer `ShortID()`.

**Verificación:** `go test -race ./internal/engine/...` en verde de forma repetida
(≥20 ejecuciones), y **añadir `-race` al job de Coverage** para que no vuelva a
degradar en silencio.

**Esfuerzo**: bajo. **Valor**: alto (corrección). **Va en la fase 0.**

---

# Orden de ejecución

Las dependencias mandan: `doctor --json` (A1.7) bloquea a `support-bundle` (A3) y al
`HEALTHCHECK` (B4).

### Fase 0 — Correcciones (días)
1. **B12** race manager↔reporter + `-race` en CI
2. **B1** validación de config (`Undecoded`) + `unarr config check`
3. **A5** rotación de logs + niveles + `unarr logs`

> Los tres son bugs latentes, no features. Ninguno depende de nada.

### Fase 1 — Diagnóstico (1–2 semanas)
4. **A1.7** refactor `internal/doctor/` + `--json` + `--quick`
5. **A1.1–A1.6** checks nuevos (ffmpeg el primero)
6. **B2** `unarr bench` (alimenta A1 y A3)
7. **B4** `HEALTHCHECK` (trivial una vez existe A1.7)
8. **A3** `support-bundle` + redacción allowlist + enlace en el issue template

> Al cerrar esta fase, el ciclo de soporte ya cambia: el usuario adjunta un bundle.

### Fase 2 — Explicabilidad y observabilidad
9. **A2** `unarr selftest`
10. **A4** `unarr why` (traza de decisiones)
11. **B7** `/metrics`

### Fase 3 — Operabilidad
12. **B3** notificaciones (webhook/ntfy/Discord/Telegram/Apprise)
13. **B5** `library organize --dry-run`
14. **B9** programación de ancho de banda
15. **B10** `unarr top`

### Aparte — requiere decisión previa
16. **B6** `unarr monitor` — decidir dónde vive el estado (recomendación: servidor) y
    dimensionar el tramo de `torrentclaw-web`. **Plan propio antes de escribir código.**

---

# Riesgos transversales

| Riesgo | Mitigación |
|---|---|
| El refactor de `doctor` (A1.7) rompe checks existentes | Migrar check a check con su test; el render de texto debe producir salida idéntica (golden test) antes de tocar nada más |
| `support-bundle` filtra un secreto | Allowlist + test de marcadores en todos los campos secretos; un campo nuevo sin marcar rompe el build |
| La traza de A4 altera decisiones de `engine/` | La traza sólo escribe metadatos; test por punto de decisión que assertea resultado idéntico con y sin traza |
| `/metrics` expuesto a internet filtra hábitos de consumo | Opt-in + guarda LAN reusando `webdavGuard`; sin títulos en los labels |
| El horario de B9 sólo frena torrent | El limiter compartido cubre torrent + debrid + usenet, o no se envía |
| `unarr top` engorda el binario | Medir el delta de `bubbletea` antes de adoptarlo; fallback a ANSI manual |
| Crecimiento de `internal/cmd/` (ya ~17k líneas) | Toda la lógica nueva en paquetes `internal/*`; `cmd/` sólo cablea Cobra. `make arch` en cada commit |

# Qué NO entra en este plan

- Shim qBittorrent, indexer Torznab, shim SABnzbd → **sesión aparte**
- Multi-viewer (sesión única actual)
- Relay propio para sustituir el SPOF de CloudFlare
- Descarga automática de ffmpeg desde `doctor --fix` (sólo se detecta y se guía)
