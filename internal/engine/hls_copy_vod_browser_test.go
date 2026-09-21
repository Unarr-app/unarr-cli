package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Opt-in interactive acceptance harness using the REAL session and HTTP
// handlers. Both local and HTTP-Range sources are exercised. The browser sees
// metrics in the DOM, without automation injecting code into the production UI.
func TestCopyVODBrowser(t *testing.T) {
	src, js := os.Getenv("UNARR_BROWSER_MEDIA"), os.Getenv("UNARR_HLS_JS")
	remoteURL := os.Getenv("UNARR_BROWSER_REMOTE_URL")
	if (src == "" && remoteURL == "") || js == "" {
		t.Skip("set UNARR_BROWSER_MEDIA or UNARR_BROWSER_REMOTE_URL, and UNARR_HLS_JS")
	}
	if _, err := os.Stat(js); err != nil {
		t.Fatal(err)
	}
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.ServeFile(w, r, src) }))
	defer source.Close()
	ss := NewStreamServer(0, 1)
	ss.SetRequireStreamToken(false)
	for _, remote := range []bool{false, true} {
		if !remote && src == "" {
			continue
		}
		id := "local"
		cfg := HLSSessionConfig{SourcePath: src, VideoCopy: true, AudioIndex: -1, Transcode: TranscodeRuntime{FFmpegPath: "ffmpeg", FFprobePath: "ffprobe"}}
		if remote {
			id = "remote"
			cfg.SourcePath = ""
			cfg.SourceURL = source.URL + "/media.mkv"
			if remoteURL != "" {
				cfg.SourceURL = remoteURL
			}
		}
		cfg.SessionID = "hls-acceptance-" + id
		probe, err := ProbeFile(context.Background(), "ffprobe", cfg.sourceRef())
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "video"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(dir, "subs"), 0o755); err != nil {
			t.Fatal(err)
		}
		// By default this harness tests A/V only. UNARR_BROWSER_SUBS=1 keeps the
		// whole-file subtitle pass running, as production does for a remote source:
		// on a bandwidth-bound link that pass is what used to starve playback.
		probeCopy := *probe
		if os.Getenv("UNARR_BROWSER_SUBS") != "1" {
			probeCopy.SubtitleTracks = nil
		}
		s := &HLSSession{cfg: cfg, probe: &probeCopy, tmpDir: dir, durationSec: probe.DurationSec, readyCh: make(chan struct{})}
		if !startCopyVOD(context.Background(), s) {
			t.Fatal("exact COPY-VOD unavailable")
		}
		defer s.Close()
		ss.HLS().RegisterKeep(s)
	}
	stop := make(chan struct{})
	var once sync.Once
	mux := http.NewServeMux()
	mux.HandleFunc("/hls/", ss.hlsHandler)
	mux.HandleFunc("/hls.js", func(w http.ResponseWriter, r *http.Request) { http.ServeFile(w, r, js) })
	mux.HandleFunc("/stop", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			once.Do(func() { close(stop) })
		}
		w.WriteHeader(204)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		page := copyVODBrowserPage
		if src == "" {
			page = strings.ReplaceAll(page, "load('local');", "load('remote');")
			page = strings.ReplaceAll(page, `<button id="local">`, `<button id="local" disabled>`)
		}
		fmt.Fprint(w, page)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	t.Logf("BROWSER ACCEPTANCE %s", srv.URL)
	select {
	case <-stop:
	case <-time.After(30 * time.Minute):
		t.Fatal("browser acceptance timed out")
	}
}

const copyVODBrowserPage = `<!doctype html><html lang="es"><meta charset="utf-8"><title>Validación HLS exacto</title>
<style>body{background:#111;color:#eee;font:16px system-ui;margin:24px}video{width:min(100%,1000px);display:block}button,input{margin:8px;padding:10px}pre{white-space:pre-wrap}</style>
<h1>Validación HLS exacto · Murderbot</h1><p>Motor real del worktree. Vídeo original, audio AAC.</p>
<button id="local">Archivo local</button><button id="remote">HTTP Range</button>
<video id="video" controls playsinline></video><button id="play">Reproducir</button><button id="pause">Pausa</button>
<label>Posición en segundos <input id="seek" type="number" value="900"></label><button id="jump">Saltar</button>
<button id="back">Volver a 0</button><button id="fast">Velocidad 4x</button><button id="normal">Velocidad 1x</button>
<button id="finish">Terminar prueba</button><pre id="metrics"></pre><pre id="events"></pre><script src="/hls.js"></script><script>
const v=document.querySelector('video'), m=document.querySelector('#metrics'), e=document.querySelector('#events');
let h, data, last=0, seeking=false, started=0, seekStarted=0, records=[];
function log(text){records.push(text);e.textContent=records.slice(-30).join('\n')}
function load(mode){if(h)h.destroy();data={mode,hls:Hls.version,firstFrameMs:null,seeks:[],errors:[],stalls:0,backwards:0,fragments:0,maxAVDelta:0};last=0;started=performance.now();records=[];
h=new Hls({maxBufferLength:30,backBufferLength:30});h.loadSource('/hls/hls-acceptance-'+mode+'/master.m3u8');h.attachMedia(v);
h.on(Hls.Events.ERROR,(_,d)=>{data.errors.push({type:d.type,detail:d.details,fatal:d.fatal});log('ERROR '+d.details)});
h.on(Hls.Events.FRAG_BUFFERED,(_,d)=>{data.fragments++;const a=d.frag.elementaryStreams.audio, b=d.frag.elementaryStreams.video;if(a&&b){const delta=Math.abs(a.startPTS-b.startPTS);data.maxAVDelta=Math.max(data.maxAVDelta,delta)}log('fragmento '+d.frag.sn+' inicio '+d.frag.start.toFixed(3))});
h.on(Hls.Events.MANIFEST_PARSED,()=>v.play().catch(()=>log('Pulsa Reproducir')))}
v.addEventListener('playing',()=>{if(data.firstFrameMs===null)data.firstFrameMs=Math.round(performance.now()-started);if(seekStarted){data.seeks.push({time:v.currentTime,ms:Math.round(performance.now()-seekStarted)});seekStarted=0}});
v.addEventListener('seeking',()=>{seeking=true});v.addEventListener('seeked',()=>{last=v.currentTime;seeking=false});
v.addEventListener('waiting',()=>{if(data&&data.firstFrameMs!==null&&!seeking)data.stalls++});
v.addEventListener('timeupdate',()=>{if(!seeking&&v.currentTime<last-.1)data.backwards++;last=v.currentTime});
setInterval(()=>{if(!data)return;const ranges=Array.from({length:v.buffered.length},(_,i)=>[v.buffered.start(i),v.buffered.end(i)]);m.textContent=JSON.stringify({...data,time:v.currentTime,duration:v.duration,paused:v.paused,ready:v.readyState,buffered:ranges,quality:v.getVideoPlaybackQuality()},null,2)},500);
document.querySelector('#local').onclick=()=>load('local');document.querySelector('#remote').onclick=()=>load('remote');
document.querySelector('#play').onclick=()=>v.play();document.querySelector('#pause').onclick=()=>v.pause();
document.querySelector('#jump').onclick=()=>{seekStarted=performance.now();v.currentTime=Number(document.querySelector('#seek').value);v.play()};
document.querySelector('#back').onclick=()=>{seekStarted=performance.now();v.currentTime=0;v.play()};
document.querySelector('#fast').onclick=()=>v.playbackRate=4;document.querySelector('#normal').onclick=()=>v.playbackRate=1;
document.querySelector('#finish').onclick=()=>{v.pause();h.destroy();fetch('/stop',{method:'POST'})};
load('local');</script></html>`
