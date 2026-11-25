package action

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/acme/ontology/internal/spec"
)

func toInt(v any) (int64, bool) {
	switch x := v.(type) {
	case int:
		return int64(x), true
	case int32:
		return int64(x), true
	case int64:
		return x, true
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1<<53 {
			return int64(x), true
		}
	case json.Number:
		n, err := x.Int64()
		return n, err == nil
	}
	return 0, false
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	}
	if n, ok := toInt(v); ok {
		return float64(n), true
	}
	return 0, false
}

// coerce converts a JSON-decoded parameter value into the Go type used by CEL and objstore.
func coerce(base spec.BaseType, v any) (any, error) {
	switch base {
	case spec.TString, spec.TAttachment:
		if s, ok := v.(string); ok {
			return s, nil
		}
		return nil, fmt.Errorf("expected string, got %T", v)
	case spec.TInteger, spec.TLong:
		n, ok := toInt(v)
		if !ok {
			return nil, fmt.Errorf("expected integer, got %v", v)
		}
		if base == spec.TInteger && (n < math.MinInt32 || n > math.MaxInt32) {
			return nil, fmt.Errorf("integer %d out of 32-bit range", n)
		}
		return n, nil
	case spec.TDouble, spec.TDecimal:
		if s, ok := v.(string); ok && base == spec.TDecimal {
			f, err := strconv.ParseFloat(s, 64)
			if err != nil {
				return nil, fmt.Errorf("expected decimal, got %q", s)
			}
			return f, nil
		}
		f, ok := toFloat(v)
		if !ok {
			return nil, fmt.Errorf("expected number, got %v", v)
		}
		return f, nil
	case spec.TBoolean:
		if b, ok := v.(bool); ok {
			return b, nil
		}
		return nil, fmt.Errorf("expected boolean, got %T", v)
	case spec.TTimestamp:
		switch x := v.(type) {
		case time.Time:
			return x.UTC(), nil
		case string:
			t, err := time.Parse(time.RFC3339Nano, x)
			if err != nil {
				return nil, fmt.Errorf("expected RFC 3339 timestamp, got %q", x)
			}
			return t.UTC(), nil
		}
		return nil, fmt.Errorf("expected timestamp, got %T", v)
	case spec.TDate:
		if s, ok := v.(string); ok {
			t, err := time.Parse(time.DateOnly, s)
			if err != nil {
				return nil, fmt.Errorf("expected date YYYY-MM-DD, got %q", s)
			}
			return t, nil
		}
		return nil, fmt.Errorf("expected date string, got %T", v)
	}
	return nil, fmt.Errorf("unsupported parameter type %s", base)
}

// coerceLiteral converts a literal from a rule's `set:` block to the property's type.
func coerceLiteral(p *spec.Property, s string) any {
	switch p.Type.Base {
	case spec.TInteger, spec.TLong:
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return n
		}
	case spec.TDouble:
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f
		}
	case spec.TBoolean:
		if b, err := strconv.ParseBool(s); err == nil {
			return b
		}
	}
	return s // strings, decimals (kept as text) and unparsable values (rejected later)
}

func zeroValue(base spec.BaseType) any {
	switch base {
	case spec.TInteger, spec.TLong:
		return int64(0)
	case spec.TDouble, spec.TDecimal:
		return float64(0)
	case spec.TBoolean:
		return false
	case spec.TTimestamp, spec.TDate:
		return time.Time{}
	}
	return ""
}

func enumKey(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	}
	if n, ok := toInt(v); ok {
		return strconv.FormatInt(n, 10)
	}
	return fmt.Sprint(v)
}

// checkConstraints validates v against c (non-applicable constraints are ignored).
func checkConstraints(c spec.Constraints, v any) error {
	if v == nil || c.IsZero() {
		return nil
	}
	if s, ok := v.(string); ok {
		if c.Regex != nil {
			re, err := regexp.Compile(*c.Regex)
			if err == nil && !re.MatchString(s) {
				return fmt.Errorf("value %q does not match %s", s, *c.Regex)
			}
		}
		n := utf8.RuneCountInString(s)
		if c.MinLen != nil && n < *c.MinLen {
			return fmt.Errorf("length %d is below minLength %d", n, *c.MinLen)
		}
		if c.MaxLen != nil && n > *c.MaxLen {
			return fmt.Errorf("length %d exceeds maxLength %d", n, *c.MaxLen)
		}
	}
	if len(c.Enum) > 0 {
		key := enumKey(v)
		found := false
		for _, e := range c.Enum {
			if e == key {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("value %q is not one of %v", key, c.Enum)
		}
	}
	if f, ok := toFloat(v); ok {
		if c.Min != nil && f < *c.Min {
			return fmt.Errorf("value %v is below min %v", f, *c.Min)
		}
		if c.Max != nil && f > *c.Max {
			return fmt.Errorf("value %v exceeds max %v", f, *c.Max)
		}
	}
	return nil
}

// checkProperty validates v against a property's own and value-type constraints.
func checkProperty(ont *spec.CompiledOntology, p *spec.Property, v any) error {
	if err := checkConstraints(p.Constraints, v); err != nil {
		return err
	}
	if p.ValueType != "" {
		if vt, ok := ont.ValueTypes[p.ValueType]; ok {
			if err := checkConstraints(vt.Constraints, v); err != nil {
				return fmt.Errorf("value type %s: %w", vt.APIName, err)
			}
		}
	}
	return nil
}

func fmtPK(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if n, ok := toInt(v); ok {
		return strconv.FormatInt(n, 10)
	}
	return fmt.Sprint(v)
}
