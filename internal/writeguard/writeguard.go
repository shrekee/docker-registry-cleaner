// Package writeguard prevents failed or ambiguous registry mutations from being
// retried or followed by further mutations. It is deliberately conservative.
package writeguard

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"sync"
)

var (
	ErrUncertain = errors.New("registry mutation was rejected or its outcome is uncertain; stop and inspect the registry before retrying")
	ErrInactive  = errors.New("registry mutation attempted outside an active operation")
	ErrActive    = errors.New("a registry mutation operation is already active")
	sha256Digest = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	// Recognize only the common complete challenges; exotic valid forms fail closed.
	authChallenge = regexp.MustCompile(`(?i)^(basic|bearer)[ \t]+realm[ \t]*=[ \t]*("[^"\r\n]+"|[^, \t\r\n]+)([ \t]*,.*)?$`)
)

type phase uint8

const (
	inactive phase = iota
	initial
	fallback
	completed
	poisoned
)

// Guard serializes mutation attempts. A failed or ambiguous mutation poisons
// the entire operation; subsequent writes are stopped before the network.
// Reads remain available for diagnosis. Never share a Guard across concurrent
// independent operations.
type Guard struct {
	registryHost            string
	registryScheme          string
	mu                      sync.Mutex
	base                    http.RoundTripper
	phase                   phase
	host, repo, tag, scheme string
	allowDistributionV2     bool
	dummyDigest             string
	forbiddenDigests        map[string]bool
	challenged              map[string]bool
}

// New configures a guard for every request throughout the client lifecycle.
func New(base http.RoundTripper, registryHost, registryScheme string) *Guard {
	if base == nil {
		base = http.DefaultTransport
	}
	return &Guard{base: base, registryHost: registryHost, registryScheme: registryScheme}
}

// Start opens a single upstream TagDelete operation, scoped to one registry,
// repository, and tag. Foreign-host uploads are refused, even when redirected.
func (g *Guard) Start(host, repo, tag, scheme string, allowDistributionV2 bool, forbiddenDigests []string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.phase != inactive {
		return ErrActive
	}
	if (scheme != "https" && scheme != "http") || scheme != g.registryScheme || !strings.EqualFold(host, g.registryHost) || host == "" || repo == "" || tag == "" || strings.ContainsAny(host, "/@?#") || strings.ContainsAny(repo, "?#") || strings.ContainsAny(tag, "/?#") {
		return errors.New("invalid operation scope")
	}
	g.host, g.repo, g.tag, g.scheme = host, repo, tag, scheme
	g.allowDistributionV2 = allowDistributionV2
	g.dummyDigest = ""
	g.forbiddenDigests = make(map[string]bool, len(forbiddenDigests))
	for _, d := range forbiddenDigests {
		g.forbiddenDigests[d] = true
	}
	g.challenged = map[string]bool{}
	g.phase = initial
	return nil
}

// Stop closes an operation. A non-nil result means the final state requires
// inspection. Also treat any upstream TagDelete error as uncertain.
func (g *Guard) Stop() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	var err error
	if g.phase == poisoned {
		err = ErrUncertain
	}
	g.phase = inactive
	g.host = ""
	g.scheme = ""
	g.repo = ""
	g.tag = ""
	g.dummyDigest = ""
	g.forbiddenDigests = nil
	g.challenged = nil
	return err
}
func mutation(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}
func (g *Guard) allowed(r *http.Request) bool {
	if r.URL == nil || r.URL.User != nil || r.URL.Fragment != "" || r.URL.Scheme != g.scheme || !strings.EqualFold(r.URL.Host, g.host) {
		return false
	}
	root := "/v2/" + g.repo + "/"
	path := r.URL.Path
	tagPath := root + "manifests/" + g.tag
	if g.phase == initial {
		return r.Method == http.MethodDelete && path == tagPath
	}
	if g.phase != fallback {
		return false
	}
	uploads := root + "blobs/uploads/"
	switch r.Method {
	case http.MethodPost:
		return path == uploads || path == strings.TrimSuffix(uploads, "/")
	case http.MethodPatch:
		return strings.HasPrefix(path, uploads) && len(path) > len(uploads)
	case http.MethodPut:
		return path == tagPath || (strings.HasPrefix(path, uploads) && len(path) > len(uploads))
	case http.MethodDelete:
		prefix := root + "manifests/"
		return strings.HasPrefix(path, prefix) && sha256Digest.MatchString(strings.TrimPrefix(path, prefix)) && g.dummyDigest != "" && strings.TrimPrefix(path, prefix) == g.dummyDigest
	}
	return false
}
func (g *Guard) RoundTrip(r *http.Request) (*http.Response, error) {
	// Protect reads as well: pagination and token endpoints may use DirectURL.
	// Do not print request URLs: query strings can contain sensitive values.
	if !g.safeRequest(r) {
		g.mu.Lock()
		if g.phase != inactive {
			g.phase = poisoned
		}
		g.mu.Unlock()
		return nil, ErrUncertain
	}
	if !mutation(r.Method) {
		return g.base.RoundTrip(r)
	}
	// Serializing through the network call prevents a concurrent write escaping
	// between receiving a failed response and poisoning the operation.
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.phase == inactive {
		return nil, ErrInactive
	}
	if g.phase == poisoned || g.phase == completed {
		return nil, ErrUncertain
	}
	if !g.allowed(r) {
		g.phase = poisoned
		return nil, ErrUncertain
	}
	var tracker *bodyTracker
	if g.phase == fallback && r.Method == http.MethodPut && r.URL.Path == "/v2/"+g.repo+"/manifests/"+g.tag {
		if r.Body == nil {
			g.phase = poisoned
			return nil, ErrUncertain
		}
		tracker = &bodyTracker{ReadCloser: r.Body, hash: sha256.New()}
		r.Body = tracker
	}
	resp, err := g.base.RoundTrip(r)
	if err != nil || resp == nil {
		g.phase = poisoned
		return nil, ErrUncertain
	}
	if resp.StatusCode == http.StatusUnauthorized {
		// A single well-formed challenge may precede the authenticated attempt.
		// Do not leak the URL or the challenge through returned errors.
		key := r.Method + " " + r.URL.String()
		if !authChallenge.MatchString(strings.TrimSpace(resp.Header.Get("WWW-Authenticate"))) || g.challenged[key] {
			g.phase = poisoned
		} else {
			g.challenged[key] = true
		}
		return resp, nil
	}
	if g.phase == initial {
		switch resp.StatusCode {
		case http.StatusAccepted:
			g.phase = completed
		case http.StatusMethodNotAllowed:
			g.phase = fallback
		case http.StatusBadRequest:
			if g.allowDistributionV2 && distributionDigestInvalid(resp) {
				g.phase = fallback
			} else {
				g.phase = poisoned
			}
		default:
			g.phase = poisoned
		}
	} else if !g.expectedFallbackStatus(r, resp) {
		g.phase = poisoned
	} else if tracker != nil {
		n, eof, sum := tracker.snapshot()
		if !eof && (r.ContentLength <= 0 || n != r.ContentLength) {
			g.phase = poisoned
		} else {
			g.dummyDigest = "sha256:" + hex.EncodeToString(sum)
			if g.forbiddenDigests[g.dummyDigest] {
				g.phase = poisoned
			}
		}
	}
	return resp, nil
}

// expectedFallbackStatus mirrors the statuses accepted by the pinned upstream
// for its fallback writes; unexpected successes cannot cause a second write.
func (g *Guard) expectedFallbackStatus(r *http.Request, resp *http.Response) bool {
	path := r.URL.Path
	switch r.Method {
	case http.MethodPost:
		return resp.StatusCode == 201 || (resp.StatusCode == 202 && resp.Header.Get("Location") != "")
	case http.MethodPatch:
		return resp.StatusCode == 202 || resp.StatusCode == 201
	case http.MethodPut:
		if path == "/v2/"+g.repo+"/manifests/"+g.tag {
			return resp.StatusCode == 201
		}
		return resp.StatusCode == 201 || resp.StatusCode == 204
	case http.MethodDelete:
		return resp.StatusCode == 202
	}
	return false
}

type replayBody struct {
	io.Reader
	io.Closer
}

// This one exception is only enabled by explicit Distribution v2 attestation.
// Read at most 8 KiB and restore the body for upstream. Duplicate/unknown keys,
// malformed JSON, multiple errors, other error codes or trailing data fail closed.
func distributionDigestInvalid(resp *http.Response) bool {
	mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" || resp.Body == nil {
		return false
	}
	original := resp.Body
	data, err := io.ReadAll(io.LimitReader(original, 8193))
	resp.Body = &replayBody{Reader: io.MultiReader(bytes.NewReader(data), original), Closer: original}
	if err != nil || len(data) > 8192 {
		return false
	}
	d := json.NewDecoder(bytes.NewReader(data))
	tok, err := d.Token()
	if err != nil || tok != json.Delim('{') {
		return false
	}
	key, err := d.Token()
	if err != nil || key != "errors" {
		return false
	}
	tok, err = d.Token()
	if err != nil || tok != json.Delim('[') {
		return false
	}
	tok, err = d.Token()
	if err != nil || tok != json.Delim('{') {
		return false
	}
	seen := map[string]bool{}
	code := ""
	for d.More() {
		k, e := d.Token()
		if e != nil {
			return false
		}
		name, ok := k.(string)
		if !ok || seen[name] {
			return false
		}
		seen[name] = true
		switch name {
		case "code":
			if d.Decode(&code) != nil {
				return false
			}
		case "message":
			var v string
			if d.Decode(&v) != nil {
				return false
			}
		case "detail":
			var v json.RawMessage
			if d.Decode(&v) != nil {
				return false
			}
		default:
			return false
		}
	}
	tok, err = d.Token()
	if err != nil || tok != json.Delim('}') || code != "DIGEST_INVALID" {
		return false
	}
	tok, err = d.Token()
	if err != nil || tok != json.Delim(']') {
		return false
	}
	tok, err = d.Token()
	if err != nil || tok != json.Delim('}') {
		return false
	}
	_, err = d.Token()
	return err == io.EOF
}

type bodyTracker struct {
	io.ReadCloser
	mu   sync.Mutex
	hash hash.Hash
	n    int64
	eof  bool
}

func (b *bodyTracker) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.mu.Lock()
	if n > 0 {
		_, _ = b.hash.Write(p[:n])
		b.n += int64(n)
	}
	if err == io.EOF {
		b.eof = true
	}
	b.mu.Unlock()
	return n, err
}
func (b *bodyTracker) snapshot() (int64, bool, []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.n, b.eof, b.hash.Sum(nil)
}

func (g *Guard) safeRequest(r *http.Request) bool {
	if r == nil || r.URL == nil || g.registryHost == "" || (g.registryScheme != "https" && g.registryScheme != "http") || r.URL.Scheme != g.registryScheme || r.URL.User != nil || r.URL.Fragment != "" || r.URL.Host == "" {
		return false
	}
	return strings.EqualFold(r.URL.Host, g.registryHost)
}
