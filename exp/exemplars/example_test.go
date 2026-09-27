// Copyright 2026 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package exemplars_test

import (
	"fmt"
	"math/rand"
	"time"

	"github.com/prometheus/client_golang/exp/exemplars"
)

// ExampleReservoir retains a bounded pool of observations from the last
// minute and draws two uniformly sampled exemplars from it. The seeded
// random source makes the selection reproducible.
func ExampleReservoir() {
	r := exemplars.NewReservoir(exemplars.Config{
		Capacity:     16,
		Window:       time.Minute,
		MaxExemplars: 2,
		Rand:         rand.New(rand.NewSource(42)),
	})
	for i := 0; i < 4; i++ {
		r.Observe(exemplars.Observation{
			Value:  float64(i) * 0.1,
			Labels: map[string]string{"trace_id": fmt.Sprintf("trace-%d", i)},
		})
	}
	for _, e := range r.Exemplars() {
		fmt.Printf("value=%.1f trace_id=%s\n", e.Value, e.Labels["trace_id"])
	}
	// Output:
	// value=0.1 trace_id=trace-1
	// value=0.3 trace_id=trace-3
}
