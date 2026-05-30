# unarr CLI agent — roadmap del diferenciador

> Estado de partida: **v0.9.19 beta** (~26k LOC fuente / ~18k test).
> Objetivo estratégico: el agente CLI es el **soporte real y diferenciador** de
> unarr — un *servidor de streaming personal* que la web sola no puede ser.
> Compite en **profundidad**, no en anchura (no apps nativas por dispositivo:
> el agente sirve a un único web-player responsive vía navegador).

## La visión en 6 puntos

1. **Hospeda localmente** toda la biblioteca.
2. **Debrid** para reproducir cualquier cosa cache-fast.
3. **Play-anything sin callejones** (local | debrid | descarga-y-reproduce, con
   fallback mid-stream).
4. **Transcodifica según el dispositivo** (direct-play cuando ya es compatible).
5. **Sirve a un web-player universal** en cualquier dispositivo vía navegador.
6. **Acceso remoto seguro** al agente.

## Mapa de partida (qué TIENE el agente hoy)

Sólido salvo nota:

- **Descarga torrent** (anacrolix): mmap, DHT warm-start, 30 trackers, pause/cancel,
  selección vídeo+subs `[engine/torrent.go]`. **Stream-while-download** con reader
  responsive + `PrioritizeTail` `[engine/stream.go]`.
- **Usenet** completo: NNTP pool, yEnc, ensamblado `WriteAt`, resume por segmento,
  par2 repair, unrar/7z `[usenet/*]`.
- **Debrid downloader**: GET con Range/resume `[engine/debrid.go]` — pero solo
  DESCARGA (no streaming). Resolución server-side.
- **HLS transcode** fMP4 + seek real + supervisor `[engine/hls.go]`, **caché HLS LRU**
  `[engine/hls_cache.go]`, **HW accel** NVENC/QSV/VAAPI/VideoToolbox `[engine/hwaccel.go]`.
- **Servidor HTTP** persistente: range/seek, rate-limit 2×bitrate, CORS `[engine/stream_server.go]`.
- **Library scan + ffprobe** (codec/HDR/tracks), parse título/temporada `[library/, mediainfo/]`.
- **Red**: CloudFlare Quick Tunnel `[funnel/]`, WireGuard userspace split-tunnel `[vpn/]`,
  NAT-PMP + UPnP `[engine/upnp.go]`. Web hace de broker de URLs (LAN/Tailscale/Public/Funnel).
- **Agente**: daemon cobra, sync HTTP long-poll + `/wake`, auto-upgrade opt-in,
  config.toml exhaustivo.

## Huecos (de más crítico a más bajo)

### Hueco #1 — Auth de stream  ✅ CERRADO (2026-05-31) / ver estado abajo
`/stream` y `/hls` se sirven **sin autenticación** (solo CORS+rate-limit). Con
funnel/UPnP el stream queda público en internet. Plan previo
`Docs/plans/security-stream-token.md` (deferido, sin código).

### Hueco #2 — Debrid en el path de streaming  🔵 DISEÑADO (ver estado abajo)
Hoy debrid es **solo descarga**, resuelto server-side; el streaming es 100%
torrent. La promesa "play instantáneo cache-fast" no ocurre. Falta: source debrid
en el path de streaming + cache-availability + **fallback torrent↔debrid mid-stream**.
Diseño por fases (2a direct-play / 2b HLS-desde-URL / 2c fallback) en el estado abajo.

### Hueco #3 — Device-profile + direct-play + ABR  ⬜
El path HLS **siempre re-encoda** (incluso mp4 h264/aac ya compatible). `DecideAction`
(passthrough/remux) existe pero muerto en el path browser. Sin negociación por
capacidades del dispositivo. Sin ABR multi-bitrate.

### Huecos medios  ⬜
- Sin gestión de espacio en disco (`Statfs`) → disco lleno revienta a mitad.
- Resume de torrent NO persiste reinicio del daemon (usenet sí).
- Sin seeding/ratio lifecycle (flags existen, nadie los aplica).
- Reproducir-mientras-baja: readahead estático 5MB, sin playhead→prioridad dinámica.
- HDR→SDR sin tonemap (zscale/zimg) → HDR desaturado.
- Sin thumbnails/sprites/trickplay.
- Subtítulos bitmap (PGS/DVB) sin burn-in.
- Audio siempre downmix estéreo AAC (sin passthrough 5.1).
- Mediaserver solo DETECTA Plex/Jellyfin/Emby — no biblioteca navegable propia.
- TLS solo vía funnel; LAN/Tailscale/UPnP = HTTP plano (mixed-content desde web HTTPS).
- Funnel = SPOF CloudFlare (rota ~6h), sin relay propio.
- "Tailscale Funnel" mal nombrado (no usa tsnet/Funnel real).
- Dos clientes HTTP divergentes (go-client vs agent client).
- Long-poll en vez de WS/SSE.

### Deuda puntual
`makeReadable` parchea mmap 0000 (frágil NFS) · par2/unrar degradan en silencio si
falta binario · VAAPI workarounds por host · cloudflared sin verificación de firma ·
WireGuard endpoint sin pin · sesión única (1 viewer).

## Mejoras detectadas durante el trabajo (backlog)

> Se rellena a medida que se trabaja cada hueco. Cada entrada: qué, por qué, prioridad.

- **Clock-skew en verificación de token** (baja): `verifyStreamToken` no tolera skew; con TTL 6h y NTP es irrelevante, pero el HLS lo mintea el web y lo verifica el agente (relojes distintos). Considerar ~60s de gracia si aparecen 404 espurios.
- **Secreto de stream en claro en DB** (baja): `agent_registration.stream_secret` es una clave HMAC viva (por arranque) en la DB central; quien lea la DB puede mintear tokens HLS de cualquier agente. Inherente al diseño (el web debe mintear HLS). Mitigado por regeneración por arranque. Excluir esta columna de cualquier JSON admin/usuario.
- **Refrescar/limpiar streamUrl al re-registrar** (baja): tras reinicio del daemon el secreto cambia; URLs `?t=` ya guardadas en `download_task.streamUrl` quedan stale hasta re-stream. Es auto-curativo, pero el web podría limpiar streamUrl en el re-register del agente.
- **gofmt preexistente** en `internal/agent/types.go` (StreamSession) y `hls.go`/`torrent.go`/`stream_source.go` (no introducido por este trabajo) — chore aparte.
- **Funnel = SPOF CloudFlare** (ya en huecos medios): el funnel sigue siendo trycloudflare; relay propio pendiente.

---

## ESTADO POR HUECO

### Hueco #1 — Auth de stream
**Estado:** 🟡 en progreso (iniciado 2026-05-31).

**Enfoque elegido** (mejora sobre el plan previo, menor blast radius — sin migración DB):
token **HMAC stateless minteado por el propio agente**. El agente ya construye las
stream URLs que reporta a la web (`daemon.go` → `streamSrv.URLsJSON()`), así que
puede firmar el token, embeberlo en la URL, y verificarlo en cada request — la web
es passthrough (cambio web ~nulo).

- Secreto: 32 bytes random en memoria del daemon (rota al reiniciar).
- Token: `<expUnix>.<hexHMAC(secret, scope:exp)>`, TTL 6h.
- `/stream` + VLC: token en query `?t=`; scope `"stream"`.
- `/hls`: token en **path** `/hls/<sessionID>/<token>/<resource>`; scope `"hls:<sessionID>"`.
  Los URIs hijos de los playlists son **relativos** → el token se propaga solo a
  segmentos/subs sin reescribir playlists.
- **Loopback exento** (mpv/vlc local + health-probe siguen funcionando; el token solo
  gatea acceso remoto LAN/Tailscale/Public/funnel).
- Config `require_stream_token` (default **true**, seguro por defecto).

**Hecho (CERRADO 2026-05-31):**

CLI (`torrentclaw-cli`):
- `internal/engine/stream_token.go` (nuevo): `mintStreamToken`/`verifyStreamToken` (HMAC-SHA256, constant-time), `newStreamSecret` (32 bytes; **fail-hard** si crypto/rand falla, sin fallback débil).
- `internal/engine/stream_server.go`: secreto + `requireToken` en StreamServer; `/stream` y `/hls` verifican el token; `URLsJSON`/`hlsBaseURLs`/`URL()` tokenizan; `StreamSecretHex()`; **sin exención de loopback**; `/playlist.m3u` ya no auto-mintea (cerrado el oracle).
- `internal/config/config.go`: `require_stream_token` (default true).
- `internal/agent/{types,daemon}.go` + `internal/cmd/daemon.go`: el agente reporta el secreto en register **solo si enforcing**.
- Tests: `stream_token_test.go` (mint/verify/expiry/tamper/scope/secret, handler /stream + /hls, **vector de paridad cross-lenguaje**).

WEB (`torrentclaw-web`):
- `src/lib/stream-token.ts` (nuevo): minter HMAC en TS (paridad byte a byte con Go, guard de clave 64-hex).
- `src/app/api/internal/stream/session/route.ts`: `buildHlsUrls` inyecta el token de path usando el secreto del agente.
- `src/lib/db/schema.ts` + migración `0134_grey_chat.sql`: columna `agent_registration.stream_secret` (ADD COLUMN nullable, segura).
- `src/app/api/internal/agent/register/route.ts` + `src/lib/services/agent.ts`: valida (64-hex) + persiste + expone en `getAgentHealth`.
- Tests: `tests/unit/stream-token.test.ts` (paridad + guard).

**Revisión adversarial** (workflow 4 dimensiones) → 1 crítico + 3 high corregidos antes de cerrar:
- **CRÍTICO**: la exención de loopback dejaba el **funnel CloudFlare** sin protección (cloudflared proxya tráfico público vía `localhost` → todo el funnel llegaba como loopback). **Fix: eliminada la exención.** Toda URL entregada ya va tokenizada, así que ningún cliente legítimo se rompe; el funnel ahora lleva el token en la URL y verifica.
- **HIGH** `/playlist.m3u` era oracle de tokens (fallback self-minting) → **fix: 404 sin streamUrl**.
- **HIGH** gate de version-skew mal señalizado (el agente reportaba el secreto aunque enforcement=off) → **fix: reportar solo si enforcing**.
- **HIGH** new-agent+old-web rompe HLS remoto → **mitigación por orden de deploy (ver abajo)**, sin tolerar tokenless (no reabrir el agujero).

**Verificación:** CLI `go build/vet/test ./...` ✓; WEB typecheck+lint+2325 unit ✓; paridad cross-lenguaje verificada en ambos sentidos.

> ⚠️ **ORDEN DE DEPLOY (obligatorio):** desplegar **primero el WEB** (columna `stream_secret` + minteo HLS), **luego** publicar el binario del agente. Un agente nuevo (enforce por defecto) contra un web viejo (sin minteo HLS) rompería el HLS remoto. El web es retrocompatible (agente viejo sin secreto → URLs sin token). Smoke real de extremo a extremo (daemon + funnel + navegador) **pendiente de hacer con un agente desplegado** — los tests cubren mint/verify/handlers y la paridad, no el round-trip cloudflared en vivo.

---

### Hueco #2 — Debrid en el path de streaming
**Estado:** 🔵 DISEÑADO (2026-05-31), listo para implementar en sesión fresca.

**Problema (confirmado en el análisis):** hoy `debrid` es **solo descarga**
(`engine/debrid.go` baja la `DirectURL` HTTPS resuelta server-side). El
streaming es **100% torrent**: `daemon.OnStreamSession` arma el provider desde
`sess.FilePath`/`sess.InfoHash`/`sess.TaskID` y `StreamSession` **no lleva
DirectURL**. La promesa "play instantáneo cache-fast por debrid" no ocurre.

**Arquitectura de providers (lo que ya hay):** `FileProvider{ NewFileReader(ctx)
io.ReadSeekCloser; FileName(); FileSize() }`. Implementaciones: `torrentFileProvider`,
`diskFileProvider`, `StreamEngine`. El /stream sirve un `FileProvider` via
`http.ServeContent` (range/seek). El HLS arranca una `HLSSession` desde una ruta
de fichero (ffmpeg `-i <path>`).

**Diseño por fases (de menos a más riesgo):**

- **Fase 2a — debrid como fuente de /stream (direct-play).** *Slice completo y
  acotado.*
  1. Añadir `DirectURL string` a `StreamSession` (web→agente) y a su validación.
  2. Nuevo `debridFileProvider` (`FileProvider`): `NewFileReader` devuelve un
     `io.ReadSeekCloser` que hace **GET con Range** contra la `DirectURL` (debrid
     ya soporta Range, ver `debrid.go`); `FileSize` via HEAD o `sess.FileSize`;
     `Seek` traducido a `Range:`. Reutilizar la lógica de `debrid.go` (416,
     Content-Range, reintentos).
  3. En `OnStreamSession`: si `sess.DirectURL` presente → `debridFileProvider`
     → `SetFile`. (Direct-play; el navegador hace range sobre el provider.)
  4. Web: al crear la sesión de stream, si el contenido está **cacheado en
     debrid**, resolver la `DirectURL` server-side (como en descargas) e incluirla
     en el `StreamSession`. Señal de cache: `debridCacheStatus` fresh (ya existe).
  5. Tests: `debridFileProvider` con un httptest server que sirve Range; round-trip
     /stream con provider debrid.

- **Fase 2b — HLS desde URL (transcode de fuentes debrid no-compatibles).**
  ffmpeg lee HTTP directo (`-i https://…`), así que `HLSSession` puede aceptar
  una URL como source en vez de una ruta. Mayor cambio en el pipeline HLS
  (timeouts, reintentos de red, headers). Permite transcodear contenido debrid.

- **Fase 2c — selección cache-fast + fallback mid-stream ("sin callejones").**
  - Conciencia de cache en el agente o señal del web para **preferir debrid
    cacheado sobre torrent** cuando aplique (hoy `resolve.go:22` pone torrent
    primero).
  - **Fallback mid-stream**: si la fuente activa muere (peers a 0 / 5xx debrid),
    cambiar a la otra sin cortar la reproducción. Complejo (estado de sesión,
    re-seek). Es lo que de verdad cierra "play-anything sin callejones".

**Ficheros a tocar:** CLI `internal/engine/{stream_server.go (provider), debrid.go,
hls.go (2b)}`, `internal/agent/types.go` (+DirectURL), `internal/cmd/daemon.go`
(wiring). WEB `src/app/api/internal/stream/session/route.ts` (resolver DirectURL +
cache), `src/lib/services/agent.ts`.

**Partes difíciles / riesgos:** ranged reader robusto sobre HTTP (reconexión,
timeouts), HLS-desde-URL (red dentro de ffmpeg), y el fallback mid-stream (estado).
Empezar por 2a (valor inmediato, riesgo bajo), 2b y 2c como iteraciones.

**Mejora detectada:** `resolve.go:22` ordena `torrent > debrid > usenet`; para el
diferenciador cache-fast convendría que, **cuando hay cache debrid confirmada**,
el orden de STREAMING (no el de descarga) prefiera debrid.
