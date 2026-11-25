package spec

import (
	"fmt"
	"regexp"
	"strconv"
)

type issue struct{ code, msg string }

// checkConstraints implements VT002 / VT003 for a base type.
func checkConstraints(base BaseType, c Constraints) []issue {
	if c.IsZero() {
		return nil
	}
	if !base.IsScalar() {
		return []issue{{"VT002", fmt.Sprintf("constraints are only allowed on scalar types, not %s", base)}}
	}
	var out []issue
	if c.Regex != nil {
		if base != TString {
			out = append(out, issue{"VT002", "regex is only allowed on string"})
		} else if _, err := regexp.Compile(*c.Regex); err != nil {
			out = append(out, issue{"VT003", fmt.Sprintf("invalid regex: %v", err)})
		}
	}
	if (c.MinLen != nil || c.MaxLen != nil) && base != TString {
		out = append(out, issue{"VT002", "minLength/maxLength are only allowed on string"})
	}
	if (c.Min != nil || c.Max != nil) && !base.IsNumeric() && !base.IsTemporal() {
		out = append(out, issue{"VT002", "min/max are only allowed on numeric, date or timestamp types"})
	}
	if len(c.Enum) > 0 {
		switch base {
		case TString:
		case TInteger:
			for _, e := range c.Enum {
				if _, err := strconv.Atoi(e); err != nil {
					out = append(out, issue{"VT002", fmt.Sprintf("enum value %q is not an integer", e)})
				}
			}
		default:
			out = append(out, issue{"VT002", "enum is only allowed on string or integer"})
		}
	}
	if c.Min != nil && c.Max != nil && *c.Min > *c.Max {
		out = append(out, issue{"VT002", "min must be <= max"})
	}
	if c.MinLen != nil && c.MaxLen != nil && *c.MinLen > *c.MaxLen {
		out = append(out, issue{"VT002", "minLength must be <= maxLength"})
	}
	return out
}
