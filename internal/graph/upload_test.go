package graph

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type bearerTransport struct{ base http.RoundTripper }

func (rt bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer test-token")
	return rt.base.RoundTrip(clone)
}

func TestLargeUploadSendsSequentialSizedUnauthenticatedChunks(t *testing.T) {
	var base string
	var mu sync.Mutex
	var offset int64
	var chunks int
	const size = 2500000
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "createUploadSession") {
			if r.Header.Get("Authorization") == "" {
				t.Error("session creation lost Graph authorization")
			}
			fmt.Fprintf(w, `{"uploadUrl":%q}`, base+"/upload")
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != "" {
			t.Error("Bearer leaked to upload URL")
		}
		if r.ContentLength <= 0 || r.ContentLength >= 60*1024*1024 {
			t.Errorf("invalid content length %d", r.ContentLength)
		}
		var start, end, total int64
		if _, err := fmt.Sscanf(r.Header.Get("Content-Range"), "bytes %d-%d/%d", &start, &end, &total); err != nil {
			t.Error(err)
		}
		if start != offset || total != size {
			t.Errorf("out of order range: %d, want %d; total %d", start, offset, total)
		}
		n, err := io.Copy(io.Discard, r.Body)
		if err != nil {
			t.Error(err)
		}
		if n != r.ContentLength || n != end-start+1 {
			t.Errorf("wrong byte count %d", n)
		}
		if end+1 < size && n%327680 != 0 {
			t.Errorf("unaligned intermediate chunk %d", n)
		}
		offset = end + 1
		chunks++
		if offset == size {
			w.WriteHeader(201)
			fmt.Fprintf(w, `{"id":"uploaded","size":%d}`, size)
		} else {
			w.WriteHeader(202)
			fmt.Fprintf(w, `{"nextExpectedRanges":["%d-"]}`, offset)
		}
	}))
	defer server.Close()
	base = server.URL
	p := filepath.Join(t.TempDir(), "large.bin")
	if err := os.WriteFile(p, make([]byte, size), 0600); err != nil {
		t.Fatal(err)
	}
	c := NewClient(&http.Client{Transport: bearerTransport{http.DefaultTransport}})
	c.Base = base
	it, err := c.UploadLarge(context.Background(), "/large.bin", p, "", 1, 4)
	if err != nil {
		t.Fatal(err)
	}
	if it.ID != "uploaded" || chunks != 3 || offset != size {
		t.Fatalf("incomplete upload: %+v %d %d", it, chunks, offset)
	}
}

func TestLargeUploadRejectsUnconfirmedFinalChunk(t *testing.T) {
	var base string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(204)
			return
		}
		if strings.HasSuffix(r.URL.Path, "createUploadSession") {
			fmt.Fprintf(w, `{"uploadUrl":%q}`, base+"/upload")
			return
		}
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(202)
		fmt.Fprint(w, `{}`)
	}))
	defer server.Close()
	base = server.URL
	p := filepath.Join(t.TempDir(), "a.bin")
	if err := os.WriteFile(p, []byte("abc"), 0600); err != nil {
		t.Fatal(err)
	}
	c := NewClient(server.Client())
	c.Base = base
	if _, err := c.UploadLarge(context.Background(), "/a.bin", p, "", 1, 2); err == nil {
		t.Fatal("reported incomplete upload as success")
	}
}
