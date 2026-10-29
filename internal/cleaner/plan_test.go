package cleaner

import (
	"regexp"
	"testing"
	"time"
)

func tp(t time.Time) *time.Time { return &t }
func TestUnionAliasUnknownAndIndex(t *testing.T) {
	now := time.Date(2026, 1, 20, 0, 0, 0, 0, time.UTC)
	items := []Item{
		{Tag: "latest", Digest: "shared", Created: tp(now.Add(-90 * 24 * time.Hour))},
		{Tag: "old-alias", Digest: "shared", Created: tp(now.Add(-90 * 24 * time.Hour))},
		{Tag: "new", Digest: "new", Created: tp(now.Add(-time.Hour))},
		{Tag: "recent", Digest: "recent", Created: tp(now.Add(-48 * time.Hour))},
		{Tag: "old", Digest: "old", Created: tp(now.Add(-100 * 24 * time.Hour))},
		{Tag: "unknown", Digest: "u"}, {Tag: "multiarch", Digest: "idx", Index: true},
		{Tag: "child", Digest: "child", Created: tp(now.Add(-200 * 24 * time.Hour)), Referenced: true},
	}
	got, err := Plan(items, Policy{KeepCount: 1, KeepAge: 72 * time.Hour, Protect: []*regexp.Regexp{regexp.MustCompile("^latest$")}, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]Item{}
	for _, v := range got {
		m[v.Tag] = v
	}
	for _, s := range []string{"latest", "old-alias", "new", "recent", "unknown", "multiarch", "child"} {
		if m[s].Action != "keep" {
			t.Errorf("%s not kept: %+v", s, m[s])
		}
	}
	if m["old"].Action != "delete-tag" {
		t.Fatal(m["old"])
	}
	if m["old-alias"].Reasons[0] != "protected-digest-alias" {
		t.Fatal(m["old-alias"])
	}
}
func TestTieDeterministicAndInvalid(t *testing.T) {
	now := time.Now()
	a := []Item{{Tag: "b", Digest: "b", Created: tp(now)}, {Tag: "a", Digest: "a", Created: tp(now)}}
	p, e := Plan(a, Policy{KeepCount: 1, Now: now})
	if e != nil || p[0].Tag != "a" || p[0].Action != "keep" || p[1].Action != "delete-tag" {
		t.Fatal(p, e)
	}
	if _, e = Plan(a, Policy{}); e == nil {
		t.Fatal("missing policy accepted")
	}
	if _, e = Plan(append(a, a[0]), Policy{KeepCount: 1}); e == nil {
		t.Fatal("duplicate accepted")
	}
	if _, e = Plan([]Item{{Tag: "x"}}, Policy{KeepCount: 1}); e == nil {
		t.Fatal("missing digest accepted")
	}
}
func TestAgeBoundaryAndFuture(t *testing.T) {
	now := time.Date(2026, 1, 20, 0, 0, 0, 0, time.UTC)
	items := []Item{{Tag: "edge", Digest: "1", Created: tp(now.Add(-24 * time.Hour))}, {Tag: "future", Digest: "2", Created: tp(now.Add(time.Hour))}, {Tag: "outside", Digest: "3", Created: tp(now.Add(-24*time.Hour - time.Nanosecond))}}
	out, e := Plan(items, Policy{KeepAge: 24 * time.Hour, Now: now})
	if e != nil {
		t.Fatal(e)
	}
	m := map[string]string{}
	for _, v := range out {
		m[v.Tag] = v.Action
	}
	if m["edge"] != "keep" || m["future"] != "keep" || m["outside"] != "delete-tag" {
		t.Fatal(m)
	}
}

func TestProtectAloneIsNotABaseline(t *testing.T) {
	_, err := Plan([]Item{{Tag: "x", Digest: "sha256:abc"}}, Policy{Protect: []*regexp.Regexp{regexp.MustCompile("^x$")}})
	if err == nil {
		t.Fatal("protect-only policy must be rejected")
	}
}

func TestFutureCreatedAlwaysRetainedWithCountOnly(t *testing.T) {
	now := time.Date(2026, 1, 20, 0, 0, 0, 0, time.UTC)
	items := []Item{{Tag: "f1", Digest: "1", Created: tp(now.Add(time.Hour))}, {Tag: "f2", Digest: "2", Created: tp(now.Add(2 * time.Hour))}, {Tag: "past", Digest: "3", Created: tp(now.Add(-time.Hour))}}
	p, e := Plan(items, Policy{KeepCount: 1, Now: now})
	if e != nil {
		t.Fatal(e)
	}
	for _, v := range p {
		if (v.Tag == "f1" || v.Tag == "f2") && v.Action != "keep" {
			t.Fatalf("future tag must be retained: %+v", v)
		}
	}
}
