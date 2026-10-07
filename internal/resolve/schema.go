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

// CheckValue reports whether v (decoded JSON) satisfies schema, as produced by
// OutputSchema: the type keyword, array items, object properties and required.
// Other JSON Schema keywords are not checked.
func CheckValue(schema map[string]any, v any) error {
	typ, _ := schema["type"].(string)
	switch typ {
	case "string":
		if _, ok := v.(string); !ok {
			return fmt.Errorf("want a string, got %s", jsonKind(v))
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("want a bool, got %s", jsonKind(v))
		}
	case "number", "integer":
		f, ok := v.(float64)
		if !ok {
			return fmt.Errorf("want %s, got %s", map[string]string{"number": "a number", "integer": "an integer"}[typ], jsonKind(v))
		}
		if typ == "integer" && f != float64(int64(f)) {
			return fmt.Errorf("want an integer, got %v", f)
		}
	case "array":
		xs, ok := v.([]any)
		if !ok {
			return fmt.Errorf("want a list, got %s", jsonKind(v))
		}
		if items, ok := schema["items"].(map[string]any); ok {
			for i, x := range xs {
				if err := CheckValue(items, x); err != nil {
					return fmt.Errorf("[%d]: %w", i, err)
				}
			}
		}
	case "object":
		m, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("want an object, got %s", jsonKind(v))
		}
		props, _ := schema["properties"].(map[string]any)
		for k, x := range m {
			if p, ok := props[k].(map[string]any); ok {
				if err := CheckValue(p, x); err != nil {
					return fmt.Errorf("%s: %w", k, err)
				}
			}
		}
		req, _ := schema["required"].([]any)
		for _, r := range req {
			if k, _ := r.(string); k != "" {
				if _, ok := m[k]; !ok {
					return fmt.Errorf("missing required %q", k)
				}
			}
		}
	}
	return nil
}

func jsonKind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "a string"
	case bool:
		return "a bool"
	case float64:
		return "a number"
	case []any:
		return "a list"
	case map[string]any:
		return "an object"
	}
	return fmt.Sprintf("%T", v)
}
