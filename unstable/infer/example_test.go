package infer_test

import (
	"fmt"
	"strings"

	"github.com/pelletier/go-toml/v2/unstable/infer"
)

// ExampleSchema shows a contract inferred from two example config files. The
// samples together cover nested objects with missing fields, heterogeneous
// arrays, a quoted key containing a dot, and all four TOML temporal kinds.
func ExampleSchema() {
	sampleA := strings.NewReader(`
name = "edge"
"a.b" = "quoted"
tags = ["x", 1]
events = []

[owner]
email = "ops@example.test"

[[servers]]
name = "a"
port = 8080

[times]
offset = 1985-04-12T23:20:50-07:00
localdt = 1985-04-12T23:20:50.52
date = 1985-04-12
clock = 23:20:50.52
`)

	sampleB := strings.NewReader(`
name = "core"
"a.b" = "dotted"
tags = ["y"]

[owner]
name = "Ada"

[[servers]]
name = "b"

[[servers]]
port = 8081

[times]
offset = 1985-04-13T00:20:50+00:00
localdt = 1985-04-13T00:20:50
date = 1985-04-13
clock = 00:20:50
`)

	schema, err := infer.Schema(sampleA, sampleB)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Print(string(schema))

	// Output:
	// {
	//   "$schema": "https://json-schema.org/draft/2020-12/schema",
	//   "properties": {
	//     "a.b": {
	//       "type": "string"
	//     },
	//     "events": {
	//       "type": "array"
	//     },
	//     "name": {
	//       "type": "string"
	//     },
	//     "owner": {
	//       "properties": {
	//         "email": {
	//           "type": "string"
	//         },
	//         "name": {
	//           "type": "string"
	//         }
	//       },
	//       "type": "object"
	//     },
	//     "servers": {
	//       "items": {
	//         "properties": {
	//           "name": {
	//             "type": "string"
	//           },
	//           "port": {
	//             "type": "integer"
	//           }
	//         },
	//         "type": "object"
	//       },
	//       "type": "array"
	//     },
	//     "tags": {
	//       "items": {
	//         "anyOf": [
	//           {
	//             "type": "integer"
	//           },
	//           {
	//             "type": "string"
	//           }
	//         ]
	//       },
	//       "type": "array"
	//     },
	//     "times": {
	//       "properties": {
	//         "clock": {
	//           "type": "string",
	//           "x-toml-type": "local-time"
	//         },
	//         "date": {
	//           "type": "string",
	//           "x-toml-type": "local-date"
	//         },
	//         "localdt": {
	//           "type": "string",
	//           "x-toml-type": "local-date-time"
	//         },
	//         "offset": {
	//           "type": "string",
	//           "x-toml-type": "offset-date-time"
	//         }
	//       },
	//       "required": [
	//         "clock",
	//         "date",
	//         "localdt",
	//         "offset"
	//       ],
	//       "type": "object"
	//     }
	//   },
	//   "required": [
	//     "a.b",
	//     "name",
	//     "owner",
	//     "servers",
	//     "tags",
	//     "times"
	//   ],
	//   "type": "object"
	// }
}
