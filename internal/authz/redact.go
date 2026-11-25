package authz

import (
	"fmt"
	"sort"
	"strings"

	"github.com/acme/ontology/internal/spec"
)

var sensitivityRank = map[spec.Sensitivity]int{
	spec.SensPublic: 0, spec.SensInternal: 1, spec.SensConfidential: 2, spec.SensRestricted: 3,
}

func rank(s spec.Sensitivity) int {
	if r, ok := sensitivityRank[s]; ok {
		return r
	}
	return sensitivityRank[spec.SensInternal] // unset means INTERNAL (the compiler default)
}

// Clearances maps a group to the highest property sensitivity its members may read (Spec §8.2).
type Clearances map[string]spec.Sensitivity

// ParseClearances parses "grp-finance=CONFIDENTIAL,grp-security=RESTRICTED".
func ParseClearances(s string) (Clearances, error) {
	c := Clearances{}
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		group, level, ok := strings.Cut(item, "=")
		lv := spec.Sensitivity(strings.ToUpper(strings.TrimSpace(level)))
		if !ok || strings.TrimSpace(group) == "" || !lv.Valid() {
			return nil, fmt.Errorf("invalid clearance %q (want group=PUBLIC|INTERNAL|CONFIDENTIAL|RESTRICTED)", item)
		}
		c[strings.TrimSpace(group)] = lv
	}
	return c, nil
}

// Level returns the clearance for a set of groups. Every authenticated caller may read INTERNAL.
func (c Clearances) Level(groups []string) spec.Sensitivity {
	level := spec.SensInternal
	for _, g := range groups {
		if l, ok := c[g]; ok && rank(l) > rank(level) {
			level = l
		}
	}
	return level
}

// Redact returns a copy of props in which every property more sensitive than level is set to
// nil, plus the sorted names of the redacted properties (reported to clients as __redacted).
func Redact(ot *spec.ObjectType, props map[string]any, level spec.Sensitivity) (map[string]any, []string) {
	out := make(map[string]any, len(props))
	for k, v := range props {
		out[k] = v
	}
	var redacted []string
	for name, p := range ot.Properties {
		if _, present := out[name]; present && rank(p.Sensitivity) > rank(level) {
			out[name] = nil
			redacted = append(redacted, name)
		}
	}
	sort.Strings(redacted)
	return out, redacted
}
