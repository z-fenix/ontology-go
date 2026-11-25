package oms

import (
	"testing/fstest"

	"github.com/acme/ontology/internal/spec"

	// Importing expr registers the CEL validators with spec via its init (Spec §5.5:
	// the server performs the same validation as ontoctl).
	_ "github.com/acme/ontology/internal/expr"
)

// compileFiles compiles in-memory source files the same way ontoctl compiles a directory.
func compileFiles(files []SourceFile) (*spec.CompiledOntology, spec.Diagnostics) {
	fsys := fstest.MapFS{}
	for _, f := range files {
		fsys[f.Path] = &fstest.MapFile{Data: []byte(f.Content)}
	}
	loaded, diags := spec.LoadFS(fsys, "")
	ont, cdiags := spec.Compile(loaded)
	all := append(diags, cdiags...)
	all.Sort()
	return ont, all
}
