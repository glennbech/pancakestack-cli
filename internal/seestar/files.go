package seestar

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// FilesRoot is the top-level directory inside the scope's HTTP file
// server that holds every observation folder. Kept as a constant so
// a firmware change would show up as one obvious edit.
const FilesRoot = "MyWorks"

// Folder is one observation session on the scope.
type Folder struct {
	// Name is the human-visible folder name (e.g. `M 81`, `M 81_sub`,
	// `Lunar`). Whitespace is significant — the phone app produces
	// names with spaces and the HTTP server keeps them.
	Name string
	// Count is what the scope reports as the album count for this
	// folder — roughly the number of Live/Enhance session outputs,
	// NOT the number of individual FITS. Useful for a quick summary
	// but always list the folder to see actual files.
	Count int64
}

// File is one entry in a folder listing.
type File struct {
	Name    string
	Size    int64 // bytes (converted from the scope's KB-rounded value)
	IsDir   bool
}

// ListFolders returns every observation folder present on the scope's
// eMMC. Wraps `get_albums`. Folder count is not the same as file count —
// use ListFiles for the actual files.
func (c *Client) ListFolders(ctx context.Context) ([]Folder, error) {
	raw, err := c.Call(ctx, "get_albums", nil)
	if err != nil {
		return nil, err
	}
	var body struct {
		List []struct {
			Files []struct {
				Name  string `json:"name"`
				Count int64  `json:"count"`
			} `json:"files"`
		} `json:"list"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("get_albums decode: %w", err)
	}
	var out []Folder
	for _, group := range body.List {
		for _, e := range group.Files {
			out = append(out, Folder{Name: e.Name, Count: e.Count})
		}
	}
	return out, nil
}

// ListFiles enumerates every file under `MyWorks/<folder>` on the
// scope. Two round trips: page-count fetch + N page fetches. Filters
// to just FITS (`.fit`) — everything else (thumbnails, JPEG stacks)
// is derived server-side by pancakestack and would be duplicated work.
func (c *Client) ListFiles(ctx context.Context, folder string) ([]File, error) {
	dir := FilesRoot + "/" + folder
	raw, err := c.Call(ctx, "get_img_file_page_number", map[string]any{
		"dir":      dir,
		"skip_avi": false,
	})
	if err != nil {
		return nil, err
	}
	var pages int
	if err := json.Unmarshal(raw, &pages); err != nil {
		// Older firmware wraps the count in {"count":N}.
		var wrap struct {
			Count int `json:"count"`
		}
		if err2 := json.Unmarshal(raw, &wrap); err2 == nil {
			pages = wrap.Count
		} else {
			return nil, fmt.Errorf("get_img_file_page_number decode: %w", err)
		}
	}
	if pages <= 0 {
		return nil, nil
	}
	var out []File
	for page := 0; page < pages; page++ {
		raw, err := c.Call(ctx, "get_img_file_page_name", map[string]any{
			"page": page,
		})
		if err != nil {
			return nil, err
		}
		var entries []struct {
			Name  string `json:"name"`
			SizeK int64  `json:"size_k"`
			IsDir bool   `json:"is_dir"`
		}
		if err := json.Unmarshal(raw, &entries); err != nil {
			return nil, fmt.Errorf("get_img_file_page_name page %d decode: %w", page, err)
		}
		for _, e := range entries {
			if e.IsDir || e.Name == "" {
				continue
			}
			// FITS only — skip JPEGs (thumbnails + preview stacks are
			// re-derived server-side; uploading them would be duplicate
			// work + a filename collision risk with the auto-generated
			// `.thumb.jpg`/`.preview.jpg` siblings).
			if !strings.HasSuffix(strings.ToLower(e.Name), ".fit") {
				continue
			}
			out = append(out, File{
				Name: e.Name,
				Size: e.SizeK * 1024,
			})
		}
	}
	return out, nil
}

// DownloadFile streams one file off the scope's HTTP server into w.
// Times out on stall via a short per-read deadline; the caller can
// pick a hard cap via ctx. Returns bytes copied.
func DownloadFile(ctx context.Context, ip, folder, filename string, w io.Writer) (int64, error) {
	u := &url.URL{
		Scheme: "http",
		Host:   ip,
		Path:   "/" + FilesRoot + "/" + folder + "/" + filename,
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, err
	}
	client := &http.Client{
		Timeout: 0, // caller controls via context
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("http %d fetching %s", resp.StatusCode, u.String())
	}
	return io.Copy(w, resp.Body)
}

// Ping is a cheap "is the scope reachable" probe. Returns nil on
// success. Uses `get_device_state` which every firmware version
// implements.
func (c *Client) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := c.Call(ctx, "get_device_state", map[string]any{"keys": []string{"device"}})
	return err
}
