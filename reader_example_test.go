package fastjson_test

import (
	"fmt"
	"log"
	"strings"

	"github.com/valyala/fastjson"
)

func ExampleReader() {
	var r fastjson.Reader

	r.Init(strings.NewReader(`{"foo":  "bar"  }[  ]
		12345"xyz" true false null    `))

	for r.Next() {
		fmt.Printf("%s\n", r.Value())
	}
	if err := r.Error(); err != nil {
		log.Fatalf("unexpected error: %s", err)
	}

	// Output:
	// {"foo":"bar"}
	// []
	// 12345
	// "xyz"
	// true
	// false
	// null
}
