package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Client struct {
	HTTP *http.Client
	Base string
}

func NewClient(httpClient *http.Client) *Client {
	return &Client{HTTP: httpClient, Base: "https://graph.microsoft.com/v1.0"}
}

type DriveItem struct {
	ID                   string `json:"id"`
	Name                 string `json:"name"`
	Size                 int64  `json:"size"`
	ETag                 string `json:"eTag"`
	CTag                 string `json:"cTag"`
	LastModifiedDateTime string `json:"lastModifiedDateTime"`
	File *struct {
		MimeType string `json:"mimeType"`
		Hashes   *struct {
			Sha256Hash string `json:"sha256Hash"`
		} `json:"hashes"`
	} `json:"file"`
	Folder *struct {
		ChildCount int `json:"childCount"`
	} `json:"folder"`
	ParentReference *struct {
		Path string `json:"path"`
	} `json:"parentReference"`
	Deleted *struct{} `json:"deleted"`
}

type DeltaResponse struct {
	Value     []DriveItem `json:"value"`
	DeltaLink string      `json:"@odata.deltaLink"`
	NextLink  string      `json:"@odata.nextLink"`
}

// DeltaGoneError is returned when the OneDrive delta link has expired (HTTP 410 Gone).
// The caller must discard the stored delta link and restart from a full sync.
type DeltaGoneError struct{}

func (e *DeltaGoneError) Error() string {
	return "OneDrive delta link expired (410 Gone); resetting to full sync"
}

func (c *Client) RootDelta(ctx context.Context, deltaLink string) (*DeltaResponse, error) {
	var u string
	if deltaLink != "" {
		u = deltaLink
	} else {
		u = c.Base + "/me/drive/root/delta"
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, fmt.Errorf("build delta request: %w", err)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// 410 Gone: delta link has expired; caller must do a full sync
	if resp.StatusCode == 410 {
		return nil, &DeltaGoneError{}
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("delta http %d: %s", resp.StatusCode, string(b))
	}
	var d DeltaResponse
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return nil, err
	}
	return &d, nil
}

func escapePathSegments(rel string) string {
	if rel == "" || rel == "/" {
		return "/"
	}
	parts := strings.Split(rel, "/")
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = url.PathEscape(p)
	}
	out := strings.Join(parts, "/")
	if !strings.HasPrefix(out, "/") {
		out = "/" + out
	}
	return out
}

func (c *Client) GetItemByPath(ctx context.Context, relPath string) (*DriveItem, error) {
	safe := escapePathSegments(relPath)
	u := fmt.Sprintf("%s/me/drive/root:%s", c.Base, safe)
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, fmt.Errorf("build get-item request: %w", err)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("get item by path http %d: %s", resp.StatusCode, string(b))
	}
	var it DriveItem
	if err := json.NewDecoder(resp.Body).Decode(&it); err != nil {
		return nil, err
	}
	return &it, nil
}

func (c *Client) ListChildren(ctx context.Context, relPath string) ([]DriveItem, error) {
	var u string
	if relPath == "" || relPath == "/" {
		u = fmt.Sprintf("%s/me/drive/root/children", c.Base)
	} else {
		safe := escapePathSegments(relPath)
		u = fmt.Sprintf("%s/me/drive/root:%s:/children", c.Base, safe)
	}

	var allItems []DriveItem

	// Follow pagination
	for u != "" {
		req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
		if err != nil {
			return nil, fmt.Errorf("build list-children request: %w", err)
		}
		
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return nil, err
		}
		
		if resp.StatusCode != 200 {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return nil, fmt.Errorf("list children http %d: %s", resp.StatusCode, string(b))
		}
		
		var page struct {
			Value    []DriveItem `json:"value"`
			NextLink string      `json:"@odata.nextLink"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
			resp.Body.Close()
			return nil, err
		}
		resp.Body.Close()
		
		allItems = append(allItems, page.Value...)
		u = page.NextLink
	}

	return allItems, nil
}

func (c *Client) DeleteByPath(ctx context.Context, relPath string) error {
	safe := escapePathSegments(relPath)
	u := fmt.Sprintf("%s/me/drive/root:%s:", c.Base, safe)
	req, err := http.NewRequestWithContext(ctx, "DELETE", u, nil)
	if err != nil {
		return fmt.Errorf("build delete request: %w", err)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 204 || resp.StatusCode == 404 || resp.StatusCode == 200 {
		return nil
	}
	b, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("delete http %d: %s", resp.StatusCode, string(b))
}

func (c *Client) DownloadTo(ctx context.Context, itemID, destPath string) error {
	u := fmt.Sprintf("%s/me/drive/items/%s", c.Base, url.PathEscape(itemID))
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return fmt.Errorf("build item-meta request: %w", err)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("item meta http %d: %s", resp.StatusCode, string(b))
	}
	var meta map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		return err
	}
	dl, _ := meta["@microsoft.graph.downloadUrl"].(string)
	if dl == "" {
		return fmt.Errorf("no @microsoft.graph.downloadUrl on item %s", itemID)
	}

	// Disable timeout for large file downloads; rely on ctx cancellation instead.
	plain := &http.Client{Timeout: 0}
	req2, err := http.NewRequestWithContext(ctx, "GET", dl, nil)
	if err != nil {
		return fmt.Errorf("build download request: %w", err)
	}
	resp2, err := plain.Do(req2)
	if err != nil {
		return err
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		b, _ := io.ReadAll(resp2.Body)
		return fmt.Errorf("download http %d: %s", resp2.StatusCode, string(b))
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return err
	}

	// Atomic write: download to .tmp first, then rename.
	tmpPath := destPath + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		return err
	}
	// Ensure temp file is cleaned up on any error path.
	defer func() {
		f.Close()
		if err != nil {
			os.Remove(tmpPath)
		}
	}()

	written, err := io.Copy(f, resp2.Body)
	if err != nil {
		return err
	}
	// Detect truncated downloads: if the server advertised Content-Length, verify it.
	if cl := resp2.ContentLength; cl > 0 && written != cl {
		err = fmt.Errorf("download truncated for item %s: received %d of %d bytes", itemID, written, cl)
		return err
	}
	// Close explicitly before rename to ensure all bytes are flushed.
	if err = f.Close(); err != nil {
		return err
	}

	// Atomic rename; retries handle Windows file-locking from AV scanners.
	return renameWithRetry(tmpPath, destPath)
}

func renameWithRetry(src, dest string) error {
	// 1. Try direct rename (atomic on POSIX, works on Windows if dest doesn't exist)
	if err := os.Rename(src, dest); err == nil {
		return nil
	}

	// 2. Windows-safe strategy: Rename Dest -> Backup, Src -> Dest, Delete Backup.
	// This prevents data loss if the final rename fails.
	bak := dest + ".old.tmp"
	_ = os.Remove(bak) // cleanup potential leftover

	var lastErr error
	for i := 0; i < 3; i++ {
		// Attempt to move existing file out of the way
		if err := os.Rename(dest, bak); err != nil {
			if os.IsNotExist(err) {
				// Dest disappeared; try moving src -> dest again
				if err := os.Rename(src, dest); err == nil {
					return nil
				}
			}
			lastErr = fmt.Errorf("backup dest failed: %w", err)
			time.Sleep(200 * time.Millisecond)
			continue
		}

		// Move new file to dest
		if err := os.Rename(src, dest); err != nil {
			// CRITICAL: Rollback! Try to restore the backup.
			if rbErr := os.Rename(bak, dest); rbErr != nil {
				// Rollback failed. Original file is at 'bak' — user must recover manually.
				lastErr = fmt.Errorf("replace failed AND rollback failed; original file saved at %q — Error: %v | RollbackErr: %v", bak, err, rbErr)
			} else {
				lastErr = fmt.Errorf("replace dest failed (rollback succeeded): %w", err)
			}
			time.Sleep(500 * time.Millisecond)
			continue
		}

		_ = os.Remove(bak)
		return nil
	}
	return fmt.Errorf("renameWithRetry failed: %w", lastErr)
}

func (c *Client) UploadSmall(ctx context.Context, relPath, localPath, ifMatch string) (*DriveItem, error) {
	safe := escapePathSegments(relPath)
	u := fmt.Sprintf("%s/me/drive/root:%s:/content", c.Base, safe)

	var lastErr error
	// Retry loop for network blips or Windows file locks
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt*500) * time.Millisecond)
		}

		// Re-open file in each attempt to handle Windows file locking/sharing violations
		f, err := os.Open(localPath)
		if err != nil {
			lastErr = err
			continue
		}

		req, err := http.NewRequestWithContext(ctx, "PUT", u, f)
		if err != nil {
			f.Close()
			return nil, err // Fatal: URL construction failed
		}
		req.Header.Set("Content-Type", "application/octet-stream")
		if ifMatch != "" {
			req.Header.Set("If-Match", ifMatch)
		}

		resp, err := c.HTTP.Do(req)
		if err != nil {
			f.Close()
			lastErr = err
			continue
		}

		// 429 Too Many Requests or 5xx Server Errors -> Retry
		// 412 Precondition Failed -> Do NOT retry (it's a logic conflict)
		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			f.Close()
			lastErr = fmt.Errorf("upload http %d: %s", resp.StatusCode, string(b))
			continue
		}

		if resp.StatusCode >= 300 {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			f.Close()
			return nil, fmt.Errorf("upload http %d: %s", resp.StatusCode, string(b))
		}

		var it DriveItem
		if err := json.NewDecoder(resp.Body).Decode(&it); err != nil {
			resp.Body.Close()
			f.Close()
			return nil, err
		}
		resp.Body.Close()
		f.Close()
		return &it, nil
	}
	return nil, fmt.Errorf("upload small failed after retries: %w", lastErr)
}

func (c *Client) UploadLarge(ctx context.Context, relPath, localPath, ifMatch string, chunkMB, parallel int) (*DriveItem, error) {
	if chunkMB <= 0 {
		chunkMB = 8
	}
	if parallel <= 0 {
		parallel = 2
	}
	if parallel > 4 {
		parallel = 4
	}
	safe := escapePathSegments(relPath)

	sessURL, err := c.createUploadSession(ctx, safe, ifMatch)
	if err != nil {
		return nil, err
	}

	stat, err := os.Stat(localPath)
	if err != nil {
		c.cancelUploadSession(sessURL)
		return nil, err
	}
	size := stat.Size()

	// Graph API requires fragment size to be a multiple of 320 KiB (327,680 bytes).
	const graphFragmentSize = 327680
	rawChunk := int64(chunkMB) * 1024 * 1024
	chunk := (rawChunk / graphFragmentSize) * graphFragmentSize
	if chunk == 0 {
		chunk = graphFragmentSize
	}

	type part struct{ Start, End int64 }
	var parts []part
	for s := int64(0); s < size; s += chunk {
		e := s + chunk - 1
		if e >= size {
			e = size - 1
		}
		parts = append(parts, part{Start: s, End: e})
	}

	type result struct {
		Item *DriveItem
		Err  error
	}
	jobs := make(chan part, len(parts))
	resc := make(chan result, len(parts))

	var wg sync.WaitGroup
	worker := func() {
		defer wg.Done()
		for p := range jobs {
			f, openErr := os.Open(localPath)
			if openErr != nil {
				resc <- result{Err: openErr}
				continue
			}

			// chunkErr tracks the outcome of this chunk's attempt; kept separate from
			// inner-scope :=-declared variables to avoid shadowing bugs.
			var (
				resp     *http.Response
				chunkErr error
			)
			for attempt := 0; attempt < 5; attempt++ {
				if _, seekErr := f.Seek(p.Start, io.SeekStart); seekErr != nil {
					chunkErr = fmt.Errorf("seek to byte %d: %w", p.Start, seekErr)
					break
				}
				lim := io.LimitReader(f, p.End-p.Start+1)

				req, reqErr := http.NewRequestWithContext(ctx, "PUT", sessURL, lim)
				if reqErr != nil {
					chunkErr = reqErr
					break // Fatal: URL or context invalid
				}
				req.Header.Set("Content-Length", strconv.FormatInt(p.End-p.Start+1, 10))
				req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", p.Start, p.End, size))

				resp, chunkErr = c.HTTP.Do(req)
				if chunkErr == nil && (resp.StatusCode == 200 || resp.StatusCode == 201 || resp.StatusCode == 202) {
					break
				}

				// Retry on network error or server-side transient failure
				shouldRetry := chunkErr != nil
				if !shouldRetry && resp != nil && (resp.StatusCode == 429 || resp.StatusCode >= 500) {
					shouldRetry = true
				}
				if shouldRetry {
					if resp != nil {
						io.Copy(io.Discard, resp.Body)
						resp.Body.Close()
						resp = nil
					}
					time.Sleep(time.Duration(1<<attempt) * 200 * time.Millisecond)
					continue
				}
				break
			}

			if chunkErr != nil {
				f.Close()
				resc <- result{Err: chunkErr}
				continue
			}
			if resp == nil {
				f.Close()
				resc <- result{Err: fmt.Errorf("upload chunk: no response after retries")}
				continue
			}

			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			f.Close()

			if resp.StatusCode == 200 || resp.StatusCode == 201 {
				var it DriveItem
				if err := json.Unmarshal(body, &it); err == nil && it.ID != "" {
					resc <- result{Item: &it}
					continue
				}
			}
			// 202 Accepted is expected for all intermediate chunks.
			if resp.StatusCode == 202 {
				resc <- result{}
				continue
			}
			resc <- result{Err: fmt.Errorf("upload chunk http %d: %s", resp.StatusCode, string(body))}
		}
	}

	for i := 0; i < parallel; i++ {
		wg.Add(1)
		go worker()
	}
	for _, p := range parts {
		jobs <- p
	}
	close(jobs)
	wg.Wait()
	close(resc)

	var lastItem *DriveItem
	for r := range resc {
		if r.Err != nil {
			// Cancel the upload session so OneDrive doesn't hold quota for an orphaned session.
			go c.cancelUploadSession(sessURL)
			return nil, r.Err
		}
		if r.Item != nil {
			lastItem = r.Item
		}
	}
	if lastItem == nil {
		it, err := c.GetItemByPath(ctx, safe)
		if err == nil {
			return it, nil
		}
		return nil, fmt.Errorf("upload session finished without final item response")
	}
	return lastItem, nil
}

// cancelUploadSession sends a DELETE to the session URL to free OneDrive quota
// for orphaned upload sessions. Called asynchronously on fatal upload failure.
func (c *Client) cancelUploadSession(sessURL string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "DELETE", sessURL, nil)
	if err != nil {
		return
	}
	if resp, err := c.HTTP.Do(req); err == nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

func (c *Client) createUploadSession(ctx context.Context, safePath, ifMatch string) (string, error) {
	u := fmt.Sprintf("%s/me/drive/root:%s:/createUploadSession", c.Base, safePath)
	body := strings.NewReader(`{"item":{"@microsoft.graph.conflictBehavior":"replace"}}`)
	req, err := http.NewRequestWithContext(ctx, "POST", u, body)
	if err != nil {
		return "", fmt.Errorf("build createUploadSession request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("createUploadSession http %d: %s", resp.StatusCode, string(b))
	}
	var out struct {
		UploadURL string `json:"uploadUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.UploadURL == "" {
		return "", fmt.Errorf("empty uploadUrl")
	}
	return out.UploadURL, nil
}
