package fastjson_test

import (
	"fmt"
	"log"
	"strings"

	"github.com/valyala/fastjson"
)

func ExampleReaderScanner() {
	var sc fastjson.ReaderScanner

	sc.Init(strings.NewReader(`{"foo": "bar"}[1,2]` + "\n" + `"xyz" 12345 true`))
	sc.MaxValueSize = 1024

	for sc.Next() {
		fmt.Printf("%s\n", sc.Value())
	}
	if err := sc.Error(); err != nil {
		log.Fatalf("unexpected error: %s", err)
	}

	// Output:
	// {"foo":"bar"}
	// [1,2]
	// "xyz"
	// 12345
	// true
}
