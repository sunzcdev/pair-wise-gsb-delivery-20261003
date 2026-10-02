// Package host defines the host environment shared by the rule service's
// two processes: the save process compiles rules against it, and the load
// process re-binds loaded programs to it.
package host

import (
	"fmt"
	"net"
	"strings"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/builtin"
	"github.com/expr-lang/expr/conf"
)

// Request is the incoming request the rules are evaluated against.
type Request struct {
	IP     string `expr:"ip"`
	Method string `expr:"method"`
	Path   string `expr:"path"`
}

// User is the authenticated principal.
type User struct {
	Role string `expr:"role"`
}

// Env is the rule evaluation environment.
type Env struct {
	Request Request `expr:"request"`
	User    User    `expr:"user"`
}

// IsInternalIP reports whether the given IP belongs to the internal network.
func IsInternalIP(params ...any) (any, error) {
	ip, ok := params[0].(string)
	if !ok {
		return nil, fmt.Errorf("isInternalIP: expected a string argument")
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false, nil
	}
	return parsed.IsPrivate() || parsed.IsLoopback(), nil
}

// RiskScore returns a static risk score for an IP address.
func RiskScore(params ...any) (any, error) {
	ip, ok := params[0].(string)
	if !ok {
		return nil, fmt.Errorf("riskScore: expected a string argument")
	}
	if strings.HasPrefix(ip, "10.") {
		return 10, nil
	}
	return 80, nil
}

// Options returns the expr options both processes compile/configure with.
func Options() []expr.Option {
	return []expr.Option{
		expr.Env(Env{}),
		expr.Function("isInternalIP", IsInternalIP, new(func(string) bool)),
		expr.Function("riskScore", RiskScore, new(func(string) int)),
	}
}

// Functions returns the host function table used to re-bind loaded programs.
func Functions() map[string]*builtin.Function {
	c := conf.CreateNew()
	for _, opt := range Options() {
		opt(c)
	}
	return c.Functions
}

// FunctionsV2 simulates a host deployment in which the signature of
// riskScore changed after the rules were compiled. Programs loaded against
// this table must be rejected before execution.
func FunctionsV2() map[string]*builtin.Function {
	c := conf.CreateNew()
	expr.Env(Env{})(c)
	expr.Function("isInternalIP", IsInternalIP, new(func(string) bool))(c)
	expr.Function("riskScore", RiskScore, new(func(string, int) int))(c)
	return c.Functions
}
