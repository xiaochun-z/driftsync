package graph

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Client struct {
	HTTP *http.Client
	// UploadHTTP must not inject Graph authorization into preauthenticated URLs.
	UploadHTTP *http.Client
	Base       string
}

func NewClient(httpClient *http.Client) *Client {
	return &Client{HTTP: httpClient, Base: "https://graph.microsoft.com/v1.0"}
}

type DriveItemHashes struct {
	Sha256Hash string `json:"sha256Hash"`
}

type DriveItemFile struct {
	MimeType string           `json:"mimeType"`
	Hashes   *DriveItemHashes `json:"hashes"`
}

type DriveItemFolder struct {
	ChildCount int `json:"childCount"`
}

type DriveItemParentReference struct {
	Path string `json:"path"`
	ID   string `json:"id"`
}

type DriveItem struct {
	ID                   string                    `json:"id"`
	Name                 string                    `json:"name"`
	Size                 int64                     `json:"size"`
	ETag                 string                    `json:"eTag"`
	CTag                 string                    `json:"cTag"`
	LastModifiedDateTime string                    `json:"lastModifiedDateTime"`
	File                 *DriveItemFile            `json:"file"`
	Folder               *DriveItemFolder          `json:"folder"`
	ParentReference      *DriveItemParentReference `json:"parentReference"`
	Deleted              map[string]any            `json:"deleted"`
	Root                 map[string]any            `json:"root"`
}

func (c *Client) GetItemByID(ctx context.Context, id string) (*DriveItem, error) {
	u := c.Base + "/me/drive/items/" + url.PathEscape(id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get item %s http %d", id, resp.StatusCode)
	}
	var item DriveItem
	if err := json.NewDecoder(resp.Body).Decode(&item); err != nil {
		return nil, err
	}
	if item.ID != id {
		return nil, fmt.Errorf("item ID mismatch: requested %s, received %s", id, item.ID)
	}
	return &item, nil
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
	return c.DownloadToChecked(ctx, itemID, destPath, "", nil)
}

// DownloadToChecked verifies bytes and the caller's original local state before
// replacing the destination. Failed verification never removes the old file.
func (c *Client) DownloadToChecked(ctx context.Context, itemID, destPath, expectedSHA string, beforeReplace func() error) error {
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

	// Use an exclusively-created name: destPath+".tmp" may be a user file.
	f, err := os.CreateTemp(filepath.Dir(destPath), ".driftsync-tmp-*")
	if err != nil {
		return err
	}
	tmpPath := f.Name()
	defer func() {
		f.Close()
		os.Remove(tmpPath)
	}()

	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(f, hash), resp2.Body)
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
	if expectedSHA != "" && !strings.EqualFold(fmt.Sprintf("%x", hash.Sum(nil)), expectedSHA) {
		return fmt.Errorf("integrity check failed for item %s: sha256 mismatch", itemID)
	}
	if beforeReplace != nil {
		if err := beforeReplace(); err != nil {
			return err
		}
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
	backup, err := os.CreateTemp(filepath.Dir(dest), ".driftsync-tmp-*")
	if err != nil {
		return err
	}
	bak := backup.Name()
	if err := backup.Close(); err != nil {
		os.Remove(bak)
		return err
	}
	if err := os.Remove(bak); err != nil {
		return err
	}

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
				return fmt.Errorf("replace failed AND rollback failed; original file saved at %q — Error: %v | RollbackErr: %v", bak, err, rbErr)
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
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if attempt > 0 {
			if err := waitRetry(ctx, time.Duration(attempt*500)*time.Millisecond); err != nil {
				return nil, err
			}
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

// UploadLarge preserves the parallel argument for config/API compatibility, but
// Graph requires fragments within one session to be sent sequentially. Different
// files can still upload concurrently through the syncer's upload workers.
func (c *Client) UploadLarge(ctx context.Context, relPath, localPath, ifMatch string, chunkMB, _ int) (*DriveItem, error) {
	f, err := os.Open(localPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := stat.Size()
	if size == 0 {
		return c.UploadSmall(ctx, relPath, localPath, ifMatch)
	}
	if chunkMB <= 0 {
		chunkMB = 8
	}
	// Each request must be strictly smaller than 60 MiB and aligned to 320 KiB.
	const fragment = int64(327680)
	const maxChunk = ((60*1024*1024 - 1) / fragment) * fragment
	chunk := maxChunk
	if chunkMB < 60 {
		chunk = (int64(chunkMB) * 1024 * 1024 / fragment) * fragment
	}
	if chunk < fragment {
		chunk = fragment
	}
	sessURL, err := c.createUploadSession(ctx, escapePathSegments(relPath), ifMatch)
	if err != nil {
		return nil, err
	}
	completed := false
	defer func() {
		if !completed {
			go c.cancelUploadSession(sessURL)
		}
	}()
	for start := int64(0); start < size; {
		length := min(chunk, size-start)
		var body []byte
		var status int
		for attempt := 0; ; attempt++ {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodPut, sessURL, io.NewSectionReader(f, start, length))
			if err != nil {
				return nil, err
			}
			req.ContentLength = length
			req.Header.Set("Content-Type", "application/octet-stream")
			req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, start+length-1, size))
			// uploadUrl is preauthenticated: never send the Graph Bearer token.
			resp, requestErr := c.uploadClient().Do(req)
			status = 0
			if resp != nil {
				status = resp.StatusCode
				body, err = io.ReadAll(resp.Body)
				resp.Body.Close()
				if requestErr == nil {
					requestErr = err
				}
			}
			retry := requestErr != nil || status == 429 || status >= 500
			if retry && attempt < 4 {
				if err := waitRetry(ctx, time.Duration(1<<attempt)*200*time.Millisecond); err != nil {
					return nil, err
				}
				continue
			}
			if requestErr != nil {
				return nil, requestErr
			}
			break
		}
		start += length
		if status == http.StatusAccepted {
			if start == size {
				return nil, fmt.Errorf("upload session did not confirm completion")
			}
			continue
		}
		if status != http.StatusOK && status != http.StatusCreated {
			return nil, fmt.Errorf("upload chunk http %d: %s", status, body)
		}
		if start != size {
			return nil, fmt.Errorf("upload session completed before all bytes were sent")
		}
		var item DriveItem
		if err := json.Unmarshal(body, &item); err != nil {
			return nil, err
		}
		if item.ID == "" {
			return nil, fmt.Errorf("upload completed without item ID")
		}
		completed = true
		return &item, nil
	}
	return nil, fmt.Errorf("upload session finished without final item response")
}

func waitRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *Client) uploadClient() *http.Client {
	if c.UploadHTTP != nil {
		return c.UploadHTTP
	}
	return http.DefaultClient
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
	if resp, err := c.uploadClient().Do(req); err == nil {
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
