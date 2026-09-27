package jsonpatch_test

import (
	"encoding/json"
	"fmt"
	"log"

	jsonpatch "github.com/evanphx/json-patch/v5"
)

// A deployment configuration is reordered and updated at the same time:
// "api" moves to the front and gets more replicas, "web" gets a new image,
// "cache" is removed and "queue" is added. Because /services is registered
// with the identity key "name", the patch expresses the reordering with a
// single move instead of replacing the whole array.
func ExampleCreatePatch() {
	original := []byte(`{
		"services": [
			{"name": "web",   "replicas": 3, "image": "web:1.0"},
			{"name": "api",   "replicas": 2, "image": "api:1.4"},
			{"name": "cache", "replicas": 1, "image": "redis:7"}
		]
	}`)
	modified := []byte(`{
		"services": [
			{"name": "api",   "replicas": 5, "image": "api:1.4"},
			{"name": "web",   "replicas": 3, "image": "web:2.0"},
			{"name": "queue", "replicas": 1, "image": "mq:3.2"}
		]
	}`)

	patch, err := jsonpatch.CreatePatch(original, modified,
		jsonpatch.WithArrayIdentity("/services", "name"))
	if err != nil {
		log.Fatal(err)
	}

	out, err := json.MarshalIndent(patch, "", "  ")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(string(out))

	// Output:
	// [
	//   {
	//     "op": "remove",
	//     "path": "/services/2"
	//   },
	//   {
	//     "from": "/services/1",
	//     "op": "move",
	//     "path": "/services/0"
	//   },
	//   {
	//     "op": "add",
	//     "path": "/services/2",
	//     "value": {
	//       "image": "mq:3.2",
	//       "name": "queue",
	//       "replicas": 1
	//     }
	//   },
	//   {
	//     "op": "replace",
	//     "path": "/services/1/image",
	//     "value": "web:2.0"
	//   },
	//   {
	//     "op": "replace",
	//     "path": "/services/0/replicas",
	//     "value": 5
	//   }
	// ]
}
