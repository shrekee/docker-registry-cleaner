package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/regclient/regclient"
	"github.com/regclient/regclient/config"
	"github.com/regclient/regclient/scheme/reg"
	"github.com/regclient/regclient/types/manifest"
	"github.com/regclient/regclient/types/mediatype"
	"github.com/regclient/regclient/types/ref"
	"github.com/shrekee/docker-registry-cleaner/internal/cleaner"
	"github.com/shrekee/docker-registry-cleaner/internal/writeguard"
)

type patterns []string

func (p *patterns) String() string     { return strings.Join(*p, ",") }
func (p *patterns) Set(s string) error { *p = append(*p, s); return nil }
func fatal(s string)                   { fmt.Fprintln(os.Stderr, "error:", s); os.Exit(1) }
func main() {
	if err := run(); err != nil {
		fatal(err.Error())
	}
}
func run() error {
	var repo string
	var count int
	var age, timeout time.Duration
	var pats patterns
	var execute, rewrite, maintenance, plain, distV2 bool
	flag.StringVar(&repo, "repository", "", "one explicit registry/repository (no tag or digest)")
	flag.IntVar(&count, "keep-count", 5, "keep newest N known image creation dates")
	flag.DurationVar(&age, "keep-age", 720*time.Hour, "keep images created within this duration")
	flag.Var(&pats, "protect", "tag regular expression to protect (repeatable)")
	flag.BoolVar(&execute, "execute", false, "execute tag removals")
	flag.BoolVar(&rewrite, "allow-tag-rewrite", false, "allow upstream fallback to upload and overwrite a tag")
	flag.BoolVar(&maintenance, "maintenance", false, "attest all writers are stopped for the complete operation")
	flag.DurationVar(&timeout, "timeout", 30*time.Minute, "maximum duration for the operation")
	flag.BoolVar(&distV2, "distribution-v2-8-3", false, "attest the endpoint is unmodified Distribution v2.8.3 for its DIGEST_INVALID fallback")
	flag.BoolVar(&plain, "plain-http", false, "allow unencrypted HTTP (isolated test registries only)")
	flag.Parse()
	if flag.NArg() != 0 || repo == "" || strings.ContainsAny(repo, "@?# ") || strings.Contains(repo, "://") {
		return fmt.Errorf("invalid repository")
	}
	// Require an explicit host, and reject tags. Never accept credentials in references.
	first, rest, ok := strings.Cut(repo, "/")
	if !ok || rest == "" || !(first == "localhost" || strings.ContainsAny(first, ".:")) || strings.Contains(rest, ":") {
		return fmt.Errorf("repository must be an explicit host/repository, without tag")
	}
	rr, err := ref.New(repo)
	if err != nil || rr.Scheme != "reg" || rr.Registry+"/"+rr.Repository != repo {
		return fmt.Errorf("invalid repository")
	}
	rr.Tag = ""
	var rs []*regexp.Regexp
	for _, s := range pats {
		r, e := regexp.Compile(s)
		if e != nil {
			return fmt.Errorf("invalid protect expression")
		}
		rs = append(rs, r)
	}
	if timeout <= 0 || count < 0 || age < 0 || (count == 0 && age == 0) {
		return fmt.Errorf("at least one valid retention rule required")
	}
	if execute && (!rewrite || !maintenance) {
		return fmt.Errorf("execute requires --allow-tag-rewrite and --maintenance")
	}
	scheme := "https"
	if plain {
		scheme = "http"
	}
	guard := writeguard.New(http.DefaultTransport, rr.Registry, scheme)
	hc := &http.Client{Transport: guard, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	opts := []regclient.Opt{regclient.WithDockerCreds(), regclient.WithRegOpts(reg.WithHTTPClient(hc), reg.WithRetryLimit(1))}
	if plain {
		host := config.HostNewName(rr.Registry)
		host.TLS = config.TLSDisabled
		opts = append(opts, regclient.WithConfigHost(*host))
	}
	rc := regclient.New(opts...)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	defer rc.Close(context.Background(), rr)
	entries, err := inventory(ctx, rc, rr)
	if err != nil {
		return fmt.Errorf("inventory incomplete; no changes made")
	}
	plan, err := cleaner.Plan(entries, cleaner.Policy{KeepCount: count, KeepAge: age, Protect: rs, Now: time.Now().UTC()})
	if err != nil {
		return fmt.Errorf("invalid retention plan")
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	if err = enc.Encode(map[string]any{"type": "plan", "repository": repo, "dry_run": !execute, "items": plan}); err != nil {
		return fmt.Errorf("failed to write plan")
	}
	if !execute {
		return nil
	}
	expected := map[string]string{}
	for _, v := range plan {
		expected[v.Tag] = v.Digest
	}
	for _, v := range plan {
		if v.Action != "delete-tag" {
			continue
		}
		current, e := snapshot(ctx, rc, rr)
		if e != nil || !same(current, expected) {
			return fmt.Errorf("preflight rejected: tag set or digest changed; stopped")
		}
		forbidden := make([]string, 0, len(expected))
		for _, digest := range expected {
			forbidden = append(forbidden, digest)
		}
		if err := guard.Start(rr.Registry, rr.Repository, v.Tag, scheme, distV2, forbidden); err != nil {
			return fmt.Errorf("write guard failed; stopped")
		}
		e = rc.TagDelete(ctx, rr.SetTag(v.Tag))
		poisoned := guard.Stop()
		if e != nil || poisoned != nil {
			return fmt.Errorf("tag %s (previous digest %s) deletion failed or outcome uncertain; stopped; inspect registry before retrying", v.Tag, v.Digest)
		}
		delete(expected, v.Tag)
		after, checkErr := snapshot(ctx, rc, rr)
		if checkErr != nil || !same(after, expected) {
			return fmt.Errorf("tag %s (previous digest %s) postflight mismatch or unavailable; outcome uncertain; inspect registry", v.Tag, v.Digest)
		}
		if err = enc.Encode(map[string]string{"type": "deleted-tag", "tag": v.Tag, "digest_before": v.Digest}); err != nil {
			return fmt.Errorf("output failed after mutation; inspect registry")
		}
	}
	return nil
}
func same(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

var validTag = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

func list(ctx context.Context, rc *regclient.RegClient, rr ref.Ref) ([]string, error) {
	l, e := rc.TagList(ctx, rr)
	if e != nil {
		return nil, e
	}
	t, e := l.GetTags()
	if e != nil {
		return nil, e
	}
	sort.Strings(t)
	for i, s := range t {
		if !validTag.MatchString(s) || (i > 0 && s == t[i-1]) {
			return nil, fmt.Errorf("invalid tag list")
		}
	}
	return t, nil
}
func snapshot(ctx context.Context, rc *regclient.RegClient, rr ref.Ref) (map[string]string, error) {
	tags, e := list(ctx, rc, rr)
	if e != nil {
		return nil, e
	}
	out := map[string]string{}
	for _, t := range tags {
		m, e := rc.ManifestHead(ctx, rr.SetTag(t), regclient.WithManifestRequireDigest())
		if e != nil || m.GetDescriptor().Digest.Validate() != nil {
			return nil, fmt.Errorf("missing digest")
		}
		out[t] = m.GetDescriptor().Digest.String()
	}
	return out, nil
}
func inventory(ctx context.Context, rc *regclient.RegClient, rr ref.Ref) ([]cleaner.Item, error) {
	tags, e := list(ctx, rc, rr)
	if e != nil {
		return nil, e
	}
	items := make([]cleaner.Item, 0, len(tags))
	children := map[string]bool{}
	seen := map[string]bool{}
	var walk func(ref.Ref, manifest.Manifest, int) error
	walk = func(r ref.Ref, m manifest.Manifest, depth int) error {
		if depth > 32 {
			return fmt.Errorf("index depth exceeded")
		}
		if !m.IsList() {
			return nil
		}
		ix, ok := m.(manifest.Indexer)
		if !ok {
			return fmt.Errorf("unknown index")
		}
		dl, e := ix.GetManifestList()
		if e != nil {
			return e
		}
		for _, d := range dl {
			if d.Digest.Validate() != nil {
				return fmt.Errorf("missing child digest")
			}
			ds := d.Digest.String()
			children[ds] = true
			if seen[ds] {
				continue
			}
			seen[ds] = true
			cm, e := rc.ManifestGet(ctx, r.SetDigest(ds))
			if e != nil {
				return e
			}
			if cm.GetDescriptor().Digest.String() != ds {
				return fmt.Errorf("child digest mismatch")
			}
			if e = walk(r, cm, depth+1); e != nil {
				return e
			}
		}
		return nil
	}
	for _, t := range tags {
		r := rr.SetTag(t)
		m, e := rc.ManifestGet(ctx, r)
		if e != nil {
			return nil, e
		}
		d := m.GetDescriptor()
		if d.Digest.Validate() != nil {
			return nil, fmt.Errorf("missing digest")
		}
		item := cleaner.Item{Tag: t, Digest: d.Digest.String(), MediaType: d.MediaType, Index: m.IsList()}
		if item.Index {
			if e = walk(rr, m, 0); e != nil {
				return nil, e
			}
		} else if d.MediaType == mediatype.OCI1Manifest || d.MediaType == mediatype.Docker2Manifest {
			im, ok := m.(manifest.Imager)
			if ok {
				cd, e := im.GetConfig()
				if e == nil && (cd.MediaType == mediatype.OCI1ImageConfig || cd.MediaType == mediatype.Docker2ImageConfig) {
					c, e := rc.BlobGetOCIConfig(ctx, r, cd)
					if e == nil {
						cfg := c.GetConfig()
						if cfg.Created != nil && !cfg.Created.IsZero() {
							tt := cfg.Created.UTC()
							item.Created = &tt
						}
					}
				}
			}
		}
		items = append(items, item)
	}
	for i := range items {
		items[i].Referenced = children[items[i].Digest]
	}
	return items, nil
}
