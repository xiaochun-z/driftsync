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
	File                 *struct {
		MimeType string `json:"mimeType"`
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

func (c *Client) RootDelta(ctx context.Context, deltaLink string) (*DeltaResponse, error) {
	var u string
	if deltaLink != "" {
		u = deltaLink
	} else {
		u = c.Base + "/me/drive/root/delta"
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
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
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
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

func (c *Client) DeleteByPath(ctx context.Context, relPath string) error {
	safe := escapePathSegments(relPath)
	u := fmt.Sprintf("%s/me/drive/root:%s:", c.Base, safe)
	req, _ := http.NewRequestWithContext(ctx, "DELETE", u, nil)
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
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
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

	plain := &http.Client{Timeout: 120 * time.Second} // SECURITY: add timeout to avoid hanging requests
	req2, _ := http.NewRequestWithContext(ctx, "GET", dl, nil)
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
	f, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp2.Body)
	return err
}

func (c *Client) UploadSmall(ctx context.Context, relPath, localPath, ifMatch string) (*DriveItem, error) {
	safe := escapePathSegments(relPath)
	u := fmt.Sprintf("%s/me/drive/root:%s:/content", c.Base, safe)
	f, err := os.Open(localPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	req, _ := http.NewRequestWithContext(ctx, "PUT", u, f)
	req.Header.Set("Content-Type", "application/octet-stream")
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("upload http %d: %s", resp.StatusCode, string(b))
	}
	var it DriveItem
	if err := json.NewDecoder(resp.Body).Decode(&it); err != nil {
		return nil, err
	}
	return &it, nil
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
		return nil, err
	}
	size := stat.Size()
	chunk := int64(chunkMB) * 1024 * 1024

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
			f, err := os.Open(localPath)
			if err != nil {
				resc <- result{Err: err}
				continue
			}
			var resp *http.Response
			for attempt := 0; attempt < 5; attempt++ {
				// FIX: Seek and recreate request body inside loop to ensure retries carry data
				if _, err := f.Seek(p.Start, io.SeekStart); err != nil {
					err = fmt.Errorf("seek error: %w", err)
					break
				}
				lim := io.LimitReader(f, p.End-p.Start+1)

				req, _ := http.NewRequestWithContext(ctx, "PUT", sessURL, lim)
				req.Header.Set("Content-Length", strconv.FormatInt(p.End-p.Start+1, 10))
				req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", p.Start, p.End, size))

				resp, err = c.HTTP.Do(req)
				if err == nil && (resp.StatusCode == 200 || resp.StatusCode == 201 || resp.StatusCode == 202) {
					break
				}

				// Retry on network error OR server error (429/5xx)
				shouldRetry := err != nil
				if !shouldRetry && resp != nil && (resp.StatusCode == 429 || resp.StatusCode >= 500) {
					shouldRetry = true
				}

				if shouldRetry {
					// Clean up before retry
					if resp != nil {
						io.Copy(io.Discard, resp.Body)
						resp.Body.Close()
					}
					time.Sleep(time.Duration(1<<attempt) * 200 * time.Millisecond)
					continue
				}
				break
			}
			if err != nil {
				f.Close()
				resc <- result{Err: err}
				continue
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			f.Close()

			if resp.StatusCode == 200 || resp.StatusCode == 201 {
				var it DriveItem
				if err := json.Unmarshal(body, &it); err == nil && it.ID != "" {
					resc <- result{Item: &it, Err: nil}
					continue
				}
			}
			// Return actual error if upload failed (e.g. 400, 401, 403)
			resc <- result{Item: nil, Err: fmt.Errorf("upload chunk http %d: %s", resp.StatusCode, string(body))}
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

func (c *Client) createUploadSession(ctx context.Context, safePath, ifMatch string) (string, error) {
	u := fmt.Sprintf("%s/me/drive/root:%s:/createUploadSession", c.Base, safePath)
	body := strings.NewReader(`{"item":{"@microsoft.graph.conflictBehavior":"replace"}}`)
	req, _ := http.NewRequestWithContext(ctx, "POST", u, body)
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
