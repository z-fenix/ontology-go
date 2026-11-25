package expr

import "github.com/acme/ontology/internal/spec"

// init wires the CEL validators into the spec compiler. Importing this package (which the
// ontoctl command and server binaries do) enables AT003/AT004 template validation. The spec
// package degrades gracefully when the hooks are nil (pure structural validation only).
func init() {
	spec.SetExprHooks(
		func(at *spec.ActionType, expr string) error {
			env, err := NewEnv(at)
			if err != nil {
				return err
			}
			return env.CompileBool(expr)
		},
		func(at *spec.ActionType, template string) error {
			env, err := NewEnv(at)
			if err != nil {
				return err
			}
			return ParseTemplate(template).Compile(env)
		},
	)
}
