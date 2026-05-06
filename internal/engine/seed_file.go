package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

// SeedFile builds a single-file torrent from an arbitrary on-disk file
// and adds it to an existing torrent client so the WebRTC peer wire
// (already configured on the client) can serve the file to a browser
// that knows the resulting info-hash.
//
// Returns the generated info-hash. The torrent is left attached to the
// client — caller is responsible for keeping it alive while a browser
// is watching. Drop it via Client.RemoveTorrent / Torrent.Drop when
// idle to free resources.
//
// Behaviour notes:
//   - The file must already exist; no download is attempted.
//   - Piece length follows the libtorrent ladder (16 KiB → 4 MiB).
//   - The torrent is "complete" from the agent's POV — it has every
//     piece — so the upload-only flow kicks in immediately.
//   - WebRTC peer behaviour comes from the client config the caller
//     constructed; SeedFile does not toggle DisableWebtorrent itself.
//     If the operator's [downloads.webrtc].enabled = false, the file
//     is still added but no browser will discover it via WSS tracker.
func SeedFile(client *torrent.Client, filePath string, trackerURLs []string) (metainfo.Hash, error) {
	if client == nil {
		return metainfo.Hash{}, errors.New("seed_file: torrent client is nil")
	}
	if filePath == "" {
		return metainfo.Hash{}, errors.New("seed_file: filePath is empty")
	}

	abs, err := filepath.Abs(filePath)
	if err != nil {
		return metainfo.Hash{}, fmt.Errorf("seed_file: resolve path: %w", err)
	}
	st, err := os.Stat(abs)
	if err != nil {
		return metainfo.Hash{}, fmt.Errorf("seed_file: stat: %w", err)
	}
	if st.IsDir() {
		return metainfo.Hash{}, fmt.Errorf("seed_file: only single files are supported, %s is a directory", abs)
	}

	info := metainfo.Info{
		PieceLength: chooseSeedPieceLength(st.Size()),
		Name:        filepath.Base(abs),
	}
	if err := info.BuildFromFilePath(abs); err != nil {
		return metainfo.Hash{}, fmt.Errorf("seed_file: build info: %w", err)
	}
	infoBytes, err := bencode.Marshal(info)
	if err != nil {
		return metainfo.Hash{}, fmt.Errorf("seed_file: marshal info: %w", err)
	}

	mi := &metainfo.MetaInfo{
		InfoBytes:    infoBytes,
		AnnounceList: makeAnnounceList(trackerURLs),
		CreatedBy:    "unarr-seed-file",
		CreationDate: time.Now().Unix(),
	}
	ih := mi.HashInfoBytes()

	t, err := client.AddTorrent(mi)
	if err != nil {
		return metainfo.Hash{}, fmt.Errorf("seed_file: add torrent: %w", err)
	}
	// Mark every piece as needed so the client treats us as a complete
	// seeder right away — anacrolix's verifier will hash the file
	// asynchronously and flip pieces to "have" as it goes.
	t.DownloadAll()

	return ih, nil
}

// makeAnnounceList shapes the tracker URL slice into the bencoded
// AnnounceList format anacrolix expects.
func makeAnnounceList(urls []string) metainfo.AnnounceList {
	if len(urls) == 0 {
		return nil
	}
	tier := make([]string, 0, len(urls))
	for _, u := range urls {
		if u == "" {
			continue
		}
		tier = append(tier, u)
	}
	if len(tier) == 0 {
		return nil
	}
	return metainfo.AnnounceList{tier}
}

// chooseSeedPieceLength picks the piece size for a single-file torrent
// based on the libtorrent / qBittorrent ladder. Mirrored from the
// wstracker-probe seeder so generated torrents are interoperable.
func chooseSeedPieceLength(size int64) int64 {
	switch {
	case size < 4*1024*1024:
		return 16 * 1024
	case size < 64*1024*1024:
		return 64 * 1024
	case size < 512*1024*1024:
		return 256 * 1024
	case size < 4*1024*1024*1024:
		return 1024 * 1024
	default:
		return 4 * 1024 * 1024
	}
}

// SeedFileOnDownloader is a convenience wrapper that pulls the
// underlying anacrolix client out of a TorrentDownloader and forwards
// to SeedFile. trackerURLs default to the downloader's WebRTC
// trackers when nil/empty.
func SeedFileOnDownloader(d *TorrentDownloader, filePath string) (metainfo.Hash, error) {
	if d == nil {
		return metainfo.Hash{}, errors.New("seed_file: downloader is nil")
	}
	trackers := d.cfg.WebRTCTrackers
	if !d.cfg.WebRTCEnabled {
		// We could still build the torrent, but no browser would find
		// it via the WSS tracker — bail loud so the operator notices.
		return metainfo.Hash{}, errors.New("seed_file: WebRTC peer disabled in config; set [downloads.webrtc].enabled = true to use this feature")
	}
	return SeedFile(d.client, filePath, trackers)
}
