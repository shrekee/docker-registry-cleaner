// Package integration exercises the built executable against a disposable local
// Distribution server. No external registry or credentials are used.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	digest "github.com/opencontainers/go-digest"
	regclient "github.com/regclient/regclient"
	"github.com/regclient/regclient/config"
	"github.com/regclient/regclient/types/descriptor"
	"github.com/regclient/regclient/types/manifest"
	"github.com/regclient/regclient/types/mediatype"
	v1 "github.com/regclient/regclient/types/oci/v1"
	"github.com/regclient/regclient/types/ref"
)

// Run with DISTRIBUTION_BIN and CLEANER_BIN pointing to the exact local
// binaries under test. This test intentionally does not download dependencies.
func TestDisposableDistribution(t *testing.T) {
	serverBin, cleanerBin := os.Getenv("DISTRIBUTION_BIN"), os.Getenv("CLEANER_BIN")
	if serverBin == "" || cleanerBin == "" {
		t.Skip("set DISTRIBUTION_BIN and CLEANER_BIN to run isolated integration")
	}
	dir := t.TempDir()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host := l.Addr().String()
	l.Close()
	cfg := fmt.Sprintf("version: 0.1\nlog:\n  level: error\nstorage:\n  filesystem:\n    rootdirectory: %s\n  delete:\n    enabled: true\nhttp:\n  addr: %s\n", filepath.Join(dir, "data"), host)
	confPath := filepath.Join(dir, "config.yml")
	if err = os.WriteFile(confPath, []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}
	server := exec.Command(serverBin, "serve", confPath)
	var logs bytes.Buffer
	server.Stdout = &logs
	server.Stderr = &logs
	if err = server.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Process.Kill(); _ = server.Wait() }()
	deadline := time.Now().Add(15 * time.Second)
	for {
		r, e := http.Get("http://" + host + "/v2/")
		if e == nil {
			r.Body.Close()
			if r.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("registry not ready: %s", logs.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	// A controllable loopback reverse proxy can deterministically emulate a
	// backend that explicitly rejects only native tag DELETE with 405, or
	// rejects the fallback manifest PUT. It never contacts external services.
	force405 := os.Getenv("INTEGRATION_FORCE_405") == "1"
	failPut := os.Getenv("INTEGRATION_FAIL_FALLBACK") == "1"
	dropDelete := os.Getenv("INTEGRATION_DROP_DELETE") == "1"
	stale := os.Getenv("INTEGRATION_STALE") == "1"
	pageFail := os.Getenv("INTEGRATION_PAGE_FAIL") == "1"
	noOp202 := os.Getenv("INTEGRATION_NOOP_202") == "1"
	var mu sync.Mutex
	var tagDeletes, fallbackPuts, mutationsAfterFailure, listCalls, armedMutations int
	failed := false
	armed := false
	if force405 || dropDelete || stale || pageFail || noOp202 {
		target, _ := url.Parse("http://" + host)
		proxy := httputil.NewSingleHostReverseProxy(target)
		listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			path := r.URL.Path
			if armed && (r.Method == "PUT" || r.Method == "POST" || r.Method == "PATCH" || r.Method == "DELETE") {
				armedMutations++
			}
			if armed && pageFail && path == "/v2/test/fixture/tags/list" && r.Method == "GET" {
				listCalls++
				mu.Unlock()
				if r.URL.Query().Get("last") != "" {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Link", `</v2/test/fixture/tags/list?n=1&last=stable>; rel="next"`)
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"name":"test/fixture","tags":["stable"]}`))
				return
			}
			if failed && (r.Method == "PUT" || r.Method == "POST" || r.Method == "PATCH" || r.Method == "DELETE") {
				mutationsAfterFailure++
			}
			if armed && stale && path == "/v2/test/fixture/tags/list" && r.Method == "GET" {
				listCalls++
				if listCalls == 2 {
					// Simulate a separate writer changing state between inventory
					// and preflight, by removing the target on this disposable server.
					req, _ := http.NewRequest("DELETE", target.String()+"/v2/test/fixture/manifests/expired", nil)
					response, err := http.DefaultClient.Do(req)
					if err != nil || response.StatusCode != 202 {
						if response != nil {
							response.Body.Close()
						}
						mu.Unlock()
						w.WriteHeader(500)
						return
					}
					response.Body.Close()
				}
			}
			if armed && path == "/v2/test/fixture/manifests/expired" && r.Method == "DELETE" {
				tagDeletes++
				if noOp202 {
					mu.Unlock()
					w.WriteHeader(http.StatusAccepted)
					return
				}
				if dropDelete {
					// Deliver the DELETE to v3, then discard the 202 response and
					// break the client connection: the mutation really happened.
					req, _ := http.NewRequest("DELETE", target.String()+path, nil)
					response, err := http.DefaultClient.Do(req)
					if err == nil {
						response.Body.Close()
					}
					failed = true
					mu.Unlock()
					conn, _, err := w.(http.Hijacker).Hijack()
					if err == nil {
						conn.Close()
					}
					return
				}
				if force405 {
					mu.Unlock()
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
			}
			if armed && path == "/v2/test/fixture/manifests/expired" && r.Method == "PUT" {
				fallbackPuts++
				if failPut {
					failed = true
					mu.Unlock()
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
			}
			mu.Unlock()
			proxy.ServeHTTP(w, r)
		}))
		defer listener.Close()
		host = strings.TrimPrefix(listener.URL, "http://")
	}
	ctx := context.Background()
	rc := regclient.New(regclient.WithConfigHost(config.Host{Name: host, TLS: config.TLSDisabled}))
	rr, err := ref.New(host + "/test/fixture")
	if err != nil {
		t.Fatal(err)
	}
	// Two distinct old manifests plus a recent one. stable/alias share a digest.
	old := time.Now().Add(-90 * 24 * time.Hour).UTC().Truncate(time.Second)
	recent := time.Now().UTC().Truncate(time.Second)
	seed := func(tag, label string, created time.Time) {
		t.Helper()
		body, err := json.Marshal(v1.Image{Created: &created, Config: v1.ImageConfig{Labels: map[string]string{"fixture": label}}, RootFS: v1.RootFS{Type: "layers", DiffIDs: []digest.Digest{}}})
		if err != nil {
			t.Fatal(err)
		}
		desc := descriptor.Descriptor{MediaType: mediatype.OCI1ImageConfig, Digest: digest.FromBytes(body), Size: int64(len(body))}
		if _, err = rc.BlobPut(ctx, rr, desc, bytes.NewReader(body)); err != nil {
			t.Fatal(err)
		}
		m, err := manifest.New(manifest.WithOrig(v1.Manifest{Versioned: v1.ManifestSchemaVersion, MediaType: mediatype.OCI1Manifest, Config: desc, Layers: []descriptor.Descriptor{}}))
		if err != nil {
			t.Fatal(err)
		}
		if err = rc.ManifestPut(ctx, rr.SetTag(tag), m); err != nil {
			t.Fatal(err)
		}
	}
	seed("stable", "shared", old)
	seed("alias", "shared", old)
	seed("expired", "distinct", old)
	seed("recent", "recent", recent)
	seed("probe", "probe", old)
	seed("child", "child", old)
	childManifest, err := rc.ManifestGet(ctx, rr.SetTag("child"))
	if err != nil {
		t.Fatal(err)
	}
	index, err := manifest.New(manifest.WithOrig(v1.Index{Versioned: v1.ManifestSchemaVersion, MediaType: mediatype.OCI1ManifestList, Manifests: []descriptor.Descriptor{childManifest.GetDescriptor()}}))
	if err != nil {
		t.Fatal(err)
	}
	if err = rc.ManifestPut(ctx, rr.SetTag("index"), index); err != nil {
		t.Fatal(err)
	}
	expiredManifest, err := rc.ManifestGet(ctx, rr.SetTag("expired"))
	if err != nil {
		t.Fatal(err)
	}
	expiredDigest := expiredManifest.GetDescriptor().Digest.String()
	// Report direct tag-delete behavior on this exact disposable backend. The
	// probe has no protected aliases and is not part of the tested retention plan.
	req, _ := http.NewRequest(http.MethodDelete, "http://"+host+"/v2/test/fixture/manifests/probe", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	t.Logf("Distribution direct tag DELETE status: %d", res.StatusCode)
	if res.StatusCode != http.StatusAccepted {
		// The native probe failed; demonstrate upstream fallback on a throwaway tag.
		if err = rc.TagDelete(ctx, rr.SetTag("probe")); err != nil {
			t.Fatalf("fallback probe: %v", err)
		}
	}
	tags := func() map[string]bool {
		t.Helper()
		list, e := rc.TagList(ctx, rr)
		if e != nil {
			t.Fatal(e)
		}
		out := map[string]bool{}
		for _, tag := range list.Tags {
			out[tag] = true
		}
		return out
	}
	run := func(args ...string) (string, error) {
		t.Helper()
		c := exec.Command(cleanerBin, args...)
		c.Env = append(os.Environ(), "DOCKER_CONFIG="+dir)
		b, e := c.CombinedOutput()
		return string(b), e
	}
	base := []string{"--repository", host + "/test/fixture", "--plain-http", "--keep-count", "0", "--keep-age", "24h", "--protect", "^stable$"}
	zeroBaseline := []string{"--repository", host + "/test/fixture", "--plain-http", "--keep-count", "0", "--keep-age", "0", "--protect", "^stable$", "--execute", "--allow-tag-rewrite", "--maintenance"}
	if o, e := run(zeroBaseline...); e == nil {
		t.Fatalf("protect-only zero baseline unexpectedly succeeded: %s", o)
	}
	before := tags()
	output, err := run(base...)
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, output)
	}
	after := tags()
	if fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatalf("dry-run modified tags: %v -> %v", before, after)
	}
	if !strings.Contains(output, "expired") || !strings.Contains(output, "stable") {
		t.Fatalf("missing plan entries: %s", output)
	}
	bad := append(append([]string{}, base...), "--execute")
	if o, e := run(bad...); e == nil {
		t.Fatalf("execution without required acknowledgments unexpectedly succeeded: %s", o)
	}
	if fmt.Sprint(before) != fmt.Sprint(tags()) {
		t.Fatal("rejected preflight modified tags")
	}
	good := append(append([]string{}, base...), "--execute", "--allow-tag-rewrite", "--maintenance")
	if os.Getenv("INTEGRATION_V2_ENABLE") == "1" {
		good = append(good, "--distribution-v2-8-3")
	}
	mu.Lock()
	armed = true
	mu.Unlock()
	o, e := run(good...)
	if noOp202 {
		if e == nil || strings.Contains(o, `"type":"deleted-tag"`) {
			t.Fatalf("no-op 202 falsely reported success: %s", o)
		}
		mu.Lock()
		writes, td := armedMutations, tagDeletes
		mu.Unlock()
		if writes != 1 || td != 1 || !tags()["expired"] {
			t.Fatalf("unexpected no-op state writes=%d deletes=%d", writes, td)
		}
		return
	}
	if pageFail {
		if e == nil || strings.Contains(o, `"type":"plan"`) {
			t.Fatalf("partial paginated listing was treated as success: %s", o)
		}
		mu.Lock()
		writes, pages := armedMutations, listCalls
		mu.Unlock()
		if writes != 0 || pages < 2 {
			t.Fatalf("pagination failure did not fail closed: writes=%d pages=%d", writes, pages)
		}
		return
	}
	if dropDelete {
		if e == nil {
			t.Fatalf("lost response returned success: %s", o)
		}
		mu.Lock()
		td, fp, later := tagDeletes, fallbackPuts, mutationsAfterFailure
		mu.Unlock()
		if td != 1 || fp != 0 || later != 0 {
			t.Fatalf("uncertain DELETE retried/followed by writes: delete=%d put=%d later=%d", td, fp, later)
		}
		if tags()["expired"] {
			t.Fatal("test setup failed: target should have been deleted before response was lost")
		}
		return
	}
	if stale {
		if e == nil || !strings.Contains(o, "preflight") {
			t.Fatalf("stale plan did not fail preflight: %s", o)
		}
		mu.Lock()
		td := tagDeletes
		mu.Unlock()
		if td != 0 {
			t.Fatalf("tool wrote despite stale preflight: %d deletes", td)
		}
		return
	}
	if failPut {
		if e == nil {
			t.Fatalf("injected fallback failure returned success: %s", o)
		}
		mu.Lock()
		td, fp, later := tagDeletes, fallbackPuts, mutationsAfterFailure
		mu.Unlock()
		if td != 1 || fp != 1 || later != 0 {
			t.Fatalf("unexpected write attempts after failure: delete=%d put=%d later=%d", td, fp, later)
		}
		if !tags()["expired"] {
			t.Fatal("injected rejected PUT unexpectedly removed tag")
		}
		return
	}
	if os.Getenv("INTEGRATION_EXPECT_REJECT") == "1" {
		if e == nil {
			t.Fatalf("expected rejected/uncertain operation, got success: %s", o)
		}
		if !tags()["expired"] {
			t.Fatal("rejected native tag DELETE unexpectedly removed tag")
		}
		return
	}
	if e != nil {
		t.Fatalf("execute: %v\n%s", e, o)
	}
	if force405 {
		mu.Lock()
		td, fp := tagDeletes, fallbackPuts
		mu.Unlock()
		if td != 1 || fp != 1 {
			t.Fatalf("fallback not observed exactly once: delete=%d put=%d", td, fp)
		}
		t.Logf("controlled 405 caused upstream fallback: native DELETE=%d, replacement PUT=%d", td, fp)
	}
	after = tags()
	if after["expired"] {
		t.Fatalf("expired tag remains: %v", after)
	}
	if !after["stable"] || !after["alias"] || !after["recent"] || !after["child"] || !after["index"] {
		t.Fatalf("protected/recent tags missing: %v", after)
	}
	verifyClient := regclient.New(regclient.WithConfigHost(config.Host{Name: host, TLS: config.TLSDisabled}))
	if _, err = verifyClient.ManifestGet(ctx, rr.SetDigest(expiredDigest)); err != nil {
		t.Fatalf("old digest unexpectedly unavailable after tag removal: %v", err)
	}
	if _, err = rc.ManifestGet(ctx, rr.SetTag("stable")); err != nil {
		t.Fatalf("protected alias unreadable: %v", err)
	}
}
