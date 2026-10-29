package writeguard

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func testGuard(base http.RoundTripper) *Guard      { return New(base, "registry.invalid", "https") }
func plainHTTPGuard(base http.RoundTripper) *Guard { return New(base, "registry.invalid", "http") }

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func reply(code int) *http.Response {
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}
}
func req(method, path string) *http.Request {
	var body io.Reader
	if method == "PUT" && path == tagPath {
		body = strings.NewReader("dummy-manifest")
	}
	r, _ := http.NewRequest(method, "https://registry.invalid"+path, body)
	return r
}

const tagPath = "/v2/example/manifests/old"

func TestFailurePoisonStopsAllWrites(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		err  error
	}{
		{"disconnect", 0, errors.New("secret-url")}, {"server", 500, nil}, {"throttle", 429, nil}, {"forbidden", 403, nil}, {"unexpected-success", 200, nil}, {"notfound", 404, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			g := testGuard(transportFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if tc.err != nil {
					return nil, tc.err
				}
				return reply(tc.code), nil
			}))
			if err := g.Start("registry.invalid", "example", "old", "https", false, nil); err != nil {
				t.Fatal(err)
			}
			_, _ = g.RoundTrip(req("DELETE", tagPath))
			_, err := g.RoundTrip(req("DELETE", tagPath))
			if !errors.Is(err, ErrUncertain) {
				t.Fatalf("retry err %v", err)
			}
			_, err = g.RoundTrip(req("POST", "/v2/example/blobs/uploads/"))
			if !errors.Is(err, ErrUncertain) {
				t.Fatalf("fallback err %v", err)
			}
			if calls != 1 {
				t.Fatalf("wire writes=%d", calls)
			}
			if !errors.Is(g.Stop(), ErrUncertain) {
				t.Fatal("missing uncertain outcome")
			}
		})
	}
}
func TestExplicit405AndAuthenticationPermitFallback(t *testing.T) {
	codes := []int{401, 405, 202, 201, 202}
	calls := 0
	g := testGuard(transportFunc(func(r *http.Request) (*http.Response, error) {
		c := codes[calls]
		if r.Body != nil {
			_, _ = io.Copy(io.Discard, r.Body)
		}
		calls++
		resp := reply(c)
		if c == 202 {
			resp.Header.Set("Location", "/v2/example/blobs/uploads/id")
		}
		if c == 401 {
			resp.Header.Set("WWW-Authenticate", `Basic realm="registry"`)
		}
		return resp, nil
	}))
	if err := g.Start("registry.invalid", "example", "old", "https", false, nil); err != nil {
		t.Fatal(err)
	}
	for _, r := range []*http.Request{req("DELETE", tagPath), req("DELETE", tagPath), req("POST", "/v2/example/blobs/uploads/"), req("PUT", "/v2/example/manifests/old"), req("DELETE", fmt.Sprintf("/v2/example/manifests/sha256:%x", sha256.Sum256([]byte("dummy-manifest"))))} {
		if _, err := g.RoundTrip(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := g.Stop(); err != nil {
		t.Fatal(err)
	}
	if calls != 5 {
		t.Fatal(calls)
	}
}
func TestFallbackFailureDoesNotRetry(t *testing.T) {
	calls := 0
	g := testGuard(transportFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return reply(405), nil
		}
		return nil, errors.New("lost reply")
	}))
	_ = g.Start("registry.invalid", "example", "old", "https", false, nil)
	_, _ = g.RoundTrip(req("DELETE", tagPath))
	_, _ = g.RoundTrip(req("PUT", tagPath))
	_, err := g.RoundTrip(req("DELETE", "/v2/example/manifests/sha256:"+strings.Repeat("a", 64)))
	if !errors.Is(err, ErrUncertain) || calls != 2 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
	if !errors.Is(g.Stop(), ErrUncertain) {
		t.Fatal("missing poison")
	}
}
func TestNativeSuccessEndsWrites(t *testing.T) {
	calls := 0
	g := testGuard(transportFunc(func(r *http.Request) (*http.Response, error) { calls++; return reply(202), nil }))
	_ = g.Start("registry.invalid", "example", "old", "https", false, nil)
	_, _ = g.RoundTrip(req("DELETE", tagPath))
	_, err := g.RoundTrip(req("PUT", tagPath))
	if !errors.Is(err, ErrUncertain) || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
	if err := g.Stop(); err != nil {
		t.Fatal(err)
	}
}
func TestOutsideOperationAndWrongFirstWrite(t *testing.T) {
	calls := 0
	g := testGuard(transportFunc(func(r *http.Request) (*http.Response, error) { calls++; return reply(200), nil }))
	_, err := g.RoundTrip(req("PUT", tagPath))
	if !errors.Is(err, ErrInactive) {
		t.Fatal(err)
	}
	_ = g.Start("registry.invalid", "example", "old", "https", false, nil)
	_, err = g.RoundTrip(req("DELETE", "/v2/other/manifests/old"))
	if !errors.Is(err, ErrUncertain) || calls != 0 {
		t.Fatalf("%v %d", err, calls)
	}
	if !errors.Is(g.Stop(), ErrUncertain) {
		t.Fatal("missing poison")
	}
}

func TestFallbackOnlyAllowsScopedWrites(t *testing.T) {
	for _, tc := range []struct{ name, method, url string }{
		{"foreign host", "PUT", "https://evil.invalid/v2/example/manifests/old"},
		{"foreign repo", "PUT", "https://registry.invalid/v2/other/manifests/old"},
		{"different tag", "PUT", "https://registry.invalid/v2/example/manifests/new"},
		{"invalid digest", "DELETE", "https://registry.invalid/v2/example/manifests/sha256:abc"},
		{"arbitrary POST", "POST", "https://registry.invalid/v2/example/other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			g := testGuard(transportFunc(func(*http.Request) (*http.Response, error) { calls++; return reply(405), nil }))
			_ = g.Start("registry.invalid", "example", "old", "https", false, nil)
			_, _ = g.RoundTrip(req("DELETE", tagPath))
			r, _ := http.NewRequest(tc.method, tc.url, nil)
			_, err := g.RoundTrip(r)
			if !errors.Is(err, ErrUncertain) || calls != 1 {
				t.Fatalf("err=%v, wire writes=%d", err, calls)
			}
			if !errors.Is(g.Stop(), ErrUncertain) {
				t.Fatal("missing poison")
			}
		})
	}
}

func TestChallengeMustBePresentAndNotRepeated(t *testing.T) {
	for _, challenge := range []bool{false, true} {
		calls := 0
		g := testGuard(transportFunc(func(*http.Request) (*http.Response, error) {
			calls++
			r := reply(401)
			if challenge {
				r.Header.Set("WWW-Authenticate", `Basic realm="registry"`)
			}
			return r, nil
		}))
		_ = g.Start("registry.invalid", "example", "old", "https", false, nil)
		_, _ = g.RoundTrip(req("DELETE", tagPath))
		_, _ = g.RoundTrip(req("DELETE", tagPath))
		_, err := g.RoundTrip(req("DELETE", tagPath))
		limit := 1
		if challenge {
			limit = 2
		}
		if calls != limit || !errors.Is(err, ErrUncertain) || !errors.Is(g.Stop(), ErrUncertain) {
			t.Fatalf("challenge=%v calls=%d err=%v", challenge, calls, err)
		}
	}
}

func TestDistributionV2ExceptionIsExactAndOptIn(t *testing.T) {
	valid := `{"errors":[{"code":"DIGEST_INVALID","message":"invalid digest","detail":{"x":"y"}}]}`
	tests := []struct {
		name, body, contentType string
		opt, accept             bool
	}{
		{"enabled", valid, "application/json; charset=utf-8", true, true},
		{"not opted in", valid, "application/json", false, false},
		{"different code", `{"errors":[{"code":"DENIED"}]}`, "application/json", true, false},
		{"two errors", `{"errors":[{"code":"DIGEST_INVALID"},{"code":"DENIED"}]}`, "application/json", true, false},
		{"duplicate code", `{"errors":[{"code":"DIGEST_INVALID","code":"DENIED"}]}`, "application/json", true, false},
		{"duplicate envelope", `{"errors":[{"code":"DIGEST_INVALID"}],"errors":[]}`, "application/json", true, false},
		{"trailing bytes", valid + "true", "application/json", true, false},
		{"oversized", valid + strings.Repeat(" ", 8193), "application/json", true, false},
		{"non-json", valid, "text/plain", true, false},
		{"malformed", `{"errors":[{"code":"DIGEST_INVALID"}`, "application/json", true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			g := testGuard(transportFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					return &http.Response{StatusCode: 400, Header: http.Header{"Content-Type": []string{tc.contentType}}, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
				}
				if r.Body != nil {
					_, _ = io.Copy(io.Discard, r.Body)
				}
				return reply(201), nil
			}))
			_ = g.Start("registry.invalid", "example", "old", "https", tc.opt, nil)
			resp, _ := g.RoundTrip(req("DELETE", tagPath))
			b, _ := io.ReadAll(resp.Body)
			if string(b) != tc.body {
				t.Fatal("response body was not preserved")
			}
			_, err := g.RoundTrip(req("PUT", tagPath))
			if tc.accept {
				if err != nil || calls != 2 {
					t.Fatalf("expected fallback: calls=%d err=%v", calls, err)
				}
			} else if !errors.Is(err, ErrUncertain) || calls != 1 {
				t.Fatalf("unexpected fallback: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestFallbackCannotDeleteUnrelatedDigest(t *testing.T) {
	calls := 0
	g := testGuard(transportFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return reply(405), nil
		}
		if r.Body != nil {
			_, _ = io.Copy(io.Discard, r.Body)
		}
		return reply(201), nil
	}))
	_ = g.Start("registry.invalid", "example", "old", "https", false, nil)
	_, _ = g.RoundTrip(req("DELETE", tagPath))
	_, _ = g.RoundTrip(req("PUT", tagPath))
	_, err := g.RoundTrip(req("DELETE", "/v2/example/manifests/sha256:"+strings.Repeat("a", 64)))
	if !errors.Is(err, ErrUncertain) || calls != 2 || !errors.Is(g.Stop(), ErrUncertain) {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}
func TestIncompleteTagPutCannotAuthorizeDigestDelete(t *testing.T) {
	calls := 0
	g := testGuard(transportFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return reply(405), nil
		}
		return reply(201), nil
	}))
	_ = g.Start("registry.invalid", "example", "old", "https", false, nil)
	_, _ = g.RoundTrip(req("DELETE", tagPath))
	_, _ = g.RoundTrip(req("PUT", tagPath))
	_, err := g.RoundTrip(req("DELETE", fmt.Sprintf("/v2/example/manifests/sha256:%x", sha256.Sum256([]byte("dummy-manifest")))))
	if !errors.Is(err, ErrUncertain) || calls != 2 || !errors.Is(g.Stop(), ErrUncertain) {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

type brokenBody struct{}

func (brokenBody) Read([]byte) (int, error) { return 0, errors.New("lost body") }
func (brokenBody) Close() error             { return nil }
func TestDistributionV2ReadFailureAndMalformedChallengeFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name string
		resp *http.Response
	}{
		{"lost body", &http.Response{StatusCode: 400, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: brokenBody{}}},
		{"malformed challenge", &http.Response{StatusCode: 401, Header: http.Header{"Www-Authenticate": []string{"not a valid challenge"}}, Body: io.NopCloser(strings.NewReader(""))}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			g := testGuard(transportFunc(func(*http.Request) (*http.Response, error) { calls++; return tc.resp, nil }))
			_ = g.Start("registry.invalid", "example", "old", "https", true, nil)
			_, _ = g.RoundTrip(req("DELETE", tagPath))
			_, err := g.RoundTrip(req("POST", "/v2/example/blobs/uploads/"))
			if !errors.Is(err, ErrUncertain) || calls != 1 || !errors.Is(g.Stop(), ErrUncertain) {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestTagPutAuthenticationChallengeRebuildsDigest(t *testing.T) {
	calls := 0
	g := testGuard(transportFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Body != nil {
			_, _ = io.Copy(io.Discard, r.Body)
		}
		switch calls {
		case 1:
			return reply(405), nil
		case 2:
			resp := reply(401)
			resp.Header.Set("WWW-Authenticate", `Basic realm="registry"`)
			return resp, nil
		case 3:
			return reply(201), nil
		default:
			return reply(202), nil
		}
	}))
	_ = g.Start("registry.invalid", "example", "old", "https", false, nil)
	for _, r := range []*http.Request{req("DELETE", tagPath), req("PUT", tagPath), req("PUT", tagPath), req("DELETE", fmt.Sprintf("/v2/example/manifests/sha256:%x", sha256.Sum256([]byte("dummy-manifest"))))} {
		if _, err := g.RoundTrip(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := g.Stop(); err != nil || calls != 4 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

type delayedReader struct {
	release <-chan struct{}
	sent    bool
}

func (r *delayedReader) Read(p []byte) (int, error) {
	<-r.release
	if r.sent {
		return 0, io.EOF
	}
	r.sent = true
	return copy(p, "dummy-manifest"), nil
}
func TestAsyncBodyReadAfterRoundTripFailsClosed(t *testing.T) {
	release := make(chan struct{})
	done := make(chan struct{})
	calls := 0
	g := testGuard(transportFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return reply(405), nil
		}
		go func() { defer close(done); _, _ = io.Copy(io.Discard, r.Body) }()
		return reply(201), nil
	}))
	_ = g.Start("registry.invalid", "example", "old", "https", false, nil)
	_, _ = g.RoundTrip(req("DELETE", tagPath))
	r, _ := http.NewRequest("PUT", "https://registry.invalid"+tagPath, &delayedReader{release: release})
	r.ContentLength = int64(len("dummy-manifest"))
	_, _ = g.RoundTrip(r)
	_, err := g.RoundTrip(req("DELETE", fmt.Sprintf("/v2/example/manifests/sha256:%x", sha256.Sum256([]byte("dummy-manifest")))))
	close(release)
	<-done
	if !errors.Is(err, ErrUncertain) || calls != 2 || !errors.Is(g.Stop(), ErrUncertain) {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestExistingDigestIsNeverDeleted(t *testing.T) {
	calls := 0
	g := testGuard(transportFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return reply(405), nil
		}
		if r.Body != nil {
			_, _ = io.Copy(io.Discard, r.Body)
		}
		return reply(201), nil
	}))
	existing := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("dummy-manifest")))
	_ = g.Start("registry.invalid", "example", "old", "https", false, []string{existing})
	_, _ = g.RoundTrip(req("DELETE", tagPath))
	_, _ = g.RoundTrip(req("PUT", tagPath))
	_, err := g.RoundTrip(req("DELETE", "/v2/example/manifests/"+existing))
	if !errors.Is(err, ErrUncertain) || calls != 2 || !errors.Is(g.Stop(), ErrUncertain) {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestSchemeDowngradeAndFragmentBlocked(t *testing.T) {
	for _, uri := range []string{"http://registry.invalid/v2/example/blobs/uploads/", "https://registry.invalid/v2/example/blobs/uploads/#fragment", "https://user:pass@registry.invalid/v2/example/blobs/uploads/"} {
		t.Run(uri, func(t *testing.T) {
			calls := 0
			g := testGuard(transportFunc(func(*http.Request) (*http.Response, error) { calls++; return reply(405), nil }))
			_ = g.Start("registry.invalid", "example", "old", "https", false, nil)
			_, _ = g.RoundTrip(req("DELETE", tagPath))
			r, _ := http.NewRequest("POST", uri, nil)
			_, err := g.RoundTrip(r)
			if !errors.Is(err, ErrUncertain) || calls != 1 || !errors.Is(g.Stop(), ErrUncertain) {
				t.Fatalf("err=%v calls=%d", err, calls)
			}
		})
	}
}
func TestExplicitPlainHTTPWorks(t *testing.T) {
	calls := 0
	g := plainHTTPGuard(transportFunc(func(*http.Request) (*http.Response, error) { calls++; return reply(202), nil }))
	_ = g.Start("registry.invalid", "example", "old", "http", false, nil)
	r, _ := http.NewRequest("DELETE", "http://registry.invalid"+tagPath, nil)
	_, err := g.RoundTrip(r)
	if err != nil || calls != 1 || g.Stop() != nil {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestInventoryReadsOnlyRegistryHostAndScheme(t *testing.T) {
	for _, tc := range []struct {
		name, uri   string
		credentials bool
		allowed     bool
	}{
		{"same origin", "https://registry.invalid/v2/example/tags/list", true, true},
		{"foreign Link unauthenticated", "https://other.invalid/v2/example/tags/list", false, false},
		{"foreign Link with credentials", "https://other.invalid/v2/example/tags/list", true, false},
		{"scheme downgrade", "http://registry.invalid/v2/example/tags/list", true, false},
		{"userinfo", "https://user:secret@registry.invalid/v2/example/tags/list", false, false},
		{"fragment", "https://registry.invalid/v2/example/tags/list#secret", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			g := testGuard(transportFunc(func(*http.Request) (*http.Response, error) { calls++; return reply(200), nil }))
			r, _ := http.NewRequest("GET", tc.uri, nil)
			if tc.credentials {
				r.Header.Set("Authorization", "Bearer secret")
			}
			_, err := g.RoundTrip(r)
			if tc.allowed {
				if err != nil || calls != 1 {
					t.Fatalf("expected allowed; calls=%d err=%v", calls, err)
				}
			} else if err == nil || strings.Contains(err.Error(), "secret") || calls != 0 {
				t.Fatalf("unsafe request escaped: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestUnsafeReadDuringDeletePoisonsLaterWrites(t *testing.T) {
	calls := 0
	g := testGuard(transportFunc(func(*http.Request) (*http.Response, error) { calls++; return reply(405), nil }))
	_ = g.Start("registry.invalid", "example", "old", "https", false, nil)
	_, _ = g.RoundTrip(req("DELETE", tagPath))
	foreign, _ := http.NewRequest("GET", "https://foreign.invalid/v2/example/tags/list", nil)
	_, readErr := g.RoundTrip(foreign)
	_, writeErr := g.RoundTrip(req("POST", "/v2/example/blobs/uploads/"))
	if !errors.Is(readErr, ErrUncertain) || !errors.Is(writeErr, ErrUncertain) || calls != 1 || !errors.Is(g.Stop(), ErrUncertain) {
		t.Fatalf("read=%v write=%v calls=%d", readErr, writeErr, calls)
	}
}
