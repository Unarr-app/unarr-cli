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

### Hueco #2 — Debrid en el path de streaming  ✅ CERRADO (2a+2b+2c, 2026-05-31)
Hoy debrid es **solo descarga**, resuelto server-side; el streaming es 100%
torrent. La promesa "play instantáneo cache-fast" no ocurre. Falta: source debrid
en el path de streaming + cache-availability + **fallback torrent↔debrid mid-stream**.
Diseño por fases (2a direct-play / 2b HLS-desde-URL / 2c fallback) en el estado abajo.

### Hueco #3 — Device-profile + direct-play + ABR  ✅ CERRADO (2026-05-31) / ver estado abajo
El path HLS re-encodaba todo (incluso mp4 h264/aac ya compatible). `DecideAction`
muerto. Sin negociación por capacidades. Sin adaptación de calidad.
Diseño por fases (3a direct-play / 3b remux fMP4 / 3c capability-negotiation / 3d ABR)
en el estado abajo. **3a + 3b + 3c CERRADAS** (smoke e2e, incl. HEVC en iPhone Safari
real). **3d resuelto como 3d-lite (auto-downshift)** — ABR multi-rendition real
descartada (N× CPU inviable single-viewer; no aplica a paths copy). Hueco COMPLETO.

### Hueco #4 — Pre-transcode (transcode-on-download)  🔵 DISEÑADO (ver estado abajo)
Al completar una descarga/import, transcodificar/remuxar en background para que el
PRIMER play sea instantáneo (direct o cache-HIT), sin transcode en vivo.
Optimización, nunca bloqueante: si no terminó a tiempo → fallback a transcode en
vivo (HLS actual). Reaprovecha `hls_cache.go` (cache-HIT ya sirve instantáneo) +
el pipeline de `prewarm` (ya hace encode de la siguiente ep) — generaliza prewarm a
"todo download, configurable" y puebla también el artefacto direct-play. Configurable
desde la web. Diseño + set de opciones en el estado abajo.

### Huecos medios  ⬜
- ~~Sin gestión de espacio en disco (`Statfs`)~~ ✅ **Pre-flight de espacio (2026-05-31)** — `CheckDiskSpace` antes de cada descarga (torrent/usenet/debrid) con reserva configurable `downloads.min_free_disk_mb` (default 2048); manager NO hace fallback en disco lleno; aviso web 507 `INSUFFICIENT_DISK` al despachar (torrentclaw). Monitoreo mid-download diferido. Ver estado abajo.
- ~~Resume de torrent NO persiste reinicio del daemon~~ ✅ **Auto-resume tras reinicio (2026-05-31)** — `agent.ActiveTaskStore` persiste los `agent.Task` de descargas en vuelo (`active-tasks.json`); el daemon los re-somete al arrancar → los downloaders reanudan los bytes (torrent vía completion DB de anacrolix, debrid vía Range, usenet vía tracker). Dedup en `manager.Submit` (restore + re-despacho web no duplican). `shuttingDown` preserva el entry en apagado limpio (solo terminal genuino lo borra). Ver estado abajo.
- ~~Sin seeding/ratio lifecycle (flags existen, nadie los aplica)~~ ✅ **Seeding/ratio lifecycle (2026-06-01)** — `seed_enabled`/`seed_ratio`/`seed_time` en `[downloads]` (opt-in, off por defecto) cableados al daemon; al completar una descarga con seeding activo el torrent sigue subiendo en background y un monitor lo dropea al alcanzar ratio (subido/tamaño) O tiempo (lo primero que toque); sin target = siembra hasta apagado. `cleanup()` ahora siempre dropea (arregla fuga en rutas de error con seeding on). Verificado con swarm loopback real. Ver estado abajo.
- ~~Reproducir-mientras-baja: readahead estático 5MB~~ ✅ **Readahead dinámico (2026-05-31)** — `dynamicReadahead(bitrate)` = ~30s de vídeo (clamp 8–96 MiB; default 24 MiB sin bitrate) en vez de 5 MiB fijos (~1.9s a 20 Mbps → se atascaba). anacrolix ya prioriza piezas en esa ventana por delante del playhead + en seek; solo faltaba dimensionarla. Bitrate probado async (sin coste TTFF). Ver estado abajo.
- ~~HDR→SDR sin tonemap~~ ✅ **Tonemap HDR→SDR (2026-05-31)** — cadena `zscale+tonemap=hable` en el transcode HLS cuando la fuente es HDR y el ffmpeg trae zscale (detectado en runtime, `FFmpegSupportsZscale`; sin zscale → comportamiento actual, no rompe). Verificado en 4K HDR10 real: de lavado/desaturado a colores vívidos. Ver estado abajo.
- ~~Sin thumbnails~~ ✅ **Fotogramas bajo demanda (2026-05-31)** — `GET /thumbnail` (ffmpeg 1 frame, `-ss` antes de `-i`, MJPEG) + panel "Características del fichero" (ruta + mediainfo completa + tira de ~5 frames). Ver estado abajo.
- ~~Sin trickplay (preview en la barra)~~ ✅ **Trickplay bajo demanda (2026-06-01)** — pista WebVTT `thumbnails` con 1 cue/10s; cada cue es una URL `/thumbnail?pos=…#xywh=0,0,W,H` (frame completo), así media-chrome solo descarga el frame que se sobrevuela. Toggle on/off por navegador (`localStorage`, default ON) + doc (web `docs/architecture/trickplay.md`). Alcance "on-demand" decidido con el usuario. Sin pregenerar sprite/BIF (sigue siendo opción futura con cacheo en disco). Ver estado abajo.
- ~~Subtítulos bitmap (PGS/DVB) sin burn-in~~ ✅ **Burn-in PGS/DVB bajo demanda (2026-06-01)** — el usuario elige una pista bitmap en el reproductor → la sesión fuerza HLS y el agente re-codifica con `[0:v:0]<vchain>[base];[0:s:N][base]scale2ref[sub][base2];[base2][sub]overlay[vout]` (overlay tras el tonemap = brillo SDR correcto; scale2ref = encaje a cualquier resolución del PGS). En la cache key. Selector web alimentado de file-details (funciona también en direct-play). Caveat: PGS + seek pierde el subtítulo. Verificado en Sonic BDremux (ES quemado). Ver estado abajo.
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
- **Data race preexistente manager↔reporter (baja)**: bajo `-race`, `Task.ToStatusUpdate()` (leído por `ProgressReporter.flushBatch`) corre sin lock contra la escritura de campos del task en `processTask` (`manager.go:371`). No introducido por el resume; expuesto al correr la suite con `-race` (la suite normal corre sin `-race`). Fix: proteger los campos de estado/progreso del `Task` con su `mu` en ToStatusUpdate + processTask. Chore aparte. Múltiples `task.ID[:8]` en `progress.go`/`torrent.go` paniquean con ids <8 chars (irreal: el web manda UUIDs) — limpiar a `ShortID` de paso.
- **Funnel = SPOF CloudFlare** (ya en huecos medios): el funnel sigue siendo trycloudflare; relay propio pendiente.
- ~~**Rutas localizadas unarr 404 (media)**~~ ✅ **ARREGLADO (2026-05-31)**: bajo `NEXT_PUBLIC_BRAND=unarr` el allowlist `UNARR_PAGE_PREFIXES` (paths EN) no reconocía los localizados de next-intl (`/es/biblioteca`, `/es/descargas`, `/es/perfil`) → 404. Fix (web): `enFirstSegmentByLocalized` (mapa localizado→EN derivado de `routing.pathnames`) + `toCanonicalPath()` en `branding/routes.ts` traduce el 1er segmento antes del match. Assertion anti-colisión en el build del mapa (fail-fast si una ruta futura reusa un segmento → no puede colar una ruta denegada). Verificado: 175 entradas, cero crossover; denegadas siguen denegadas.
- ~~**Thumbnails — sprites/trickplay (media)**~~ ✅ **Trickplay CERRADO bajo demanda (2026-06-01)**: la preview de barra usa cues `/thumbnail` en vivo (un frame por cue al sobrevolar), no un sprite pregenerado. El sprite/BIF de toda la timeline con cacheo en disco del agente sigue siendo una optimización futura (no necesaria para la UX actual). Ver estado abajo.
- **nvenc "Invalid Level" en fuentes anamórficas (alta — destapado en el smoke de trickplay)** ✅ **ARREGLADO (2026-06-01)**: el nivel H.264 del transcode HLS se derivaba solo de la altura → una fuente 2.39:1 escalada a 1080 (~2586×1080 = 11016 MBs) revienta el `MaxFS` de L4.1 (8192); ffmpeg fallaba (`InitializeEncoder failed: invalid param (8): Invalid Level` en nvenc, `frame MB size > level limit` en libx264) y la sesión no producía ningún segmento. Casi todos los rips 4K son anamórficos → reproducción HLS rota en silencio. Fix (`hwaccel.go`): `H264LevelForFrame(width,height)` deriva el nivel del recuento de macrobloques real (máx. entre el nivel por-altura y el por-MB); `hls.go` calcula el ancho de salida y lo usa. Ver estado abajo.

### Hueco medio — Readahead dinámico (ver-mientras-baja)  ✅ CERRADO (2026-05-31)
El lector de torrent usaba un readahead **estático de 5 MiB** (~1.9s de un stream 4K de 20 Mbps) → al reproducir un torrent a medio bajar, la reproducción adelantaba a la descarga y se atascaba.
- `dynamicReadahead(bitrateBps)` (`readahead.go`): ~30s de vídeo, clamp [8, 96] MiB; default 24 MiB cuando el bitrate es desconocido (ya ~5× el viejo 5 MiB). anacrolix (`SetResponsive`+`SetReadahead`) ya prioriza las piezas de esa ventana por delante del read position y re-prioriza en seek — el feedback playhead→prioridad estaba; solo faltaba dimensionar la ventana.
- `torrentFileProvider` lleva `bitrateBps atomic.Int64`, sondeado **async** (`probeMediaInfo` en goroutine vía DataDir+DisplayPath) — sin coste de TTFF; hasta resolverse usa el default, y los readers posteriores (cada range/seek crea uno) cogen el valor preciso. StreamEngine (CLI) → default 24 MiB.
- **Smoke**: ffprobe en 4K real (20.7 Mbps) → readahead **73 MiB** (~28s) vs 5 MiB. Tests del func puro + -race limpio en el probe async. /critico: código sólido, fix aplicado (probe síncrono→async para eliminar 3s de TTFF si falta la cabecera).

### Hueco medio — Trickplay (preview en la barra)  ✅ CERRADO (2026-06-01)
Preview de fotograma al pasar el ratón por la barra de búsqueda, **bajo demanda** (sin pregenerar sprite). Alcance decidido con el usuario: on-demand + UX no invasiva + activable/desactivable + documentado.
- **Web** (rama `feat/unarr-brand`): `buildTrickplayVtt()` (`src/lib/stream/trickplay.ts`) emite una pista WebVTT `thumbnails` con 1 cue/10s; cada cue apunta a `GET /thumbnail?pos=<seg>&w=320#xywh=0,0,W,H` (frame completo, alto par derivado del aspecto). media-chrome solo descarga el frame sobrevolado y lo cachea. Wiring en `HlsStreamPlayer` (fetch a `file-details` → blob VTT → `<track>`), botón on/off + var CSS de fondo en `MediaChromePlayer`, toggle por navegador en `localStorage` (`useTrickplay`, default ON). Doc: `docs/architecture/trickplay.md`. Tests: `trickplay.test.ts` (6, formato cue + alto par + token vacío + inputs insuficientes).
- **Smoke real** (iPhone-equiv en Chrome, F1 4K DV+HDR10): vídeo reproduce → hover en la barra renderiza un frame real en la posición (1:17:36) ≠ el frame en curso; etiqueta de tiempo inmediata; toggle off → `<track>` desaparece (sin preview) y persiste `localStorage="0"`; toggle on → vuelven los 932 cues. CORS del `<img crossorigin>` OK (allowlist del agente).
- **No invasivo**: nada carga hasta el hover; 1er frame ~0.8–2.1s en 4K-desde-NAS, re-hover instantáneo (caché navegador); la etiqueta de tiempo aparece ya aunque el frame se esté generando.

### Hueco medio — Burn-in de subtítulos bitmap (PGS/DVB)  ✅ CERRADO (2026-06-01)
Los subs de imagen (PGS/DVB/VOBSUB) no se pueden servir como WebVTT; se incrustan en el vídeo durante el transcode. Alcance (decidido con el usuario): bajo demanda + nudge cuando el fichero SOLO tiene bitmap (sin auto-activar).
- **Agente** (rama `unarr-burnin` ex `feat/unarr-agent`): `HLSSessionConfig.BurnSubtitleIndex *int` (nil=sin burn; puntero para que el 0 no se confunda con "quema pista 0"); en la cache key (`KeyFor`/`KeyForID`). `buildHLSFFmpegArgsAt`: si el índice apunta a una pista bitmap válida, `-map [vout]` + `-filter_complex [0:v:0]<vchain>[base];[0:s:N][base]scale2ref[sub][base2];[base2][sub]overlay[vout]`. Overlay TRAS el tonemap (subs SDR no se aplastan); scale2ref encaja el lienzo PGS al frame. Índice inválido/texto/fuera de rango → fallback a encode limpio (log). `IsTextSubtitle` ahora incluye `"text"` (paridad con el clasificador web). Tests `TestBuildHLSFFmpegArgsBurnSubtitle` (filter_complex/overlay/[vout] vs -vf según bitmap/texto/rango) + cache-key.
- **Web** (rama `unarr-burnin` ex `feat/unarr-brand`): columna `streaming_session.burn_subtitle_index` (migración 0139, NOT NULL default -1) en identidad de sesión + dedup; `session/route` fuerza `playMethod=hls` cuando hay burn; `agent.ts` lo pasa al daemon. Selector en `MediaChromePlayer` alimentado de **file-details** (`subtitleTracks`, mediainfo estática) → aparece también en direct-play; posición del array = `-map 0:s:N`. `isBitmapSubtitleCodec` (`src/lib/stream/subtitles.ts`) espeja `IsTextSubtitle`. Notice: "incrustando" al quemar / nudge si solo-bitmap. Doc: `docs/architecture/subtitle-burn-in.md`.
- **Smoke real** (Sonic 2020 BDremux 1080p, 7 PGS + 1 subrip): selector lista los 7 PGS (EN/ES/NL · imagen), excluye el subrip; elegir ES (`0:s:2`) fuerza HLS, el agente transcodifica con overlay sin error y el frame muestra **"Sé lo que estáis pensando."** quemado (posición + brillo correctos). /critico 2 revisores: arreglado `"text"` (paridad), reset de burn al cambiar de ítem, `bitmapSubtitles` a flatMap.
- **Caveat**: PGS + seek pierde el subtítulo (el `-ss` antes de `-i` tira el estado del decoder PGS). Reproducción lineal desde el inicio = OK. Mitigación futura: decodificar PGS desde el epoch cercano.
- **Aislamiento**: este trabajo se hizo en worktrees dedicados (`/tmp/tc-unarr-{web,cli}`, rama `unarr-burnin`) tras una colisión de ramas en los checkouts primarios compartidos. Merge a `feat/unarr-{brand,agent}` pendiente de decisión del operador.

### Bug agente — nvenc "Invalid Level" en fuentes anamórficas  ✅ ARREGLADO (2026-06-01)
Destapado durante el smoke de trickplay: el HLS de un 4K anamórfico (3840×1604, 2.39:1) no producía **ningún** segmento.
- **Causa**: el nivel H.264 se derivaba solo de la altura de salida (`H264LevelForHeight`). Escalado a 1080 de alto, un 2.39:1 queda ~2586×1080 = 11016 macrobloques, que supera el `MaxFS` del nivel 4.1 (8192). ffmpeg fallaba al abrir el encoder (`InitializeEncoder failed: invalid param (8): Invalid Level` en h264_nvenc; el equivalente `frame MB size > level limit` en libx264) → 0 paquetes → la sesión se quedaba en "preparando sesión" hasta el timeout de mark-ready. Casi todo rip 4K es 2.39:1, así que la reproducción HLS estaba rota para la mayoría de pelis 4K (en silencio).
- **Fix** (`hwaccel.go` + `hls.go`): `H264LevelForFrame(width, height)` deriva el nivel del recuento de macrobloques real (`levelForMacroblocks`, tabla MaxFS de la spec) y devuelve el máximo entre ese y el nivel por-altura (que conserva el margen de fps/MBPS). `hls.go` calcula el ancho de salida (`probe.Width * outputHeight / probe.Height`, par) y llama a `H264LevelForFrame`. 16:9 no cambia (mismo resultado que antes); anamórfico sube a 5.0 cuando hace falta. `transcoder.go` no se toca (su `SourceHeight` nunca se rellena → ya cae al default seguro 5.1).
- **Reproducido + verificado**: con `/usr/bin/ffmpeg` 6.1.1 + nvenc, `testsrc=2586x1080 @ -level:v 4.1` reproduce el error exacto; `@ 5.0` codifica OK. Tras el fix, sesión HLS del F1 4K arranca sin "Invalid Level"/auto-restart/timeout y el `<video>` carga (`readyState 4`, `duration 9313s`). Tests `H264LevelForFrame` (16:9 sin regresión + anamórfico → 5.0).

### Hueco medio — Seeding/ratio lifecycle  ✅ CERRADO (2026-06-01)
Los flags `SeedRatio`/`SeedTime` (`TorrentConfig`) estaban DECLARADOS pero nadie los consumía, y `SeedEnabled` estaba hardcodeado a `false` en ambos constructores → el daemon nunca sembraba y, si se forzaba, sembraba para siempre.
- **Config** (`config.go`): `[downloads]` += `seed_enabled` (bool), `seed_ratio` (float), `seed_time` (string duración tipo `"24h"`). Opt-in, off por defecto (zero-values = apagado, sin entradas en `applyDefaults`). Tests `TestLoadSeeding{DefaultsOff,Explicit}`.
- **Wiring** (`daemon.go`): parsea `seed_time` (`time.ParseDuration`) y cablea los 3 campos a `TorrentConfig`; log de arranque que distingue ratio / tiempo / ambos / indefinido. El `unarr download` one-shot (foreground) sigue `SeedEnabled:false` a propósito (leech + exit; comentado).
- **Ciclo** (`torrent.go`): `seedTargetReached(ratio, time, uploaded, size, elapsed)` puro (ratio = subido/tamaño-seleccionado, estable entre resumes; el primero de ratio>0 o tiempo>0 que se cumple gana; ambos 0 = nunca para). `seedAndDrop` corre detached en un `seedCtx` propio del downloader (cancelado en `Shutdown`) — NO el ctx de la task, que se cancela en cuanto `Download` retorna y el manager libera el slot. Tick configurable (`seedCheckInterval`, default 30s; tests lo bajan). Sale sin dropear si el handle ya se quitó de `d.active` (cancel/pause del usuario) → ni lee stats de un torrent cerrado ni dropea dos veces.
- **Bug latente arreglado de paso**: `cleanup()` tenía `if !SeedEnabled { Drop }` — en rutas de ERROR (metadata timeout, disco, poll) con seeding activo borraba de `d.active` pero NO dropeaba → fuga. Ahora `cleanup()` siempre dropea (solo lo llama el error-path y el éxito-sin-seeding); el éxito-con-seeding hace el handoff a `seedAndDrop`.
- **Smoke real** (`seed_lifecycle_smoke_test.go`, tag `smoke`): swarm loopback de dos clientes anacrolix (un seeder sirviendo 4 MiB + nuestro `TorrentDownloader` leecheándolo vía `AddClientPeer`). Tras completar (4194304 bytes reales transferidos), `seedAndDrop` con `SeedTime=1s` dispara el target de tiempo (`seed time 1s reached, uploaded 0 B — dropping`) y quita el torrent de `d.active`. Verifica el path real Stats/Drop/ticker, no mocks. Tests puros `TestSeedTargetReached` (9 casos: ratio/tiempo/ninguno/ambos/guarda-tamaño-0) + `TestTorrentDownloader_SeedRatioTime`.

### Hueco medio — HDR→SDR tonemap en transcode  ✅ CERRADO (2026-05-31)
HDR (HDR10/HLG/DV) transcodificado a SDR salía lavado/desaturado (sin tonemap). Ahora `buildHLSFFmpegArgsAt` inserta `zscale=t=linear:npl=100,format=gbrpf32le,zscale=p=bt709,tonemap=tonemap=hable:desat=0,zscale=t=bt709:m=bt709:r=tv` tras el scale y antes de `format=`, cuando `probe.HDR != "" && Transcode.TonemapHDR`.
- **Gate por capacidad**: `FFmpegSupportsZscale(ffmpegPath)` (cacheado, `ffmpeg -filters`) → solo activa si el build trae zscale/zimg. Sin zscale → no se inserta (la fuente sigue reproduciéndose, desaturada — no rompe). `transcoder.go:270` ya advertía que builds sin zimg no pueden tonemapear; el static ffbinaries puede faltarle, pero `/usr/bin/ffmpeg` (distro) y el docker sí lo traen.
- **Filtro CPU válido para todos los encoders**: el decode hwaccel deja los frames en memoria de sistema (no se setea `-hwaccel_output_format`), así que el zscale CPU corre antes del `format=`/`hwupload` (VAAPI) igual que el scale existente.
- **Smoke real**: extraído un frame de un 4K HDR10 (Frankenstein DV+HDR10) con y sin la cadena → ambas válidas (sin error), la tonemapeada con rojo vívido + negros profundos vs la lavada. /critico 1 revisor: cadena correcta, sin bugs bloqueantes; fix aplicado (soltar mutex antes del exec en la detección), HLG/DV-only documentados como aproximación (mejor que el baseline).

### Hueco medio — Auto-resume de descargas tras reinicio  ✅ CERRADO (2026-05-31)
Antes: tras reiniciar el daemon, una descarga en vuelo quedaba abandonada (cola in-memory perdida, el web no re-despacha una tarea "downloading" atascada) hasta reintento manual. Los BYTES ya persistían (mmap + completion DB BoltDB de anacrolix, keyed por info_hash; debrid Range; usenet tracker) — faltaba que el daemon se re-sometiera solo.
- **`agent.ActiveTaskStore`** (`active-tasks.json`, atómico tmp+rename): persiste el payload `agent.Task` re-submittable de descargas en vuelo. Add al arrancar la descarga, Remove en terminal genuino.
- **Manager**: interfaz `taskPersister` (inyectable/testeable) + `SetTaskStore`. `Submit` ahora DEDUPLICA (mismo id del restore + re-despacho web no lanzan 2 goroutines) y persiste descargas (no stream/seed/upgrade-ReplacePath). `recordFinished` borra del store SALVO `shuttingDown` (atomic) → un apagado limpio preserva el entry; terminal genuino (completado/fallo/cancel-usuario) lo borra. ForceStart se limpia en el re-submit (respeta MaxConcurrent).
- **Daemon**: construye el store, `SetTaskStore`, y al arrancar re-somete `Load()` antes del sync loop.
- **/critico**: 1 revisor → **bug CRÍTICO (conf 98)**: el daemon hacía `cancel()` (ctx padre) ANTES de `manager.Shutdown()` → contextos de tarea cancelados antes de marcar `shuttingDown` → recordFinished con shuttingDown=false → borraba el entry → NO resume (guard era código muerto). FIX: `Manager.Shutdown` cancela los contextos él mismo ANTES de `wg.Wait` (con shuttingDown ya puesto) + el daemon llama `Shutdown` antes de `cancel()`. + ForceStart-strip + excluir upgrade. Tests: store round-trip, dedup, persist/remove-terminal, keep-on-shutdown, stream-no-persiste.
- **Smoke**: cubierto por unit tests (incl. shutdown-keeps). El e2e real (descarga → kill daemon → restart → resume) no se ejecutó para no reiniciar el agente dev en uso por el usuario.

### Hueco medio — Gestión de espacio en disco (pre-flight)  ✅ CERRADO (2026-05-31)
Una descarga ya no llena el disco a 0 a mitad (corrompía el fichero parcial).
- **CLI**: `internal/engine/diskspace.go` — `CheckDiskSpace(dir, need, reserve)` usa `agent.DiskInfo` (Statfs/GetDiskFreeSpaceEx, ya abstraído) y devuelve `*InsufficientDiskError` si `free-need < reserve`; best-effort (need≤0 o stat falla → nil, ENOSPC sigue de backstop). Cableado antes de escribir en los 3 downloaders (torrent: DataDir+totalBytes; debrid: outputDir+restantes; usenet: outputDir+totalBytes solo en fresh). Reserva por `SetMinFreeBytes` desde `downloads.min_free_disk_mb` (default 2048 MiB). `manager` falla sin fallback en disco lleno (otra fuente llena el mismo disco). Fix latente: `formatBytes` paniqueaba ≥1PB (array hasta TB) → +PB/EB+clamp.
- **WEB**: `/api/internal/download` rechaza 507 `INSUFFICIENT_DISK` antes de crear la tarea si `diskFreeBytes - sizeBytes < 2 GiB` (reserva = default agente). Solo single-file torrent + agente online (telemetría de disco ya fluía). Saltado: stream, usenet, episodios (sizeBytes=pack completo → falso reject), agente offline. `DownloadButton` muestra estado `diskfull` (i18n 7 locales, namespace torrent). Bajo unarr el endpoint está fuera del allowlist → unarr solo streamea; el pre-flight del agente cubre sus descargas.
- **Tests/smoke**: Go `diskspace_test` (Statfs real vía TempDir: enough/insufficient/reserve/unknown/bad-dir). Web reject no e2e-smokeable en el dev box (es unarr → endpoint 404); verificado por build+typecheck+lógica. /critico 2 revisores → 2 bugs reales (guard sin `health.online`; falso reject en season packs) + 4 clarity.

### Hueco medio — Características del fichero + thumbnails bajo demanda  ✅ CERRADO (2026-05-31)
Panel "ver características del fichero" (ruta + mediainfo completa: codec/HDR/bit-depth/tracks audio+subs/tamaño/duración — ya en DB vía ffprobe, solo faltaba surface) + tira de fotogramas extraídos en vivo por el agente.
- **CLI**: `GET /thumbnail?p=&pos=&w=&t=` en el stream server (ffmpeg `-ss <pos>` antes de `-i`, `-frames:v 1`, MJPEG a stdout). Token scope `thumb:<sha256(path)>` (mismo HMAC que `/stream`/`/hls`; web mintea, agente verifica; vector cross-lang Go↔TS pinneado). Clamp a fichero regular, 404-sin-oracle, timeout 20s. `ffmpegPath` cableado en `daemon.go`. Floor `0.13.0`.
- **WEB**: endpoints bajo `/api/internal/stream/` (permitido en unarr; `/api/internal/library` NO) — `file-details` (mediainfo + URLs de frames vía funnel HTTPS) + `owned-files` (lista mínima por contentId, solo items con ffprobe). Lógica pura testeada en `src/lib/stream/thumbnails.ts`. Modal compartido `FileDetailsModal`/`useFileDetails` con skeleton + carga progresiva ("Generando X/N…") + fallback por frame. Gating `supportsThumbnails`/`THUMBNAIL_MIN_VERSION`.
- **Alcance en ambas marcas**: torrentclaw → acción en los 3 builders de menú de biblioteca (`fileInfoMenuItem` compartido). unarr → `UnarrFileDetailsButton` en `/title/<id>` (la biblioteca unarr son estanterías, no `LibraryPage`). Modal reutiliza labels neutrales (namespace `library`, no `torrent`) → marca limpia.
- **Tests/smoke**: Go (token vector, args, 400/404/503, stub-ffmpeg success) + web (resolveThumbnails, parity, version gate, i18n 7 locales). Smoke real contra biblioteca local 4K (Frankenstein, HEVC DV+HDR10): ffmpeg extrae JPEG válido, modal unarr muestra mediainfo + 5 frames vía funnel. /critico 4 revisores → 5 fixes (clipboard promise, dedup posiciones short-clip, tipos compartidos, guard videoInfo, helper menú).

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
**Estado:** ✅ CERRADO (2a+2b+2c, 2026-05-31).

**CERRADO 2c (2026-05-31):** fallback mid-stream, alcance = **refresh de URL
debrid** (decisión del usuario; el swap cross-source torrent↔debrid se difiere —
caso raro, gran complejidad). La preferencia cache-fast (preferir debrid
cacheado sobre torrent en streaming) ya la daban 2a/2b por orden de resolución.
Los links debrid caducan; una peli larga sobrevive al link → al detectar expiry
(401/403/404/410 en direct-play, o salida de red de ffmpeg en HLS) el agente
re-resuelve (mismo info_hash → link fresco) y reanuda sin reiniciar.
- WEB: endpoint `POST /api/internal/agent/stream-url` (withAgentAuth) →
  re-resuelve + actualiza fila + devuelve URL. Guard: sesión debrid viva
  (`direct_url IS NOT NULL`). 409 sin sesión, 410 si re-resolución falla.
- CLI: `agentClient.RefreshStreamURL`; `debridFileProvider` URL mutable bajo
  mutex + reader refresca en expiry (bounded 1+1) + **coalescing singleflight**
  (N readers del `<video>` → 1 re-resolución); HLS refresca `s.liveURL` (guarded,
  cfg inmutable → race-free con el seek-restart del handler HTTP) antes del
  auto-restart de ffmpeg.
- Validado: reader refresh + coalescing unit-tested (incl. -race); endpoint
  e2e contra AllDebrid real (URL fresca + fila). El swap torrent↔debrid queda
  como mejora futura si aparece demanda.

**CERRADO 2b (2026-05-31):** HLS-desde-URL para contenido debrid no-nativo
(mkv/HEVC/…). ffmpeg lee la URL debrid directa (`-i <url>` + flags de red
`-reconnect*`/`-rw_timeout`) y transcodifica a HLS; el seek reinicia ffmpeg con
`-ss` antes de `-i` (input-seek vía Range). Cache de segmentos por `CacheID`
(info_hash) → replay hace cache-HIT pese a que la URL cambia cada resolución.
Validado e2e contra AllDebrid real: mkv HEVC x265 → h264_nvenc desde la URL →
Chrome reproduce 1080p vía hls.js, subtítulos extraídos del mkv remoto. Bump
CLI 0.11.0→0.12.0 (gate `DEBRID_HLS_MIN_VERSION`). Ficheros: CLI
`engine/hls.go` (SourceURL/CacheID/sourceRef + flags red), `cmd/daemon.go`
(branch 2b + helper `startHLSPlayback`), `engine/hls_cache.go` (`KeyForID`),
`library/mediainfo/ffprobe.go` (no enmascarar errores de URL). WEB
`stream/debrid-stream-source.ts` (playMethod direct|hls por contenedor),
`services/agent-version-compare.ts` (`supportsDebridHls`).
Limitación: solo audio default (raw debrid sin UI de pistas); subs bitmap (PGS)
no soportados (igual que HLS local). Si AllDebrid no marca "ready" al primer
addMagnet → fallback torrent (sin callejón).

**CERRADO 2a (2026-05-31):** debrid como fuente de `/stream` (direct-play),
validado e2e contra AllDebrid real (cuenta hello@torrentclaw.com): play de un
infoHash cacheado mp4 → web resuelve la DirectURL → agente sirve `/stream` por
GETs ranged → Chrome reproduce el mp4 1080p real (incluido seek a offset alto
para el moov de un fichero sin faststart). CLI bump 0.10.0→0.11.0 (binario local,
sin publicar). Fichero clave: `internal/engine/stream_source_debrid.go`.
- CLI: `StreamSession.DirectURL`; `debridFileProvider` (`io.ReadSeekCloser` sobre
  HTTP Range, Seek sin red + GET lazy + reopen-on-seek + HEAD para tamaño +
  nombre derivado de URL para Content-Type correcto); branch en
  `daemon.OnStreamSession` (DirectURL presente → provider en goroutine →
  SetFile → MarkSessionReady), antes de validar filePath y sin ffmpeg.
- WEB: columna `streaming_session.direct_url` (mig 0137) + índice
  `idx_debrid_cache_info_hash` (mig 0138, getHashCacheTier filtra por info_hash);
  helper `resolveDebridStreamSource` (honesty gate: sin fichero local + infoHash
  + agente ≥0.11.0 + `getHashCacheTier`==="verified" + container mp4/m4v +
  audioIndex -1 + !forceTranscode → resuelve DirectURL, playMethod="direct",
  quality "original"); gate de versión `DEBRID_STREAM_MIN_VERSION`/
  `supportsDebridStream`; `getPendingStreamSessions` emite `directUrl` + fallback
  fileName/fileSize vía join a `torrent` (cubre el caso HEAD-falla del provider).
- Player: sin cambios — reusa el path direct-play del hueco #3 (playMethod=direct
  + streamUrls).
- Limitación 2a (honesta): solo contenido debrid mp4/m4v browser-native; mkv/HEVC
  debrid → fallback a torrent hasta 2b (HLS-desde-URL). Si AllDebrid no marca el
  torrent "ready" al primer addMagnet → fallback a torrent (sin callejón).

**Diseño original (2b/2c siguen vigentes):**

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

---

### Hueco #3 — Device-profile + direct-play + ABR
**Estado:** 🔵 EN CURSO (2026-05-31). Análisis cerrado; fase 3a en implementación.

**Problema (confirmado en el análisis):**
- El path browser usa **HLS y SIEMPRE re-encoda**: `buildHLSFFmpegArgsAt`
  (`engine/hls.go`) pone `-c:v libx264|nvenc|…` + cadena de filtros completa
  (scale/format/setparams) + AAC, sin rama de copia. Un mp4 h264/aac 8-bit SDR
  que el navegador reproduciría tal cual se transcodifica entero. Coste de CPU
  puro desperdicio.
- `DecideAction` + `diskFileSource`/`transcodeSource` (`engine/probe.go`,
  `engine/stream_source.go`) **son código muerto**: cero callers en producción,
  solo tests. Distinguen `passthrough/remux/remux-audio/transcode-video` y detectan
  10-bit/HDR — la lógica de decisión ya existe, no está cableada.

**Lo que ya hay y se reaprovecha:**
- El agente ya expone **dos paths** en el StreamServer (puerto 11818):
  - `/stream` → sirve el fichero crudo con `http.ServeContent` (HTTP Range
    completo, sin ffmpeg, ya tokenizado). **Direct-play ya es posible aquí.**
  - `/hls/<id>/…` → transcode HLS.
- El web **construye las URLs** (HLS hoy) desde la info de red del agente
  (`streamPort`, `tailscaleIp`, `lanIp`, `funnelUrl`, `streamSecret`) y **puede
  mintear tokens** (`mintStreamToken`, scope `stream` es constante). O sea: el web
  puede construir la URL `/stream?t=…` de direct-play él mismo.
- `libraryItem` ya guarda del scan: `videoCodec`, `audioCodec`, `bitDepth`, `hdr`,
  `resolution`. Con el contenedor (extensión de `fileName`), el web tiene todo
  para decidir direct-play SIN re-probar.

**Diseño por fases (de menos a más riesgo):**

- **Fase 3a — direct-play passthrough para items de biblioteca.** *El web decide.*
  *Slice acotado, ambos sentidos de version-skew seguros vía gate de versión.*
  1. WEB `decidePlayMethod({videoCodec,audioCodec,bitDepth,hdr,container})` →
     `"direct" | "hls"` (espeja la rama passthrough de Go `DecideAction`: solo
     `mp4/m4v` + `h264` + `aac` + 8-bit + SDR → direct; todo lo demás → hls).
  2. WEB gate: `supportsDirectPlay(agentVersion)` (constante de versión mínima).
     Direct-play solo si el agente la soporta; si no → hls (sin regresión).
  3. WEB sesión: en la rama `libraryItemPublicId`, seleccionar los campos codec;
     calcular `playMethod` (gated); persistirlo en `streamingSession.play_method`
     (migración aditiva, `db:generate`); devolver `playMethod` + `streamUrls`
     (`/stream?t=` minteadas por el web, lan/ts/funnel) en la respuesta.
  4. WEB sync: `getPendingStreamSessions` emite `playMethod` al agente.
  5. CLI: `StreamSession.PlayMethod string`; en `OnStreamSession`, si
     `PlayMethod=="direct"` → `streamSrv.SetFile(NewDiskFileProvider(path))` +
     `MarkSessionReady` (sin ffmpeg). Else → `StartHLSSession` (actual).
  6. WEB player (`HlsStreamPlayer.tsx`): si `data.playMethod==="direct"` → usar
     `data.streamUrls` + attach nativo `<video src>` (mp4 = reproducible en todo
     navegador, sin hls.js). Else → flujo HLS actual.
  - **Limitación honesta:** solo cubre items de biblioteca (escaneados, con
    metadata codec). Raw `infoHash`/`taskId` → hls (sin probe). Cubrir esos
    casos = fase 3a-bis (el agente decide tras probar, reportando playMethod por
    `MarkSessionReady` — requiere extender el payload + SSE + diferir el attach
    del player al evento ready). Diferido por mayor superficie.

- **Fase 3b — remux fMP4 progresivo vía /stream (ENFOQUE ELEGIDO 2026-05-31).**
  Caso `mkv` (u otro contenedor no-mp4) con h264 + aac + 8-bit + SDR: codecs ya
  browser-native, solo el contenedor estorba. `-c copy` evita el re-encode de vídeo.
  Descartado HLS-copy (duraciones de segmento variables vs manifiesto pre-render →
  rompe seek; arreglarlo = probe de keyframes lento o reescribir el núcleo HLS).
  **Enfoque:** ffmpeg `-c copy -movflags +frag_keyframe+empty_moov+default_base_moof
  -f mp4` mkv→fMP4 a fichero temporal **creciente**; servir ese fMP4 por **/stream**
  (mismo path direct-play 3a, attach nativo, sin hls.js, sin manifiesto).
  **Núcleo real (la parte no-trivial):** servir un fichero que **crece** mientras
  ffmpeg escribe. El `/stream` actual usa `http.ServeContent` (asume fichero completo
  y seekable). Hay que:
  - Resucitar/adaptar el `transcodeSource` muerto (`engine/stream_source.go`):
    ffmpeg→tmp creciente, `ReadAt` con bloqueo hasta que los bytes existan
    (`readBlockTimeout`), `EstimatedSize` = bitrate×duración para que la barra del
    player tenga timeline.
  - Un **responder de Range manual** en /stream para fuentes no-finales (en vez de
    `http.ServeContent`): leer `Range`, `ReadAt` la fuente, escribir 206 +
    `Content-Range` con el tamaño estimado. El path mp4-completo (3a) sigue usando
    ServeContent (rápido).
  - Caveat: seek-adelante a zona no-remuxada bloquea hasta que el copy la alcanza
    (copy es I/O-bound, rápido). Seek-atrás (bytes ya en disco) inmediato.
  **Plan de incrementos seguros:**
  - **3b-i (agente, dormido):** `remuxSource` + responder Range para fuentes
    crecientes, gateado tras `PlayMethod=="remux"` (que el web aún no envía) →
    commiteable sin romper nada, con tests.
  - **3b-ii (web+player):** `decidePlayMethod` devuelve `"remux"` para
    contenedor-no-mp4 + h264/aac/8-bit/SDR; player trata `playMethod != "hls"` igual
    que direct (streamUrls + attach nativo). Activa 3b. Mismo gate de versión.
  **Ficheros:** CLI `engine/stream_source.go` (remuxSource), `engine/stream_server.go`
  (range responder + provider creciente), `cmd/daemon.go` (branch `remux`),
  `engine/transcoder.go` (args `-c copy` fMP4). WEB `lib/stream/play-method.ts`
  (+"remux"), `stream/session/route.ts`, `HlsStreamPlayer.tsx` (`!= "hls"`).

  **CERRADO 2026-05-31:**
  - CLI 3b-i (`feat/unarr-agent` 4a12f13): `GrowingSource` + `NewRemuxSource`
    (reusa `transcodeSource`+`ActionRemux`, estimate = tamaño origen para copy);
    `StreamServer.SetGrowingFile` + `serveGrowing` (responder Range manual: 206
    con total estimado en `Content-Range`, body chunked mientras no-final, exact
    `Content-Length` al finalizar, bloqueo vía `ReadAt`); branch `remux` en
    `OnStreamSession`. Tests `parseByteRange`+`serveGrowing` (full/offset/bounded/
    estimate/HEAD/416). build+vet+test verdes.
  - WEB 3b-ii (`feat/unarr-brand` 10b7d602): `decidePlayMethod`→`"remux"` para
    codecs compatibles en contenedor no-nativo; ruta gatea remux como direct
    (versión, metadata, sin downscale, audioIndex -1); player trata `!= "hls"`
    como attach nativo. lint+typecheck+2334 unit OK.
  - **Smoke e2e (browser, mkv h264/aac 1080p):** `playMethod: remux`, `hlsUrls:
    null`; agente `[stream …] remux (copy) → fMP4`; `/stream` HEAD 200 + GET Range
    206 con fMP4 válido (`ftyp iso6 mp41`+`moov`); browser reproduce 1080p nativo,
    duration leída del fMP4, **seek a 2min OK**, **0 reqs `/hls`**. ✓
  - **Bug cazado por el smoke:** la respuesta `created` de la ruta quedó en
    `playMethod === "direct" ? null` (en vez de `!== "hls"`) → devolvía `hlsUrls`
    para remux. Corregido (el player usaba streamUrls igual, pero inconsistente).
  - **Limitación:** seek-adelante a zona aún-no-remuxada bloquea hasta que el copy
    (rápido) la alcanza; seek-atrás inmediato. Audio no-default / subs-bitmap →
    siguen yendo por HLS (gate `audioIndex == -1`).

- **Fase 3c — capability negotiation (device-profile).** El web envía
  `{maxHeight, codecs:[h264,hevc,av1], containers}` (de UA + `canPlayType`).
  `decidePlayMethod` se hace device-aware: p.ej. Safari/AppleTV que reproduce HEVC
  nativo → passthrough HEVC en vez de transcode HEVC→h264. Reemplaza el heurístico
  UA-burdo de `resolveAutoQuality`. Web+CLI.

- **Fase 3d — ABR.** ABR multi-rendition real **DESCARTADA**: N pipelines ffmpeg
  simultáneos = N× CPU para 1 espectador (mata NAS/Pi), y no aplica a los paths
  copy (direct/remux = 1 bitrate). Resuelto como **3d-lite (auto-downshift)**:
  el player ya tenía sondeo de ancho de banda + recomendación + selector manual;
  3d-lite automatiza la bajada — buffering sostenido 10s → siguiente calidad menor
  (nueva sesión a bitrate menor), progresivo hasta 480p. Reusa
  `recommendLowerQuality`/`setQuality`. `setQuality(.., {persist:false})` para no
  pisar la preferencia del usuario por un stall transitorio. **CERRADO (web 8bf8e416)**;
  smoke en Chrome (Slow-3G + seek → consola `auto-downshift 720p → 480p`, nueva
  sesión reproduce). Hallazgo: este Chrome reproduce HLS **nativo** (como Safari);
  hls.js es fallback.

**Ficheros a tocar (3a):** CLI `internal/agent/types.go` (+PlayMethod),
`internal/cmd/daemon.go` (branch SetFile vs HLS). WEB
`src/lib/services/agent-version-compare.ts` (gate), `src/lib/stream/play-method.ts`
(nuevo), `src/lib/stream-token.ts` (scope stream), `src/lib/db/schema.ts` +
migración (`streamingSession.play_method`), `src/app/api/internal/stream/session/route.ts`
(decisión + URLs), `src/lib/services/agent.ts` (`getPendingStreamSessions` emite
playMethod), `src/components/stream/HlsStreamPlayer.tsx` (attach nativo).

**Seguridad de version-skew (3a):**
- Web nuevo + agente viejo: gate `supportsDirectPlay` ve versión vieja → hls. ✓
- Web viejo + agente nuevo: web nunca manda `direct` → agente hls. ✓
- Campo `PlayMethod` desconocido en agente viejo = ignorado por el unmarshal. ✓

**Empezar por 3a** (valor inmediato — el caso primario de unarr es la biblioteca
local escaneada; mp4-h264-aac es común en web-dl/YIFY). 3b/3c/3d como iteraciones.

**Hecho (Fase 3a CERRADA 2026-05-31):**
- CLI (`feat/unarr-agent` c8d7c4b): `StreamSession.PlayMethod`; `OnStreamSession`
  ramifica `direct` → `SetFile(NewDiskFileProvider)` + `MarkSessionReady` (sin
  ffmpeg, antes del check de ffmpeg para funcionar con transcode off). `go build`
  + `vet` + tests verdes.
- WEB (`feat/unarr-brand` 636fbe59): `decidePlayMethod()` (espeja la rama
  passthrough de Go, conservador) + test unitario; gate `supportsDirectPlay`
  (`DIRECT_PLAY_MIN_VERSION = 0.10.0`); decisión en la ruta de sesión (solo
  library item + sin downscale + `audioIndex == -1`); `buildStreamUrls` mintea
  token scope `stream` (paridad Go); `streaming_session.play_method` (migración
  0135) emitido al agente vía `getPendingStreamSessions`; player ramifica a
  `<video src>` nativo. lint + typecheck:all + 2333 unit + build (brand unarr) OK.
- Revisión adversarial (correctness + security/parity, 2 agentes): **0 hallazgos
  bloqueantes**. Token parity y version-skew (ambos sentidos) confirmados.

**Correcciones de la revisión propia (3a):** direct-play exige `audioIndex == -1`
(servir el fichero entero no respeta una pista de audio no-default elegida por el
usuario → esos casos van a HLS con `-map 0:a:N`).

**Smoke e2e (3a) — PASADO 2026-05-31** (agente dev 0.10.0 build local + item de
biblioteca mp4-h264-aac `/mnt/nas/peliculas/.../Tangled.Ever.After...mp4` + browser):
- POST `/api/internal/stream/session` → `playMethod: direct`, `streamUrls` con
  `/stream?t=` (token web scope `stream`), `hlsUrls: null`. ✓
- Agente: `[stream …] direct-play: Tangled…mp4` (SetFile, sin ffmpeg). ✓
- `/stream`: HEAD 200 `video/mp4` `Content-Length 128321419`; GET Range 0-1023 →
  206 + bytes mp4 reales (`ftyp isom…avc1`). **Token web verificado por Go → paridad
  cross-lenguaje confirmada en vivo** (sin token → 404). ✓
- CORS desde origen browser (`localhost:3030`): ACAO correcto, preflight 204. ✓
- Browser: `<video>.currentSrc` = `/stream?t=…` (NO `/hls`), `readyState 4`,
  reproduciéndose, 1920×1080 nativo, **13 reqs `/stream`, 0 `/hls`**, attach
  **nativo** (`[hls] (native) loadedmetadata`, sin hls.js). Telemetría
  metric/progress OK. ✓

**Bug pre-existente encontrado + arreglado durante el smoke** (web 764f5b01): el
allow-list de la marca unarr (`src/lib/branding/routes.ts`) NO incluía
`/api/internal/agent` ni `/api/internal/stream` → en unarr el agente daba 404 al
registrar y el player 404 al crear sesión. **El streaming + agente de unarr estaban
rotos de raíz.** Añadidos al allow-list (superficie del agente/media propio del
usuario, cero superficie torrent).

**Nota de release:** versión bumpeada a **0.10.0** (`version.go`, CLI 944d652) — solo
binario local para el smoke, **sin publicar nada**. `DIRECT_PLAY_MIN_VERSION = 0.10.0`
(web 52d958f0). Al publicar la release real del CLI, debe ser >= 0.10.0.

**Backlog detectado en 3a (baja prioridad):**
- `streaming_session.transport` queda `"hls"` también para sesiones direct
  (el enum `TRANSPORT_VALUES` solo tiene `"hls"`); telemetría imprecisa, no bug.
  Añadir `"direct"` al vocabulario cuando se toque la métrica.
- Modelo single-viewer: dos plays direct simultáneos → el último `SetFile` gana;
  el tab viejo reproduciría contenido nuevo en silencio (HLS al menos 404ea).
- Direct-play no aplica `audioIndex` ni extrae subs a WebVTT (usa pistas
  embebidas vía `<video>` nativo); subs bitmap no se ven. Aceptable en 3a.
- Listener `loadedmetadata {once:true}` del attach nativo no se limpia
  explícitamente en cleanup (idempotente, impacto nulo).

**Fase 3c CERRADA 2026-05-31** (capability-negotiation, alcance ampliado):
- CLI (`feat/unarr-agent` 957d499): `NewRemuxSource` copia el vídeo para cualquier
  codec decodificable: h264, o HEVC/AV1 si el dispositivo lo declara. HEVC se muxea
  con `-tag:v hvc1` (Apple lo exige). Audio no-aac (ac3/eac3/dts) se transcodifica a
  aac copiando el vídeo (`ActionRemuxAudio`) → cubre el muy común **h264+ac3 mkv**.
- WEB (`feat/unarr-brand` b0681d99): player sondea `canPlayType` (`detectDeviceCaps`)
  y envía `{hevc,av1}` en el POST; `decidePlayMethod(p, caps)` device-aware:
  HEVC/AV1 → `remux` solo si el dispositivo decodifica; audio no-aac ya no fuerza
  `hls`. Tests caps actualizados (10).
- **Smoke e2e:** caps gate (sin caps→`hls`, con caps→`remux`); h264+ac3 remux
  reproduce en Chrome (audio transcodeado, vídeo copiado); retag verificado por
  ffprobe (`codec_name=hevc`, `codec_tag_string=hvc1`); **HEVC reproduce en iPhone
  Safari real (Tailscale) — confirmado por el usuario.** ✓
- **Caveat:** playback HEVC en Apple no se puede smokear en este host (Chrome-Linux
  no decodifica HEVC; Mac-mini Safari por SSH bloqueado por TCC: Automation +
  Screen Recording necesitan click GUI). Verificado vía iPhone del usuario.

**Diagnóstico time-to-first-frame (2026-05-31)** (instrumentación en 957d499:
timers `probe`/`spawn`, `first fMP4 bytes after`, `serveGrowing blocked`):
- Agente NO es el cuello: probe 16–98ms, spawn 1–194ms, primer byte fMP4 ~201ms,
  **0 bloqueos** en `serveGrowing` (LAN ni remoto). Remux `-c copy` completo de un
  fichero de ~780MB en ~16s (limitado por lectura NAS).
- `moov` al frente (empty_moov OK) → el player no busca metadata al final.
- Cliente (Chrome/LAN): POST→primer request ~480ms (sobre todo carga de página).
- **1ª reproducción lenta = warm-up de red (Tailscale); 2ª/3ª rápidas** (confirmado
  por el usuario). No es un problema de código.
- Player YA da feedback (fases `loading-meta`/`probing-transport`/`playing` +
  overlay "Preparando…" + spinner de buffering + mensaje stuck >10s). El "sin
  feedback" del test fue por usar URL cruda (sin UI), no el flujo real.
- **Conclusión:** sin optimización de código necesaria. Arranque garantizado-instante
  = **hueco #4 (pre-transcode)**: dejar el remux/encode hecho antes del play.

---

### Hueco #4 — Pre-transcode (transcode-on-download)
**Estado:** 🔵 DISEÑADO (2026-05-31), pendiente de implementar.

**Qué es:** al completar una descarga (o import a biblioteca), procesar en
background para que la reproducción sea **instantánea** sin transcode en vivo.
Es una optimización: si no terminó cuando el usuario da play → fallback al
transcode en vivo (HLS actual). **Nunca bloquea.**

**Sinergia con lo existente (clave — gran parte de la infra ya está):**
- `hls_cache.go`: un encode HLS completo se cachea y el cache-HIT lo sirve
  instantáneo (cero ffmpeg). Pre-transcode = poblar esa cache antes del play.
- `stream-prewarm.ts` + `createPrewarmSession`: ya lanza un encode HLS de la
  siguiente ep en background. Pre-transcode = generalizar prewarm a "cualquier
  download, configurable", + producir también el artefacto direct-play (3a).
- Por tanto el trabajo NUEVO es: (1) disparador on-download-complete, (2)
  superficie de config en web, (3) gobernanza de recursos + cola, (4) decisión
  "qué producir" (remux mp4 para 3a vs HLS cache vs nada si ya es native).

**Opciones a exponer en la web (set propuesto):**

1. **Activación + disparador**
   - Toggle global on/off (default OFF — CPU/disco intensivo).
   - Disparador: al completar descarga / al escanear-importar / manual
     ("optimizar ahora" por item) / programado (ventana horaria).
   - Default recomendado: on-download-complete, pero solo en ventana idle + sin
     stream en vivo activo.

2. **Qué producir (target) — modo Auto recomendado (por probe):**
   - ya browser-native (mp4 h264/aac 8-bit SDR) → **nada** (3a lo sirve crudo).
   - solo contenedor incompatible (mkv h264/aac) → **remux** a mp4 (barato, sin
     re-encode; habilita 3a direct-play). *(necesita 3b para el manifiesto.)*
   - codec incompatible (HEVC/AV1/10-bit/HDR) → **transcode** a H.264 (caro).
   - Modos: solo-remux / remux+transcode / forzar H.264 universal.
   - Formato salida: mp4 direct-play (seek nativo) vs HLS cache (multi-network)
     vs ambos. Recomendado: mp4 si compatible, HLS si requiere transcode.

3. **Calidad**
   - Mantener original (passthrough cuando se pueda) / cap 1080p / ladder ABR
     (480/720/1080/original — encaja con 3d).
   - "Solo transcodear si ayuda" (no tocar lo ya compatible).

4. **Selección / alcance**
   - Todo / solo biblioteca (pelis+series) / solo lo problemático (p.ej. solo
     4K HEVC, dejar h264).
   - Solo watchlist / recién añadido / todo. Reglas por carpeta de biblioteca.

5. **Gobernanza de recursos (lo más importante — es pesado):**
   - Concurrencia (N transcodes paralelos, default 1).
   - HW accel si disponible (nvenc/qsv/vaapi); cap de threads CPU.
   - Ventana horaria (solo idle, p.ej. 02:00–08:00).
   - **Pausar cuando hay stream en vivo** (no pelear por CPU con la reproducción).
   - Prioridad de cola (watchlist primero / más pequeño primero / más nuevo).
   - (Laptops) solo con AC / no en batería.

6. **Disco / retención (liga con el hueco medio de espacio en disco):**
   - Dónde guardar (cache dir) + tamaño máx + evicción LRU (ya parcial en cache).
   - Mantener SIEMPRE el original; el transcode es artefacto adicional.
   - TTL: borrar pre-transcode no visto en N días; pin a visto/favorito.
   - Re-transcodear al cambiar la config de calidad (invalidación).

7. **UX / estado**
   - Cola + progreso por item en la web ("Optimizando para reproducción
     instantánea…"). Badge en library card: "listo para play instantáneo" vs
     "se transcodificará al reproducir". Notificación al terminar (opcional).

8. **Fallback / límites**
   - Si no terminó a tiempo → transcode en vivo (HLS). Nunca bloquea el play.
   - Solo ficheros locales en disco (no debrid/torrent sin bajar).

**MVP recomendado (fase 4a):** toggle on/off + disparador on-download-complete +
modo Auto (remux-si-compatible / transcode-si-no) + concurrencia 1 +
pausar-si-stream-activo + reusar `hls_cache` + badge "listo". El resto (ladder
ABR, ventanas horarias, reglas por carpeta, TTL avanzado, formato mp4 vs HLS
configurable) en fases 4b/4c.

**Dependencias:** el camino mp4/remux depende del hueco #3 (3a ya hecho; 3b para
el remux-a-mp4 con manifiesto correcto). El camino HLS-cache es implementable ya
(reusa cache + prewarm). La gobernanza (pausar-si-stream) necesita señal de
"stream activo" en el daemon (la hay: `streamSrv.HasFile()` + registro HLS).
