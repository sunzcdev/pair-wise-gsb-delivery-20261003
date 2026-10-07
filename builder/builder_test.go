package builder

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/mna/pigeon/bootstrap"
)

var grammar = `
{
var test = "some string"

func init() {
	fmt.Println("this is inside the init")
}
}

start = additive eof
additive = left:multiplicative "+" space right:additive {
	fmt.Println(left, right)
} / mul:multiplicative { fmt.Println(mul) }
multiplicative = left:primary op:"*" space right:multiplicative { fmt.Println(left, right, op) } / primary
primary = integer / "(" space additive:additive ")" space { fmt.Println(additive) }
integer "integer" = digits:[0123456789]+ space { fmt.Println(digits) }
space = ' '*
eof = !. { fmt.Println("eof") }
`

func TestBuildParser(t *testing.T) {
	p := bootstrap.NewParser()
	g, err := p.Parse("", strings.NewReader(grammar))
	if err != nil {
		t.Fatal(err)
	}
	if err := BuildParser(io.Discard, g); err != nil {
		t.Fatal(err)
	}
}

func TestBuildDelayedActionFlag(t *testing.T) {
	const grammar = `
start = "a" ~{ return "x", nil }
`
	p := bootstrap.NewParser()
	g, err := p.Parse("", strings.NewReader(grammar))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := BuildParser(&buf, g); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "deferred: true") {
		t.Fatal("generated parser is missing the deferred: true action flag")
	}
	if !strings.Contains(buf.String(), "registerDeferred") {
		t.Fatal("generated parser is missing the deferred action runtime")
	}
}
