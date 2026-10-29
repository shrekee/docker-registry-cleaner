// Package cleaner implements a conservative tag-retention policy.
package cleaner

import (
	"fmt"
	"regexp"
	"sort"
	"time"
)

type Item struct {
	Tag        string     `json:"tag"`
	Digest     string     `json:"digest"`
	MediaType  string     `json:"media_type"`
	Created    *time.Time `json:"created,omitempty"`
	Index      bool       `json:"index"`
	Referenced bool       `json:"referenced_by_index"`
	Reasons    []string   `json:"reasons"`
	Action     string     `json:"action"`
}
type Policy struct {
	KeepCount int
	KeepAge   time.Duration
	Protect   []*regexp.Regexp
	Now       time.Time
}

// Plan retains the union of all retention rules. Unknown timestamps and indexes
// are always retained. Digest aliases of a protected tag are retained as a group.
func Plan(items []Item, p Policy) ([]Item, error) {
	if p.KeepCount < 0 || p.KeepAge < 0 || (p.KeepCount == 0 && p.KeepAge == 0) {
		return nil, fmt.Errorf("at least one valid retention rule is required")
	}
	out := append([]Item(nil), items...)
	seen := map[string]bool{}
	for i := range out {
		v := &out[i]
		if v.Tag == "" || v.Digest == "" || seen[v.Tag] {
			return nil, fmt.Errorf("incomplete or duplicate tag metadata")
		}
		seen[v.Tag] = true
		v.Reasons = nil
		v.Action = "delete-tag"
	}
	ranked := make([]int, 0, len(out))
	for i := range out {
		if out[i].Created != nil && !out[i].Created.IsZero() {
			ranked = append(ranked, i)
		}
	}
	sort.Slice(ranked, func(i, j int) bool {
		a, b := out[ranked[i]], out[ranked[j]]
		if a.Created.Equal(*b.Created) {
			return a.Tag < b.Tag
		}
		return a.Created.After(*b.Created)
	})
	for i := range out {
		v := &out[i]
		if v.Created == nil || v.Created.IsZero() {
			v.Reasons = append(v.Reasons, "unknown-created")
		}
		if v.Created != nil && v.Created.After(p.Now) {
			v.Reasons = append(v.Reasons, "future-created")
		}
		if v.Index {
			v.Reasons = append(v.Reasons, "index")
		}
		if v.Referenced {
			v.Reasons = append(v.Reasons, "index-child")
		}
		for _, re := range p.Protect {
			if re.MatchString(v.Tag) {
				v.Reasons = append(v.Reasons, "protected-pattern")
				break
			}
		}
		if v.Created != nil && p.KeepAge > 0 && !v.Created.Before(p.Now.Add(-p.KeepAge)) {
			v.Reasons = append(v.Reasons, "within-keep-age")
		}
	}
	for i, idx := range ranked {
		if i < p.KeepCount {
			out[idx].Reasons = append(out[idx].Reasons, "within-keep-count")
		}
	}
	protected := map[string]bool{}
	for _, v := range out {
		if len(v.Reasons) > 0 {
			protected[v.Digest] = true
		}
	}
	for i := range out {
		v := &out[i]
		if protected[v.Digest] && len(v.Reasons) == 0 {
			v.Reasons = append(v.Reasons, "protected-digest-alias")
		}
		if len(v.Reasons) > 0 {
			v.Action = "keep"
		} else {
			v.Reasons = []string{"outside-retention"}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Tag < out[j].Tag })
	return out, nil
}
