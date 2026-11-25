package spec

import (
    "fmt"
    "io/fs"
    "path"
    "regexp"
    "sort"
    "strconv"
    "strings"

    "gopkg.in/yaml.v3"
)

// File is one parsed YAML source file.
type File struct {
    Path string     // display path used in diagnostics
    Root *yaml.Node // top-level node of the document (line/column preserved)
}

var reYAMLLine = regexp.MustCompile(`line (\d+)`)

// LoadFS recursively loads *.yaml / *.yml from fsys in deterministic (lexical) order.
// displayRoot is prefixed to paths in diagnostics.
func LoadFS(fsys fs.FS, displayRoot string) ([]*File, Diagnostics) {
    var diags Diagnostics
    var paths []string
    err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
        if err != nil {
            return err
        }
        if d.IsDir() {
            return nil
        }
        switch strings.ToLower(path.Ext(p)) {
        case ".yaml", ".yml":
            paths = append(paths, p)
        }
        return nil
    })
    if err != nil {
        diags = append(diags, Diagnostic{Code: "G000", Severity: SevError,
            Message: fmt.Sprintf("cannot read ontology directory: %v", err), Pos: SourcePos{File: displayRoot}})
        return nil, diags
    }
    sort.Strings(paths)

    files := make([]*File, 0, len(paths))
    for _, p := range paths {
        display := path.Join(displayRoot, p)
        data, err := fs.ReadFile(fsys, p)
        if err != nil {
            diags = append(diags, Diagnostic{Code: "G000", Severity: SevError,
                Message: fmt.Sprintf("cannot read file: %v", err), Pos: SourcePos{File: display}})
            continue
        }
        var doc yaml.Node
        if err := yaml.Unmarshal(data, &doc); err != nil {
            line := 1
            if m := reYAMLLine.FindStringSubmatch(err.Error()); m != nil {
                line, _ = strconv.Atoi(m[1])
            }
            diags = append(diags, Diagnostic{Code: "G000", Severity: SevError,
                Message: "invalid YAML: " + err.Error(), Pos: SourcePos{File: display, Line: line, Col: 1}})
            continue
        }
        if len(doc.Content) == 0 { // empty file
            continue
        }
        files = append(files, &File{Path: display, Root: doc.Content[0]})
    }
    return files, diags
}
