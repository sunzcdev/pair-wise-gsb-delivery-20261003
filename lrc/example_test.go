package lrc_test

import (
	"encoding/json"
	"fmt"
	"math/rand"

	"github.com/klauspost/reedsolomon/lrc"
)

// ExampleLayout shows saving the layout parameters as metadata, encoding an
// object, planning a single-shard repair, and the read-count saving compared
// with a flat MDS stripe.
func ExampleLayout() {
	// k=6 data shards, l=3 groups (2 data shards each), p=1 local parity
	// per group, r=2 global parity: 11 shards total.
	ly, err := lrc.NewLayout(6, 3, 1, 2)
	if err != nil {
		panic(err)
	}

	// Persist the layout parameters next to the object.
	meta, _ := json.Marshal(ly.Params())
	fmt.Printf("metadata: %s\n", meta)

	// Restore the layout from the saved metadata.
	var saved lrc.Params
	_ = json.Unmarshal(meta, &saved)
	restored, err := lrc.NewLayoutFromParams(saved)
	if err != nil {
		panic(err)
	}

	rng := rand.New(rand.NewSource(1))
	shards := make([][]byte, restored.Shards())
	for i := 0; i < restored.DataShards(); i++ {
		shards[i] = make([]byte, 128)
		rng.Read(shards[i])
	}
	for i := restored.DataShards(); i < restored.Shards(); i++ {
		shards[i] = make([]byte, 128)
	}
	if err := restored.Encode(shards); err != nil {
		panic(err)
	}

	// Lose a single data shard and ask which shards to read.
	shards[0] = nil
	plan, err := restored.RepairPlan(shards)
	if err != nil {
		panic(err)
	}
	for _, step := range plan.Steps {
		fmt.Printf("repair tier=%s: restore shard %v by reading shard %v\n",
			step.Tier, step.Missing, step.ReadShards)
	}
	fmt.Printf("LRC total shards read: %d\n", plan.ReadCount)
	fmt.Printf("flat (6,2) MDS would read: %d\n", restored.DataShards())
	fmt.Printf("reads saved: %d\n", restored.DataShards()-plan.ReadCount)

	if err := restored.Reconstruct(shards); err != nil {
		panic(err)
	}
	fmt.Println("repair succeeded")

	// Output:
	// metadata: {"type":"lrc/v1","data_shards":6,"groups":3,"local_parity":1,"global_parity":2}
	// repair tier=local: restore shard [0] by reading shard [1 6]
	// LRC total shards read: 2
	// flat (6,2) MDS would read: 6
	// reads saved: 4
	// repair succeeded
}
