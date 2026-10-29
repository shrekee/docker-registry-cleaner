package writeguard

import (
	"context"
	"fmt"
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

// Exercises the pinned regclient path, not a reimplementation of its fallback.
// The server records mutations before simulating a failed/ambiguous response.
func TestPinnedRegclientStopsAfterUncertainNativeAndFallbackWrites(t *testing.T) {
	for _, stage := range []string{"native-delete", "overwrite"} {
		t.Run(stage, func(t *testing.T) {
			var mu sync.Mutex
			counts := map[string]int{}
			overwritten := false
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				counts[r.Method+" "+r.URL.Path]++
				mu.Unlock()
				switch {
				case r.Method == "DELETE" && r.URL.Path == tagPath:
					if stage == "native-delete" {
						w.WriteHeader(500)
					} else {
						w.WriteHeader(405)
					}
				case r.Method == "HEAD" && r.URL.Path == tagPath:
					w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
					w.Header().Set("Docker-Content-Digest", "sha256:"+strings.Repeat("a", 64))
					w.Header().Set("Content-Length", "100")
					w.WriteHeader(200)
				case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/v2/example/blobs/uploads"):
					// Simulate a successful blob mount so fallback reaches manifest overwrite.
					w.WriteHeader(201)
				case r.Method == "PUT" && r.URL.Path == tagPath:
					overwritten = true // registry accepted the bytes but the reply is an error
					w.WriteHeader(500)
				default:
					w.WriteHeader(500)
				}
			}))
			defer s.Close()
			host := strings.TrimPrefix(s.URL, "http://")
			h := config.HostNewName(host)
			h.TLS = config.TLSDisabled
			g := New(http.DefaultTransport, host, "http")
			rc := regclient.New(regclient.WithConfigHost(*h), regclient.WithRegOpts(reg.WithHTTPClient(&http.Client{Transport: g}), reg.WithRetryLimit(1)), regclient.WithRetryDelay(time.Millisecond, time.Millisecond))
			r, err := ref.New(fmt.Sprintf("%s/example:old", host))
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
				t.Fatal("TagDelete unexpectedly succeeded")
			}
			if g.Stop() == nil {
				t.Fatal("missing uncertain result")
			}
			mu.Lock()
			defer mu.Unlock()
			if counts["DELETE "+tagPath] != 1 {
				t.Fatalf("native DELETE repeated: %v", counts)
			}
			if stage == "native-delete" && counts["PUT "+tagPath] != 0 {
				t.Fatalf("unexpected fallback after uncertain native DELETE: %v", counts)
			}
			if stage == "overwrite" && (!overwritten || counts["PUT "+tagPath] != 1) {
				t.Fatalf("did not reach a single simulated overwrite: %v", counts)
			}
			for key := range counts {
				if strings.HasPrefix(key, "DELETE /v2/example/manifests/sha256:") {
					t.Fatalf("unexpected subsequent digest deletion: %v", counts)
				}
			}
		})
	}
}
