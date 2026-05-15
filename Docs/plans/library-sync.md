# Plan: Sincronización bidireccional de biblioteca (CLI ↔ Web)

## Context
La biblioteca web solo muestra descargas completadas (download_task + debrid). El `unarr scan` escanea ficheros con ffprobe y los sube al servidor, pero solo soporta un path, no detecta borrados del disco, y no permite borrar ficheros desde la web. El usuario quiere una biblioteca unificada que refleje el estado real de su colección y se sincronice en ambas direcciones.

## Protocolo de sincronización

### Forward Sync (Disco → Web)
1. CLI escanea todos los `ScanPaths` configurados
2. Para cada path: descubre ficheros, compara con cache (skip ffprobe si no cambió), sube a `/library-sync`
3. En `isLastBatch=true`: el servidor elimina items con ese `scanPath` que no estén en el batch (ficheros borrados del disco desaparecen de la web)

### Reverse Sync (Web → Disco)
1. CLI llama a `GET /agent/library-deletions` — items que el usuario soft-deleted desde la web
2. Si `AutoDelete=true` o `--yes`: borra ficheros del disco
3. Si no: muestra lista y pide confirmación interactiva
4. Llama a `POST /agent/library-deletions/confirm` con los IDs confirmados → hard-delete en DB

### Resolución de conflictos
- Fichero en disco pero no en web → forward sync lo añade
- Fichero en web pero no en disco → forward sync lo elimina (isLastBatch)
- Soft-deleted en web, aún en disco → reverse sync lo borra del disco y confirma
- Soft-deleted en web, ya borrado del disco → reverse sync confirma directamente
- Race condition (user borra en web mientras CLI escanea) → forward sync skippea rows con `deleted_at IS NOT NULL`

---

## Fase 1: Multi-path + Forward Sync mejorado

### 1.1 CLI — Config multi-path
**Archivo:** `torrentclaw-cli/internal/config/config.go`
- Añadir `ScanPaths []string` a `LibraryConfig`
- Migrar `ScanPath` → `ScanPaths[0]` en `Load()` si `ScanPaths` está vacío
- Añadir `AutoDelete bool` (default false)

### 1.2 CLI — Cache v2
**Archivo:** `torrentclaw-cli/internal/library/types.go`
- Cambiar `LibraryCache` a version 2: `Paths map[string][]LibraryItem`
- Migración v1→v2: `Path`+items → `Paths[Path]`

**Archivo:** `torrentclaw-cli/internal/library/cache.go`
- `LoadCache` detecta versión y migra
- `SaveCache` siempre guarda v2

### 1.3 CLI — Scan multi-path
**Archivo:** `torrentclaw-cli/internal/cmd/scan.go`
- `unarr scan` sin args → escanea todos los `ScanPaths`
- `unarr scan /path/a /path/b` → escanea paths específicos y los recuerda en config
- Loop: para cada path, scan + sync con su `scanPath`

### 1.4 CLI — Nuevo comando `unarr sync`
**Archivo nuevo:** `torrentclaw-cli/internal/cmd/sync.go`
- Forward sync: scan ligero (sin ffprobe para ficheros sin cambios) + upload
- Sin reverse sync todavía (Fase 3)
- Flags: `--dry-run`, `--paths`

### 1.5 Web — Columna `scan_path` en `library_item`
**Archivo:** `torrentclaw-web/src/lib/db/schema.ts`
- Añadir `scanPath: varchar(2048)` a tabla `libraryItem`
- Generar migración con `pnpm db:generate`

**Archivo:** `torrentclaw-web/src/lib/services/library-upgrade.ts`
- `syncLibraryItems()`: persistir `scanPath` en cada row al hacer upsert

### 1.6 CLI — Daemon multi-path
**Archivo:** `torrentclaw-cli/internal/cmd/daemon.go`
- `runAutoScan()` itera sobre todos los `ScanPaths`

---

## Fase 2: Reverse Sync (Web → Disco)

### 2.1 Web — Soft-delete
**Archivo:** `torrentclaw-web/src/lib/db/schema.ts`
- Añadir `deletedAt: timestamp` a tabla `libraryItem`
- Generar migración

### 2.2 Web — Endpoints de borrado
**Archivo nuevo:** `torrentclaw-web/src/app/api/internal/library/items/route.ts`
- `DELETE` — session auth, recibe `{itemIds: number[]}`, hace soft-delete (`deletedAt = NOW()`)

**Archivo nuevo:** `torrentclaw-web/src/app/api/internal/agent/library-deletions/route.ts`
- `GET` — agent auth, devuelve items con `deletedAt IS NOT NULL` para ese usuario
- `POST` — agent auth, recibe `{confirmedIds: number[]}`, hard-delete los rows

### 2.3 Web — Heartbeat con pendingDeletions
**Archivo:** endpoint de heartbeat del agente
- Añadir `pendingDeletions: number` al response (count de items con `deletedAt IS NOT NULL`)

### 2.4 Web — Forward sync respeta soft-deletes
**Archivo:** `torrentclaw-web/src/lib/services/library-upgrade.ts`
- `syncLibraryItems()` en `isLastBatch`: la query de DELETE excluye rows con `deletedAt IS NOT NULL`

### 2.5 CLI — Agent client nuevos métodos
**Archivo:** `torrentclaw-cli/internal/agent/client.go`
- `GetLibraryDeletions(ctx) → []DeletionItem`
- `ConfirmLibraryDeletions(ctx, ids []int) → error`

**Archivo:** `torrentclaw-cli/internal/agent/types.go`
- `DeletionItem {ID int, FilePath string, DeletedAt string}`

### 2.6 CLI — Sync reverse
**Archivo:** `torrentclaw-cli/internal/cmd/sync.go`
- Después del forward sync: llama a `GetLibraryDeletions()`
- Valida que cada fichero está dentro de un `ScanPaths` conocido (seguridad)
- Si `AutoDelete` o `--yes`: borra y confirma
- Si no: muestra lista interactiva, pide confirmación
- Flag `--no-delete` para skip reverse sync
- Si `BackupDir` configurado: mover a backup en vez de borrar

### 2.7 CLI — Daemon auto-delete
**Archivo:** `torrentclaw-cli/internal/cmd/daemon.go`
- Al final de `runAutoSync()`: si `AutoDelete=true`, procesa deletions automáticamente
- Si no: log warning "N files pending deletion, run `unarr sync`"

---

## Fase 3: Web UI (brief)

- Botón "Eliminar" en items de biblioteca → llama `DELETE /library/items`
- Badge "Pendiente de borrar" en items soft-deleted
- Posibilidad de cancelar el borrado (clear `deletedAt`)
- Vista unificada: scanned items + downloaded items en la misma vista

---

## Archivos clave

### CLI (Go)
| Archivo | Cambio |
|---------|--------|
| `internal/config/config.go` | ScanPaths, AutoDelete, migración |
| `internal/library/types.go` | Cache v2 con Paths map |
| `internal/library/cache.go` | Load/Save v2, migración v1 |
| `internal/library/sync.go` | BuildSyncItems (sin cambios) |
| `internal/cmd/scan.go` | Multi-path loop |
| `internal/cmd/sync.go` | **Nuevo** — comando sync bidireccional |
| `internal/cmd/daemon.go` | runAutoSync multi-path + reverse |
| `internal/agent/client.go` | GetLibraryDeletions, ConfirmLibraryDeletions |
| `internal/agent/types.go` | DeletionItem type |

### Web (TypeScript)
| Archivo | Cambio |
|---------|--------|
| `src/lib/db/schema.ts` | scanPath + deletedAt en library_item |
| `src/lib/services/library-upgrade.ts` | persistir scanPath, respetar soft-deletes |
| `src/app/api/internal/agent/library-deletions/route.ts` | **Nuevo** — GET + POST |
| `src/app/api/internal/library/items/route.ts` | **Nuevo** — DELETE soft-delete |
| Endpoint heartbeat del agente | pendingDeletions en response |

---

## Verificación

### Fase 1
1. `go build ./cmd/unarr/ && go test ./...`
2. Configurar 2 scan paths en config.toml, ejecutar `unarr scan` → ambos se escanean
3. Borrar un fichero del disco, ejecutar `unarr scan` → desaparece de la web
4. `pnpm build` en torrentclaw-web para verificar tipos

### Fase 2
1. Desde la web: borrar un item de la biblioteca
2. Ejecutar `unarr sync` → muestra el fichero pendiente de borrar, pedir confirmación
3. Confirmar → fichero se borra del disco y desaparece de la web
4. `unarr sync --dry-run` → muestra lo que haría sin hacer nada
5. Con `auto_delete = true` en config: el daemon borra automáticamente

### Fase 3
1. Verificar visualmente en Chrome DevTools la UI de borrado
2. Verificar que el badge "pendiente" aparece y desaparece correctamente
