package writeguard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	regclient "github.com/regclient/regclient"
	"github.com/regclient/regclient/config"
	"github.com/regclient/regclient/scheme/reg"
	"github.com/regclient/regclient/types/ref"
)

// Tests a real pinned regclient through the guard and an actual HTTP server.
// The server records each write *before* dropping the response, modelling a
// server that may have completed the mutation even when the client cannot tell.
func TestRegclientUncertainResponseDoesNotRetryOrContinueWrites(t *testing.T) {
	for _, stage := range []string{"native-disconnect", "native-retry-after", "blob-post-unexpected-success", "overwrite-disconnect", "digest-disconnect"} {
		t.Run(stage, func(t *testing.T) {
			var mu sync.Mutex
			var writes []string
			failureAt := -1
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				write := r.Method == "POST" || r.Method == "PUT" || r.Method == "PATCH" || r.Method == "DELETE"
				if write {
					mu.Lock()
					writes = append(writes, r.Method+" "+r.URL.Path)
					mu.Unlock()
				}
				fail := (stage == "native-disconnect" && r.Method == "DELETE" && r.URL.Path == tagPath) || (stage == "overwrite-disconnect" && r.Method == "PUT" && r.URL.Path == tagPath) || (stage == "digest-disconnect" && r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/v2/example/manifests/sha256:"))
				if fail {
					mu.Lock()
					failureAt = len(writes) - 1
					mu.Unlock()
					c, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Errorf("hijack: %v", err)
						return
					}
					_ = c.Close()
					return
				}
				switch {
				case r.Method == "DELETE" && r.URL.Path == tagPath:
					if stage == "native-retry-after" {
						mu.Lock()
						failureAt = len(writes) - 1
						mu.Unlock()
						w.Header().Set("Retry-After", "0")
						w.WriteHeader(http.StatusServiceUnavailable)
					} else {
						w.WriteHeader(http.StatusMethodNotAllowed)
					}
				case r.Method == "HEAD" && r.URL.Path == tagPath:
					w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
					w.Header().Set("Docker-Content-Digest", "sha256:"+strings.Repeat("a", 64))
					w.Header().Set("Content-Length", "100")
					w.WriteHeader(http.StatusOK)
				case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/v2/example/blobs/uploads"):
					if stage == "blob-post-unexpected-success" {
						mu.Lock()
						failureAt = len(writes) - 1
						mu.Unlock()
						w.WriteHeader(http.StatusOK)
					} else {
						w.WriteHeader(http.StatusCreated)
					} // pretend anonymous blob mounts succeeded
				case r.Method == "PUT" && r.URL.Path == tagPath:
					w.WriteHeader(http.StatusCreated)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			host := strings.TrimPrefix(server.URL, "http://")
			h := config.HostNewName(host)
			h.TLS = config.TLSDisabled
			g := New(http.DefaultTransport, host, "http")
			rc := regclient.New(regclient.WithConfigHost(*h), regclient.WithRegOpts(reg.WithHTTPClient(&http.Client{Transport: g, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}), reg.WithRetryLimit(1)), regclient.WithRetryDelay(time.Millisecond, time.Millisecond))
			r, err := ref.New(host + "/example:old")
			if err != nil {
				t.Fatal(err)
			}
			if err = g.Start(host, "example", "old", "http", false, nil); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err = rc.TagDelete(ctx, r)
			if err == nil {
				t.Fatal("unexpected success")
			}
			if g.Stop() == nil {
				t.Fatal("uncertain write did not poison guard")
			}
			mu.Lock()
			defer mu.Unlock()
			if failureAt < 0 {
				t.Fatalf("failure point never reached: %v", writes)
			}
			if len(writes) != failureAt+1 {
				t.Fatalf("writes followed uncertain response: failure index=%d writes=%v", failureAt, writes)
			}
			if writes[0] != "DELETE "+tagPath {
				t.Fatalf("wrong first write: %v", writes)
			}
			switch stage {
			case "native-disconnect", "native-retry-after":
				if len(writes) != 1 {
					t.Fatalf("native request retried: %v", writes)
				}
			case "blob-post-unexpected-success":
				if !strings.HasPrefix(writes[len(writes)-1], "POST /v2/example/blobs/uploads") {
					t.Fatalf("blob post not reached: %v", writes)
				}
			case "overwrite-disconnect":
				if writes[len(writes)-1] != "PUT "+tagPath {
					t.Fatalf("overwrite not reached: %v", writes)
				}
			case "digest-disconnect":
				if !strings.HasPrefix(writes[len(writes)-1], "DELETE /v2/example/manifests/sha256:") {
					t.Fatalf("digest delete not reached: %v", writes)
				}
			}
		})
	}
}
