// Package rulesdemo defines the rule expressions and the stable host
// environment shared by the independent saver and loader processes.
package rulesdemo

import "github.com/expr-lang/expr"

// Env is the structured input every rule is evaluated against.
type Env struct {
	UserID int
	Role   string
	Score  int
	Tags   []string
}

// Rules are the expressions compiled once and (in a real service) reloaded on
// every restart instead of being recompiled.
var Rules = map[string]string{
	"vip":        `Role == "admin" || Score >= 90`,
	"tagged":     `len(Tags) > 0 && Score >= discount(10)`,
	"newcomer":   `UserID < 1000 && Role matches "user|guest"`,
	"high-score": `len(Tags) > 1 ? Score * 2 : Score`,
}

// Hosts returns the host function bindings keyed by stable symbol name.
// Signature must match the signature used when the rules were compiled.
func Hosts() expr.Hosts {
	return expr.Hosts{
		"discount": {
			Fn:        discount,
			Signature: "func(int) int",
		},
	}
}

func discount(args ...any) (any, error) {
	return args[0].(int), nil
}

// CompileOptions are the options both processes must agree on.
func CompileOptions() []expr.Option {
	return []expr.Option{
		expr.Env(Env{}),
		expr.Function("discount", discount, new(func(int) int)),
	}
}
