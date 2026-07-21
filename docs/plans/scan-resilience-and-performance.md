# Escaneo de biblioteca resiliente y performante

Estado: **plan aprobado, sin implementar**. Fecha: 2026-07-21.

Sucesor de `da8fce9` (`fix(scan): never report an inconclusive probe as a damaged
file`), que detuvo la corrupción pero **no** hizo el scan reanudable.

## Por qué

Un scan interrumpido hoy falla limpiamente, pero se pierde entero. Causa raíz en
tres capas:

1. El ctx del scan **es** el ctx raíz del daemon (`daemon.go:304`), cancelado en
   SIGTERM y en revocación de credenciales. No hay ctx propio de scan.
2. El unit systemd (`daemon_install.go`) no fija `TimeoutStopSec` → 90 s por
   defecto. Un scan de 1665 ficheros tarda mucho más. Cualquier `systemctl
   restart`, `unarr update` o actualización lo corta.
3. `SaveCache` solo corre **después** de un sync completo. Un scan cortado al 60 %
   no persiste nada; el siguiente ciclo empieza de cero. El modo incremental solo
   sirve si el ciclo anterior terminó.

Efecto medido en prod (incidente 2026-07-21): las bibliotecas grandes son las más
castigadas — el agente de 1665 ficheros perdió el 62 % frente al 19,5 % de las de
<100. Una biblioteca suficientemente grande respecto a la frecuencia de reinicios
puede **no completar nunca** un ciclo.

## Decisión de arquitectura

**Journal JSONL append-only + snapshot compacto al cierre de ciclo. No sqlite.**

- sqlite añade cgo (o ~8-10 MB con modernc) a un CLI multiplataforma, y su locking
  sobre home-dirs NFS es fuente conocida de corrupción — justo el entorno de los
  peores afectados.
- Un JSONL es crash-safe por construcción (la última línea parcial se detecta y
  descarta), inspeccionable por soporte con `tail`, y cuesta O(bytes añadidos), no
  O(biblioteca).
- El snapshot `library.json` ya tiene rename atómico; basta con dejar de
  reescribirlo por checkpoint. Con `MarshalIndent` por checkpoint serían ~100 GB
  escritos por ciclo de 20k items. Descartado.

**La reanudación no cambia el contrato de sync.** Se reanuda el trabajo local
(probes); el sync solo ocurre al cerrar un ciclo completo. La lista de items del
sync nace **siempre** del discovery del ciclo actual, reutilizando veredictos
journaled cuando `size+mtime` coinciden. Así un ciclo reanudado sigue siendo una
declaración completa y veraz de "lo que existe", y `fullCycle=true` es legítimo.

## Fase 1 — Ciclo de vida y apagado ordenado

Riesgo mínimo, entregable sola. Corta la pérdida de scans truncados a los 90 s.

### 1.1 Supervisor con ctx propio — `internal/cmd/daemon.go`

Sustituir `go runAutoScan(ctx, ...)` (línea 1211) por un supervisor cuyo ciclo
corre con `context.WithCancel(context.Background())` — desacoplado del ctx raíz.
El ticker sí escucha `daemonCtx.Done()`.

```go
type scanSupervisor struct {
    mu          sync.Mutex
    cycleCancel context.CancelFunc
    done        chan struct{}
}
func (s *scanSupervisor) Run(daemonCtx context.Context)
func (s *scanSupervisor) Stop(grace time.Duration) // cancela y espera SOLO el flush del journal
```

Política: **cancelar en caliente, no drenar**. Con journal, cancelar pierde solo
los ≤`Workers` probes en vuelo (se re-proban al reanudar); drenar añadiría 30-60 s
de apagado para salvar 8 ficheros. `Stop` espera únicamente flush+fsync+close
(<1 s). Llamarlo en el branch SIGTERM (~línea 1248), junto a `manager.Shutdown`.

La revocación de credenciales (`OnRevoked`, línea 1220) sigue cancelando todo:
sin identidad no hay sync, y el journal queda para el próximo arranque.

### 1.2 Presupuesto por fichero — `internal/library/scanner.go`

`ExtractMediaInfo` **no tiene timeout propio** (`ffprobe.go:81` usa el ctx del
scan, hoy sin deadline): un mount NFS colgado congela un worker para siempre.
`AssessTruncation` sí acota sus sub-probes (30 s), pero el header probe no.

`ScanOptions` gana `PerFileTimeout time.Duration` (default 2 min). En
`scanSingleFile`, envolver probe+truncación con `context.WithTimeout`. El timeout
produce `ErrProbeInconclusive` → `ScanAborted` (clasificación ya existente y
correcta). Añadir `cmd.WaitDelay = 5*time.Second` a los `exec.CommandContext` de
`ffprobe.go` e `integrity.go` para no filtrar goroutines con procesos en D-state
(hoy `WaitDelay` no se usa en ningún sitio del repo).

### 1.3 Unidades de servicio — `internal/cmd/daemon_install.go`

- systemd: `TimeoutStopSec=60` (cubre drain de descargas 30 s + flush de scan 5 s
  con margen) y `KillMode=mixed` (SIGTERM solo al principal, para que los ffprobe
  hijos no reciban una señal que malinterpreten; SIGKILL al cgroup al expirar).
  `Restart=always` + `RestartSec=10` **se quedan**: con reanudación dejan de ser
  un problema y son deseables.
- launchd: `<key>ExitTimeOut</key><integer>60</integer>`.
- Windows: la Scheduled Task muere con `taskkill /f`, sin señal. No hay apagado
  ordenado posible; la defensa real es el journal (un kill de proceso no pierde lo
  ya `Flush()`-eado al page cache; solo un corte de luz lo haría, y para eso está
  el fsync periódico).
- **Flota existente**: los unit files viejos no se regeneran solos. Marcador
  `# unarr-unit-rev: 2` en el template, check en `doctor.go` ("unit desactualizado
  → re-ejecuta `unarr daemon install`") y regeneración automática en
  `self_update.go` tras un upgrade (ya reinicia el servicio).

## Fase 2 — Persistencia incremental y reanudación

El corazón. Contiene los cinco puntos de riesgo de borrado.

### 2.1 Journal — nuevo `internal/library/journal.go`

Fichero: `config.DataDir()/scan-journal.jsonl`.

```go
type journalRecord struct {
    T            string       `json:"t"` // "begin" | "item" | "end"
    Cycle        string       `json:"cycle,omitempty"`
    Roots        []string     `json:"roots,omitempty"`
    ProbeVersion int          `json:"probeVersion,omitempty"`
    Root         string       `json:"root,omitempty"`
    Item         *LibraryItem `json:"item,omitempty"`
    At           string       `json:"at,omitempty"`
}

func OpenJournal(path string) (*ScanJournal, error)
func (j *ScanJournal) Begin(cycleID string, roots []string, probeVersion int) error
func (j *ScanJournal) Append(root string, item LibraryItem) error
func (j *ScanJournal) Close() error
func ReplayJournal(path string, roots []string, probeVersion int) (overlay map[string]LibraryItem, cycleID string, err error)
func ResetJournal(path string) error
```

Cadencia: `Flush` por item (protege contra kill de proceso, ~µs); `fsync` cada 20
items o 5 s (protege contra corte de luz; ~5-10 ms en HDD de NAS). Replay
idempotente: varias entradas para el mismo path → gana la última (permite
re-emitir veredictos, ver 3.2).

### 2.2 Cache v3 + migración — `types.go`, `cache.go`

```go
type LibraryItem struct {
    // ... campos actuales ...
    Root         string `json:"root,omitempty"`
    ProbeVersion int    `json:"probeVersion,omitempty"`
}
type LibraryCache struct {
    Version      int      `json:"version"`      // formato = 3
    ProbeVersion int      `json:"probeVersion"` // versión de LÓGICA
    CycleID      string   `json:"cycleId"`
    ScannedAt    string   `json:"scannedAt"`
    Path         string   `json:"path"`
    Roots        []string `json:"roots,omitempty"`
    Items        []LibraryItem `json:"items"`
}
const cacheFormatVersion = 3
const probeVersion = 3
```

`LoadCacheFrom` deja de devolver `nil` en mismatch: `case 2: migrateCacheV2()`
conserva items (paths, fingerprints, mtimes), estampa `Root=cache.Path` y
`ProbeVersion=2`. **Un bump de versión nunca vuelve a costar un full-scan de
flota** (re-probe gradual en Fase 4).

Atomicidad en `SaveCacheTo`: `json.Marshal` compacto (no `MarshalIndent`, mitad de
tamaño), escribir a `.tmp`, **`f.Sync()` antes del rename** (hoy falta) y fsync del
directorio en unix.

> **PELIGRO (borrado):** la migración v2→v3 debe preservar `FilePath` byte a byte
> — es la clave del upsert `(user_id, file_path)`. Un path alterado crea fila
> nueva y el siguiente `fullCycle` **borra la vieja con su `matchStrategy=manual`**.
> Test obligatorio de identidad de paths.

### 2.3 Scanner: hook de persistencia + plan de trabajo ordenado

```go
type ScanOptions struct {
    Workers        int
    FFprobePath    string
    FFmpegPath     string
    Incremental    bool
    PerFileTimeout time.Duration
    OnProgress     func(scanned, total int, current string)
    OnItem         func(item LibraryItem) // al COMPLETAR cada item → journal
    probeFile      probeFunc              // seam de test, no exportado
}
```

`Scan` llama `opts.OnItem(item)` bajo el mismo mutex donde hoy hace `append`.

**Reanudación sin maquinaria nueva**: el daemon construye `existing` como
snapshot ⊕ overlay del journal y pasa `Incremental: true`. El camino incremental
existente (skip si `size+mtime` coinciden y `Integrity == nil`) **es** el
mecanismo de resume: cada fichero journaled se re-stat-ea (verificación fresca de
existencia e identidad) y reutiliza su veredicto; los no probados no están en el
overlay → se proban. Los dañados se re-proban siempre (regla actual, correcta).

**Priorización** por tiers tras `discoverFiles`: (0) sin entrada = nuevos,
(1) size/mtime cambiado, (2) con veredicto damaged, (3) unchanged. Con journal +
este orden, ciclos repetidamente interrumpidos hacen progreso **monótono**.

### 2.4 Orquestador — nuevo `internal/library/cycle.go`

Extrae el closure `doScan` de 140 líneas de `runAutoScan`.

```go
type CycleSyncer interface {
    SyncLibrary(ctx context.Context, req agent.LibrarySyncRequest) (*agent.LibrarySyncResponse, error)
}
func RunScanCycle(ctx context.Context, sync CycleSyncer, opts CycleOptions) (CycleResult, error)
```

Secuencia:

1. Cargar snapshot; migrar si v2. `basePathChanged` (se mueve aquí desde
   `daemon.go`) → descarta overlay y fuerza full.
2. `ReplayJournal(roots, probeVersion)`; si el `begin` no coincide en roots o
   probeVersion → `ResetJournal` (**PELIGRO 4**).
3. `journal.Begin(newCycleID, ...)`; escribir `scan-state.json` inicial.
4. Discovery de **todos** los roots primero (barato: walk+stat) → totales reales
   para observabilidad y ETA.
5. Probing por root con `Scan` (OnItem → journal; checkpoint de estado cada 30 s).
6. Si el ctx murió → `journal.Close()`, marcar `Interrupted`, **no sync**, devolver
   error (conserva el contrato de `da8fce9`).
7. Ciclo completo: `fullCycle := sessionFullCycle(rootResults)` (función pura
   única, testeada), `BuildSyncItems` por root (RelPath correcto gracias a
   `Item.Root`), `SyncBatches` como hoy.
8. `SaveCache` (v3, compacto, fsync) → `ResetJournal` →
   `detectAndSubmitSkipSegments`. Morir entre snapshot y reset es inocuo: el
   replay produce los mismos veredictos.
9. Prewarm de sidecars **después** del sync (hoy corre antes, retrasando la
   visibilidad en el servidor minutos u horas en el primer scan).

`runAutoScan` queda en ~40 líneas. `cmd/scan.go` (`runScan`) migra al mismo
orquestador, eliminando la divergencia actual entre scan manual y auto-scan.

### 2.5 Estado observable — nuevo `internal/library/scanstate.go`

`config.DataDir()/scan-state.json`, escritura atómica pequeña:

```go
type ScanState struct {
    CycleID   string; StartedAt string; Roots []string
    Discovered, Probed, Reused, Aborted int
    Resumes          int    `json:"resumes"`
    IncompleteStreak int    `json:"incompleteStreak"`
    LastCheckpointAt string; CompletedAt string; LastFullCycleAt string
}
```

`unarr scan --status` lo muestra: "ciclo en curso 8 421/19 800 (reanudado ×2),
último ciclo completo hace 3 d". Si `IncompleteStreak >= 3`: WARN + `notify.Send`
con remedio (bajar workers / revisar mount).

### Puntos donde un error BORRA datos del usuario

1. **`fullCycle` sobre declaración incompleta** → el stale-cleanup del servidor
   (`sync.ts`) hace DELETE **sin límite de prefijo**. Guardia: `sessionFullCycle`
   devuelve true solo si (todos los roots completaron sin error) ∧ (`Aborted == 0`)
   ∧ (no `Interrupted`). Función pura con tabla de tests. Los items reanudados no
   lo comprometen: fueron re-stat-eados este ciclo.
2. **Items del sync derivados del overlay en vez del discovery** → un fichero
   borrado mientras el daemon estaba parado se resucitaría como fila fantasma que
   el cleanup nunca limpia (siempre se "ve"). Regla estructural: la lista nace
   **exclusivamente** del discovery; el overlay solo se consulta por path
   descubierto.
3. **`IsLastBatch=true` en un sync parcial** → dispara el DELETE igualmente. Los
   progress-syncs (3.3) usan una función que estructuralmente no puede emitirlo.
4. **Overlay con roots/probeVersion desalineados** → descartar journal, nunca
   "adaptar".
5. **Migración que toque `FilePath`** → ver 2.2.

## Fase 3 — Performance

### 3.1 Workers según almacenamiento

Coste real por fichero nuevo: ffprobe header (0,2-1 s local, 1-5 s NAS) +
fingerprint (2 MiB en 2 lecturas + seek) + demux de cola (ventana ≥90 s, cap 30 s)
+ decode C ocasional (cap 30 s). Peor caso ~60-125 s/fichero. Ocho ffprobe
concurrentes ya han causado OOM-kills en NAS (evidencia en el comentario de
`tailDecodeFails`).

Default adaptativo cuando `cfg.Library.Workers == 0`, en nuevo
`internal/library/fsclass_linux.go` (+ stubs por SO):

```go
// unix.Statfs(root).Type: NFS(0x6969)/CIFS(0xFF534D42)/SMB2(0xFE534D42)/FUSE(0x65735546)
//   → red:   min(3, NumCPU)   (seek-storms en arrays HDD + RTT)
//   → local: min(NumCPU, 8)
```

Config explícita siempre gana. Windows: heurística UNC / `GetDriveType`. macOS:
4 conservador. Backpressure: reutilizar `mediainfo.LoadAverage1` con el patrón de
`waitForLowLoad` (`prewarm.go:327`) entre admisiones de tier 0/1 cuando
load > 1,0×NumCPU, con cap de espera corto. No inventar mecanismo nuevo.

### 3.2 Dos pasadas dentro del ciclo (~3-4× mejor time-to-complete)

Pasada 1: header probe + fingerprint + parseo — todo lo que el sync necesita. Al
terminarla en todos los roots, **el ciclo ya es completo y sincronizable** (los
checks A/B/C solo **añaden** `truncated`/`tail_corrupt`). Pasada 2:
`AssessTruncation` sobre los ficheros sin veredicto, re-emitiendo `journal.Append`
(el replay toma la última entrada por path). Convierte "biblioteca gigante que
nunca completa" en "completa rápido, profundiza incrementalmente".

### 3.3 Progress-sync seguro

El servidor **solo borra con `isLastBatch=true`** (verificado en `sync.ts:251`),
así que un upsert parcial es seguro **hoy**, sin cambios de servidor.

```go
// SyncProgress sube items nuevos/cambiados a mitad de ciclo. Estructuralmente
// NUNCA emite IsLastBatch ni FullCycle — no existe parámetro para ello.
func SyncProgress(ctx context.Context, ac *agent.Client, items []agent.LibrarySyncItem, agentID, scanPath string, roots []string) error
```

Cadencia: cada 500 items de tier 0/1 (solo nuevos/cambiados — reenviar unchanged
duplicaría el coste de matching en el servidor).

## Fase 4 — Migración gradual y observabilidad remota

- **Re-probe gradual por `probeVersion`**: al subirlo, el planner añade un tier 4
  ("items con `ProbeVersion < probeVersion`") limitado a `max(200, 5 %)` por ciclo.
  La flota converge en días sin estampida de full-scans.
- **Progreso en heartbeat**: campos `omitempty` `ScanProbed`, `ScanTotal`,
  `ScanCycleStartedAt` en `agent.SyncRequest`; el servidor los ignora hasta que
  quiera pintarlos. Riesgo cero.
- `unarr doctor`: checks "N ciclos sin completar" y "unit file desactualizado".

## Tests

Patrones existentes: tests junto al código, `t.TempDir()`, ficheros sparse sobre
`minFileSize` (ver `TestScanCancelledContextFailsInsteadOfFlaggingFiles`,
`scanner_test.go:106` — debe seguir verde y rápido).

- `journal_test.go`: roundtrip begin/append/replay; línea final truncada →
  descartada; begin con roots distintos → overlay vacío; última entrada por path
  gana; reset.
- `scanner_resume_test.go`: con el seam `probeFile` contando invocaciones —
  escanear 5, cancelar tras journal de 2, re-escanear → exactamente 3 probes;
  orden de tiers vía registro de orden en el fake.
- `cycle_test.go`: tabla de `sessionFullCycle` (root fallido / aborted>0 /
  interrumpido / reanudado limpio → true solo el último); items del sync =
  discovery, no overlay; snapshot+reset tras ciclo; no-sync en interrupción.
- `cache_migrate_test.go`: v2→v3 conserva `FilePath` byte a byte, fingerprints y
  mtimes; JSON corrupto → nil sin pánico.
- `daemon_scan_lifecycle_test.go`: `Stop` retorna <1 s con journal flusheado.
- Timeout por fichero: probeFile que bloquea → `ScanAborted=true` en
  `PerFileTimeout`, sin cuelgue.
- Lado web: test explícito de que `syncLibraryItems` con `isLastBatch=false` no
  ejecuta ningún DELETE (hoy implícito; el progress-sync lo vuelve contrato).

## Orden de entrega

1 (lifecycle+systemd, riesgo mínimo) → 2 (journal+resume+orquestador, valor
central y todos los riesgos de borrado) → 3 (performance; 3.3 no toca servidor) →
4 (migración gradual y telemetría). Cada fase compila y se despliega sola.
