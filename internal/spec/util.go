package spec

import (
    "fmt"
    "regexp"
    "sort"
    "strings"
    "unicode"
)

var (
    reTypeName     = regexp.MustCompile(`^[A-Z][A-Za-z0-9]{1,63}$`)
    reMemberName   = regexp.MustCompile(`^[a-z][A-Za-z0-9]{1,63}$`)
    reOntologyName = regexp.MustCompile(`^[a-z][a-z0-9_]{1,63}$`)
    reTemplate     = regexp.MustCompile(`\{\{\s*([A-Za-z_][A-Za-z0-9_]*)\s*\}\}`)
)

// ToSnake converts camelCase / PascalCase to snake_case ("HTTPServer" -> "http_server").
func ToSnake(s string) string {
    rs := []rune(s)
    var b strings.Builder
    for i, r := range rs {
        if unicode.IsUpper(r) {
            if i > 0 {
                prev := rs[i-1]
                nextLower := i+1 < len(rs) && unicode.IsLower(rs[i+1])
                if unicode.IsLower(prev) || unicode.IsDigit(prev) || (unicode.IsUpper(prev) && nextLower) {
                    b.WriteByte('_')
                }
            }
            b.WriteRune(unicode.ToLower(r))
            continue
        }
        b.WriteRune(r)
    }
    return b.String()
}

func levenshtein(a, b string) int {
    ra, rb := []rune(a), []rune(b)
    prev := make([]int, len(rb)+1)
    cur := make([]int, len(rb)+1)
    for j := range prev {
        prev[j] = j
    }
    for i := 1; i <= len(ra); i++ {
        cur[0] = i
        for j := 1; j <= len(rb); j++ {
            cost := 1
            if ra[i-1] == rb[j-1] {
                cost = 0
            }
            cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
        }
        prev, cur = cur, prev
    }
    return prev[len(rb)]
}

// suggest returns the closest candidate (case-insensitive distance <= 2), or "".
func suggest(name string, candidates []string) string {
    sorted := append([]string(nil), candidates...)
    sort.Strings(sorted)
    best, bestD := "", 3
    for _, c := range sorted {
        d := levenshtein(strings.ToLower(name), strings.ToLower(c))
        if d < bestD && d < len(name) {
            best, bestD = c, d
        }
    }
    return best
}

func didYouMean(name string, candidates []string) string {
    if s := suggest(name, candidates); s != "" {
        return fmt.Sprintf(" (did you mean %q?)", s)
    }
    return ""
}

func keys[V any](m map[string]V) []string {
    out := make([]string, 0, len(m))
    for k := range m {
        out = append(out, k)
    }
    sort.Strings(out)
    return out
}
