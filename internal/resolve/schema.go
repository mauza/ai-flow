package resolve

import (
	"fmt"
	"sort"

	"cel.dev/cel-go/cel"
)

// OutputSchema converts an outputs shorthand into JSON Schema.
//
//	string | number | integer | bool | object | [string] | <JSON Schema map>
func OutputSchema(typ any) (map[string]any, error) {
	switch t := typ.(type) {
	case string:
		switch t {
		case "string", "number", "integer", "object":
			return map[string]any{"type": t}, nil
		case "bool", "boolean":
			return map[string]any{"type": "boolean"}, nil
		}
		return nil, fmt.Errorf("unknown type %q (string, number, integer, bool, object, [string] or a JSON Schema)", t)
	case []any:
		if len(t) != 1 {
			return nil, fmt.Errorf("list types look like [string]")
		}
		item, err := OutputSchema(t[0])
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "array", "items": item}, nil
	case map[string]any:
		if _, ok := t["type"]; !ok {
			return nil, fmt.Errorf("JSON Schema needs a type")
		}
		return t, nil
	}
	return nil, fmt.Errorf("unsupported output type %v", typ)
}

// ResultSchema is the JSON Schema a node's structured result must satisfy:
// {outcome: <enum>, summary: string, outputs: {...}}.
func ResultSchema(n *Node) map[string]any {
	props := map[string]any{}
	var required []string
	names := make([]string, 0, len(n.Outputs))
	for k := range n.Outputs {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		s, err := OutputSchema(n.Outputs[k])
		if err != nil {
			s = map[string]any{}
		}
		props[k] = s
		required = append(required, k)
	}
	outputs := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		outputs["required"] = required
	}
	var outcomes []string
	for _, o := range n.Outcomes {
		if o != "limit" && o != "timeout" {
			outcomes = append(outcomes, o)
		}
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"outcome": map[string]any{"type": "string", "enum": outcomes},
			"summary": map[string]any{"type": "string", "description": "One or two sentences on what happened and why this outcome."},
			"outputs": outputs,
		},
		"required":             []string{"outcome", "summary", "outputs"},
		"additionalProperties": false,
	}
}

func celEnv() (*cel.Env, error) {
	return cel.NewEnv(
		cel.Variable("task", cel.DynType),
		cel.Variable("run", cel.DynType),
		cel.Variable("nodes", cel.DynType),
		cel.Variable("inputs", cel.DynType),
	)
}

// CompileCEL checks that expr is a valid boolean CEL expression.
func CompileCEL(expr string) error {
	if expr == "" {
		return fmt.Errorf("empty expression")
	}
	env, err := celEnv()
	if err != nil {
		return err
	}
	ast, iss := env.Compile(expr)
	if iss != nil && iss.Err() != nil {
		return fmt.Errorf("CEL: %v", iss.Err())
	}
	if t := ast.OutputType(); t != cel.BoolType && t != cel.DynType {
		return fmt.Errorf("CEL: expression must be boolean, got %v", t)
	}
	return nil
}

// EvalCEL evaluates a boolean CEL expression against the run context.
func EvalCEL(expr string, vars map[string]any) (bool, error) {
	env, err := celEnv()
	if err != nil {
		return false, err
	}
	ast, iss := env.Compile(expr)
	if iss != nil && iss.Err() != nil {
		return false, iss.Err()
	}
	prg, err := env.Program(ast)
	if err != nil {
		return false, err
	}
	for _, k := range []string{"task", "run", "nodes", "inputs"} {
		if _, ok := vars[k]; !ok {
			vars[k] = map[string]any{}
		}
	}
	out, _, err := prg.Eval(vars)
	if err != nil {
		return false, err
	}
	b, ok := out.Value().(bool)
	if !ok {
		return false, fmt.Errorf("expression returned %T, not bool", out.Value())
	}
	return b, nil
}
