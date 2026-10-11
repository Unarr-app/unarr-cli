package arr

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// Event types of /api/v3/history?eventType=N (the query takes the numeric enum).
const (
	eventGrabbed        = 1
	eventFolderImported = 3

	historyPageSize = 1000
	fileIDsPerCall  = 100
)

// historyEvents returns the history records of one event type, newest first,
// walking pages until one comes back short or stop(oldest record of the page)
// says the rest is out of range (when stop is non-nil).
func (c *Client) historyEvents(eventType int, stop func(HistoryRecord) bool) ([]HistoryRecord, error) {
	var all []HistoryRecord
	for page := 1; ; page++ {
		path := fmt.Sprintf(
			"/api/v3/history?page=%d&pageSize=%d&sortKey=date&sortDirection=descending&eventType=%d",
			page, historyPageSize, eventType)
		var resp HistoryResponse
		if err := c.get(path, &resp); err != nil {
			return nil, fmt.Errorf("history: %w", err)
		}
		all = append(all, resp.Records...)
		if len(resp.Records) < historyPageSize {
			return all, nil
		}
		if stop != nil && stop(resp.Records[len(resp.Records)-1]) {
			return all, nil
		}
	}
}

// filesPath returns the file resource and its id query parameter for the app.
func filesPath(app string) (resource, idParam string, err error) {
	switch app {
	case "sonarr":
		return "episodefile", "episodeFileIds", nil
	case "radarr":
		return "moviefile", "movieFileIds", nil
	}
	return "", "", fmt.Errorf("%s has no per-file indexer flags", app)
}

// MediaFiles fetches the current state of the given imported files. Files the
// history still names but *arr no longer has (replaced by an upgrade, deleted)
// are left out: they are not an error, there is simply nothing to flag.
//
// Sonarr/Radarr answer 500 to the WHOLE batch GET when a single id does not
// exist, and a real history is full of such ids. A failed batch is therefore
// split in halves until the orphans are isolated, and a single id is resolved
// with GET /{resource}/{id}, which cleanly says 404 for an orphan.
func (c *Client) MediaFiles(app string, ids []int) ([]MediaFile, error) {
	resource, idParam, err := filesPath(app)
	if err != nil {
		return nil, err
	}
	var out []MediaFile
	for start := 0; start < len(ids); start += fileIDsPerCall {
		end := min(start+fileIDsPerCall, len(ids))
		batch, err := c.mediaFilesBatch(resource, idParam, ids[start:end])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", resource, err)
		}
		out = append(out, batch...)
	}
	return out, nil
}

func (c *Client) mediaFilesBatch(resource, idParam string, ids []int) ([]MediaFile, error) {
	q := url.Values{}
	for _, id := range ids {
		q.Add(idParam, fmt.Sprint(id))
	}
	var batch []MediaFile
	err := c.get("/api/v3/"+resource+"?"+q.Encode(), &batch)
	if err == nil {
		return batch, nil
	}
	var he *httpStatusError
	if !errors.As(err, &he) || he.Status != http.StatusInternalServerError {
		return nil, err // auth, network, proxy HTML…: a real failure
	}
	if len(ids) == 1 {
		return c.mediaFileOne(resource, ids[0])
	}
	half := len(ids) / 2
	a, err := c.mediaFilesBatch(resource, idParam, ids[:half])
	if err != nil {
		return nil, err
	}
	b, err := c.mediaFilesBatch(resource, idParam, ids[half:])
	if err != nil {
		return nil, err
	}
	return append(a, b...), nil
}

// mediaFileOne returns the file, or nothing when *arr says 404 (orphan id).
func (c *Client) mediaFileOne(resource string, id int) ([]MediaFile, error) {
	var f MediaFile
	err := c.get(fmt.Sprintf("/api/v3/%s/%d", resource, id), &f)
	var he *httpStatusError
	if errors.As(err, &he) && he.Status == http.StatusNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return []MediaFile{f}, nil
}

// SetIndexerFlags rewrites only the indexerFlags of the given files
// (PUT /{episodefile|moviefile}/bulk). It never downloads or renames anything.
func (c *Client) SetIndexerFlags(app string, changes []FlagChange) error {
	resource, _, err := filesPath(app)
	if err != nil {
		return err
	}
	if err := c.do("PUT", "/api/v3/"+resource+"/bulk", changes, nil); err != nil {
		return fmt.Errorf("%s bulk update: %w", resource, err)
	}
	return nil
}
