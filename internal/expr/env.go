// Package expr compiles and evaluates the CEL expressions and templates used by action types.
//
// Scope (S2): submission-criteria compile checks, template reference checks, and runtime
// evaluation against parameter values. Multi-hop link traversal is validated structurally by
// callers (path existence); the CEL layer treats objectRef variables as dynamic maps.
package expr

import (
	"fmt"
	"time"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/uuid"

	"github.com/acme/ontology/internal/spec"
)

// MaxCostLimit bounds CEL evaluation cost (Spec §3.5).
const MaxCostLimit = 10000

// MaxLinkHops bounds link traversal depth in expressions (Spec §3.5).
const MaxLinkHops = 3

// Env is a compiled CEL environment for one action type's parameters.
type Env struct {
	cel    *cel.Env
	params map[string]*spec.ActionParam
}

func baseEnvOptions() []cel.EnvOption {
	return []cel.EnvOption{
		cel.Variable("user", cel.MapType(cel.StringType, cel.DynType)),
		// now() and uuid() are provided as zero-arg overloads with bindings.
		cel.Function("now",
			cel.Overload("now_timestamp", nil, cel.TimestampType,
				cel.FunctionBinding(func(...ref.Val) ref.Val { return types.Timestamp{Time: time.Now().UTC()} }))),
		cel.Function("uuid",
			cel.Overload("uuid_string", nil, cel.StringType,
				cel.FunctionBinding(func(...ref.Val) ref.Val { return types.String(uuid.NewString()) }))),
	}
}

func celType(t spec.ParamType) *cel.Type {
	switch t.Kind {
	case spec.KindObjectRef:
		return cel.MapType(cel.StringType, cel.DynType)
	case spec.KindObjectRefList:
		return cel.ListType(cel.MapType(cel.StringType, cel.DynType))
	}
	switch t.Base {
	case spec.TInteger, spec.TLong:
		return cel.IntType
	case spec.TDouble, spec.TDecimal:
		return cel.DoubleType
	case spec.TBoolean:
		return cel.BoolType
	case spec.TTimestamp, spec.TDate:
		return cel.TimestampType
	}
	return cel.StringType
}

// NewEnv builds the CEL environment for an action type.
func NewEnv(at *spec.ActionType) (*Env, error) {
	opts := baseEnvOptions()
	for _, name := range at.ParamOrder {
		opts = append(opts, cel.Variable(name, celType(at.Parameters[name].Type)))
	}
	e, err := cel.NewEnv(opts...)
	if err != nil {
		return nil, fmt.Errorf("build CEL env: %w", err)
	}
	return &Env{cel: e, params: at.Parameters}, nil
}

// CompileBool compiles expr and checks it yields a bool (used for submission criteria).
func (e *Env) CompileBool(expr string) error {
	ast, iss := e.cel.Compile(expr)
	if iss != nil && iss.Err() != nil {
		return fmt.Errorf("%s", iss.Err().Error())
	}
	if ast.OutputType() != cel.BoolType && ast.OutputType() != cel.DynType {
		return fmt.Errorf("expression must return bool, got %s", ast.OutputType())
	}
	return nil
}

// Program is a compiled, runnable expression.
type Program struct{ prg cel.Program }

// Compile compiles expr into a runnable program with the cost limit applied.
func (e *Env) Compile(expr string) (*Program, error) {
	ast, iss := e.cel.Compile(expr)
	if iss != nil && iss.Err() != nil {
		return nil, fmt.Errorf("%s", iss.Err().Error())
	}
	prg, err := e.cel.Program(ast, cel.CostLimit(MaxCostLimit))
	if err != nil {
		return nil, err
	}
	return &Program{prg: prg}, nil
}

// EvalBool evaluates a compiled program against vars and returns its bool result.
func (p *Program) EvalBool(vars map[string]any) (bool, error) {
	out, _, err := p.prg.Eval(vars)
	if err != nil {
		return false, err
	}
	b, ok := out.Value().(bool)
	if !ok {
		return false, fmt.Errorf("expression did not evaluate to bool")
	}
	return b, nil
}

// Eval evaluates a compiled program and returns the raw value.
func (p *Program) Eval(vars map[string]any) (ref.Val, error) {
	out, _, err := p.prg.Eval(vars)
	return out, err
}

// ReferencedParams returns the top-level parameter identifiers mentioned in expr,
// so callers can prefetch only the objects an expression needs.
func (e *Env) ReferencedParams(expr string) []string {
	var out []string
	for name := range e.params {
		if mentions(expr, name) {
			out = append(out, name)
		}
	}
	return out
}

func mentions(expr, ident string) bool {
	for i := 0; i+len(ident) <= len(expr); i++ {
		if expr[i:i+len(ident)] != ident {
			continue
		}
		before := i == 0 || !isIdentChar(expr[i-1])
		after := i+len(ident) >= len(expr) || !isIdentChar(expr[i+len(ident)])
		if before && after {
			return true
		}
	}
	return false
}

func isIdentChar(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}
