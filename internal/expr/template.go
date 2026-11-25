package expr

import (
	"fmt"
	"regexp"
	"strings"
)

var reTemplateExpr = regexp.MustCompile(`\{\{(.*?)\}\}`)

// Template is a string that may embed {{ CEL expression }} fragments.
type Template struct {
	raw       string
	wholeExpr string   // non-empty when the whole string is a single expression (type preserved)
	exprs     []string // all embedded expressions, in order
}

// ParseTemplate extracts the embedded expressions from s.
func ParseTemplate(s string) *Template {
	t := &Template{raw: s}
	ms := reTemplateExpr.FindAllStringSubmatch(s, -1)
	for _, m := range ms {
		t.exprs = append(t.exprs, strings.TrimSpace(m[1]))
	}
	if len(t.exprs) == 1 {
		if trimmed := strings.TrimSpace(s); strings.HasPrefix(trimmed, "{{") && strings.HasSuffix(trimmed, "}}") &&
			strings.Count(trimmed, "{{") == 1 {
			t.wholeExpr = t.exprs[0]
		}
	}
	return t
}

// Exprs returns the embedded expressions.
func (t *Template) Exprs() []string { return t.exprs }

// IsWholeExpr reports whether the template is exactly one expression (result keeps its type).
func (t *Template) IsWholeExpr() bool { return t.wholeExpr != "" }

// Compile compiles every embedded expression against env (for validation).
func (t *Template) Compile(env *Env) error {
	for _, ex := range t.exprs {
		if _, err := env.Compile(ex); err != nil {
			return fmt.Errorf("template %q: %w", t.raw, err)
		}
	}
	return nil
}

// Render evaluates the template against vars. A whole-expression template keeps the raw value's
// type; a mixed template renders to a string.
func (t *Template) Render(env *Env, vars map[string]any) (any, error) {
	if t.wholeExpr != "" {
		prg, err := env.Compile(t.wholeExpr)
		if err != nil {
			return nil, err
		}
		v, err := prg.Eval(vars)
		if err != nil {
			return nil, err
		}
		return v.Value(), nil
	}
	out := t.raw
	for _, ex := range t.exprs {
		prg, err := env.Compile(ex)
		if err != nil {
			return nil, err
		}
		v, err := prg.Eval(vars)
		if err != nil {
			return nil, err
		}
		out = strings.Replace(out, "{{"+ex+"}}", fmt.Sprint(v.Value()), 1)
		// also handle spaced form
		out = reTemplateExpr.ReplaceAllStringFunc(out, func(m string) string {
			if strings.TrimSpace(m[2:len(m)-2]) == ex {
				return fmt.Sprint(v.Value())
			}
			return m
		})
	}
	return out, nil
}
