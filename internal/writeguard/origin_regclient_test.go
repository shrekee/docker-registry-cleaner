package writeguard

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	digest "github.com/opencontainers/go-digest"
	regclient "github.com/regclient/regclient"
	"github.com/regclient/regclient/config"
	"github.com/regclient/regclient/scheme/reg"
	"github.com/regclient/regclient/types/descriptor"
	"github.com/regclient/regclient/types/ref"
)

// These tests exercise pinned regclient v0.11.7, not just Guard.RoundTrip.
func testClient(host, scheme, user, pass string, transport http.RoundTripper) (*regclient.RegClient, *Guard) {
	h := config.HostNewName(host)
	if scheme == "http" {
		h.TLS = config.TLSDisabled
	}
	h.User = user
	h.Pass = pass
	guard := New(transport, host, scheme)
	rc := regclient.New(regclient.WithConfigHost(*h), regclient.WithRegOpts(reg.WithHTTPClient(&http.Client{Transport: guard}), reg.WithRetryLimit(1)), regclient.WithRetryDelay(time.Millisecond, time.Millisecond))
	return rc, guard
}
func testRef(t *testing.T, host string) ref.Ref {
	t.Helper()
	r, err := ref.New(host + "/example:old")
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestRegclientSameOriginBasicAuthentication(t *testing.T) {
	var authenticated atomic.Int32
	user, pass := "fixture-user", "fixture-password"
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte(user+":"+pass)) {
			w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
			w.WriteHeader(401)
			return
		}
		authenticated.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"name":"example","tags":["old"]}`)
	}))
	defer s.Close()
	host := strings.TrimPrefix(s.URL, "http://")
	rc, _ := testClient(host, "http", user, pass, http.DefaultTransport)
	tl, err := rc.TagList(testCtx(t), testRef(t, host))
	if err != nil {
		t.Fatalf("same-origin Basic failed: %v", err)
	}
	if authenticated.Load() != 1 || len(tl.Tags) != 1 || tl.Tags[0] != "old" {
		t.Fatalf("unexpected result: auth calls=%d tags=%v", authenticated.Load(), tl.Tags)
	}
}

func TestRegclientCrossOriginPaginationBlocked(t *testing.T) {
	var foreignCalls atomic.Int32
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignCalls.Add(1)
		fmt.Fprint(w, `{"name":"example","tags":["fake"]}`)
	}))
	defer foreign.Close()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Link", fmt.Sprintf(`<%s/v2/example/tags/list?last=old>; rel="next"`, foreign.URL))
		fmt.Fprint(w, `{"name":"example","tags":["old"]}`)
	}))
	defer s.Close()
	host := strings.TrimPrefix(s.URL, "http://")
	rc, _ := testClient(host, "http", "fixture-user", "fixture-password", http.DefaultTransport)
	_, err := rc.TagList(testCtx(t), testRef(t, host))
	if err == nil || foreignCalls.Load() != 0 {
		t.Fatalf("pagination must fail closed: err=%v foreign calls=%d", err, foreignCalls.Load())
	}
}

func TestRegclientExternalBearerRealmBlockedBeforeNetwork(t *testing.T) {
	var foreignCalls atomic.Int32
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignCalls.Add(1)
		fmt.Fprint(w, `{"token":"example"}`)
	}))
	defer foreign.Close()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="registry"`, foreign.URL))
		w.WriteHeader(401)
	}))
	defer s.Close()
	host := strings.TrimPrefix(s.URL, "http://")
	rc, _ := testClient(host, "http", "fixture-user", "fixture-password", http.DefaultTransport)
	_, err := rc.TagList(testCtx(t), testRef(t, host))
	if err == nil || foreignCalls.Load() != 0 {
		t.Fatalf("external token service must be blocked: err=%v foreign calls=%d", err, foreignCalls.Load())
	}
}

func TestRegclientUploadDowngradeBlockedBeforeNetwork(t *testing.T) {
	var deletes, posts, puts atomic.Int32
	var host string
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "DELETE":
			deletes.Add(1)
			w.WriteHeader(405)
		case r.Method == "HEAD":
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", "sha256:"+strings.Repeat("a", 64))
			w.Header().Set("Content-Length", "100")
			w.WriteHeader(200)
		case r.Method == "POST":
			posts.Add(1)
			w.Header().Set("Location", "http://"+host+"/v2/example/blobs/uploads/id")
			w.WriteHeader(202)
		case r.Method == "PUT":
			puts.Add(1)
			w.WriteHeader(201)
		default:
			w.WriteHeader(500)
		}
	}))
	defer s.Close()
	host = strings.TrimPrefix(s.URL, "https://")
	rc, guard := testClient(host, "https", "fixture-user", "fixture-password", s.Client().Transport)
	if err := guard.Start(host, "example", "old", "https", false, nil); err != nil {
		t.Fatal(err)
	}
	err := rc.TagDelete(testCtx(t), testRef(t, host))
	guardErr := guard.Stop()
	if err == nil || guardErr == nil || deletes.Load() != 1 || posts.Load() != 1 || puts.Load() != 0 {
		t.Fatalf("downgraded upload must fail closed: err=%v guard=%v DELETE=%d POST=%d PUT=%d", err, guardErr, deletes.Load(), posts.Load(), puts.Load())
	}
}

func TestRegclientForeignDescriptorURLBlocked(t *testing.T) {
	var foreignCalls atomic.Int32
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { foreignCalls.Add(1); w.WriteHeader(200) }))
	defer foreign.Close()
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) }))
	defer registry.Close()
	host := strings.TrimPrefix(registry.URL, "http://")
	rc, _ := testClient(host, "http", "fixture-user", "fixture-password", http.DefaultTransport)
	d := descriptor.Descriptor{Digest: digest.FromString("fixture"), URLs: []string{foreign.URL + "/blob"}}
	_, err := rc.BlobGet(testCtx(t), testRef(t, host), d)
	if err == nil || foreignCalls.Load() != 0 {
		t.Fatalf("foreign descriptor must fail closed: err=%v calls=%d", err, foreignCalls.Load())
	}
}
