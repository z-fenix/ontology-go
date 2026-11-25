package spec

import (
    "fmt"
    "sort"
)

// SourcePos locates an element in the YAML sources.
type SourcePos struct {
    File string `json:"file,omitempty"`
    Line int    `json:"line,omitempty"`
    Col  int    `json:"col,omitempty"`
}

func (p SourcePos) String() string {
    if p.File == "" {
        return "<unknown>"
    }
    return fmt.Sprintf("%s:%d:%d", p.File, p.Line, p.Col)
}

// Severity of a diagnostic.
type Severity string

const (
    SevError   Severity = "error"
    SevWarning Severity = "warning"
)

// Diagnostic is a single validation finding.
type Diagnostic struct {
    Code     string    `json:"code"`
    Severity Severity  `json:"severity"`
    Message  string    `json:"message"`
    Path     string    `json:"path,omitempty"`
    Pos      SourcePos `json:"pos"`
}

func (d Diagnostic) String() string {
    if d.Path == "" {
        return fmt.Sprintf("%s %s %s\n  %s", d.Pos, d.Severity, d.Code, d.Message)
    }
    return fmt.Sprintf("%s %s %s\n  %s: %s", d.Pos, d.Severity, d.Code, d.Path, d.Message)
}

// Diagnostics is an ordered list of findings.
type Diagnostics []Diagnostic

// HasErrors reports whether any error exists (warnings count when strict).
func (ds Diagnostics) HasErrors(strict bool) bool {
    for _, d := range ds {
        if d.Severity == SevError || (strict && d.Severity == SevWarning) {
            return true
        }
    }
    return false
}

// Count returns the number of errors and warnings.
func (ds Diagnostics) Count() (errs, warns int) {
    for _, d := range ds {
        switch d.Severity {
        case SevError:
            errs++
        case SevWarning:
            warns++
        }
    }
    return errs, warns
}

// Codes returns the diagnostic codes in order (useful in tests).
func (ds Diagnostics) Codes() []string {
    out := make([]string, 0, len(ds))
    for _, d := range ds {
        out = append(out, d.Code)
    }
    return out
}

// Sort orders diagnostics by file, line, column, code (in place).
func (ds Diagnostics) Sort() {
    sort.SliceStable(ds, func(i, j int) bool {
        a, b := ds[i].Pos, ds[j].Pos
        if a.File != b.File {
            return a.File < b.File
        }
        if a.Line != b.Line {
            return a.Line < b.Line
        }
        if a.Col != b.Col {
            return a.Col < b.Col
        }
        return ds[i].Code < ds[j].Code
    })
}
