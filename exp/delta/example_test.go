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

package delta_test

import (
	"fmt"

	"github.com/prometheus/client_golang/exp/delta"
	dto "github.com/prometheus/client_model/go"
)

// scriptedSource simulates the cumulative values that an /metrics scrape
// would return at successive moments in time.
type scriptedSource struct {
	requests float64
}

func (s *scriptedSource) Gather() ([]*dto.MetricFamily, error) {
	return []*dto.MetricFamily{
		counterFamily("requests_total", map[string]float64{"api": s.requests}),
	}, nil
}

func ExampleReader_twoCadences() {
	source := &scriptedSource{}

	// Two independent readers: one reports every minute, the other every
	// five minutes. Each keeps its own private baseline.
	minuteReader := delta.NewReader(source)
	fiveMinuteReader := delta.NewReader(source)

	// Moment 0: establish both baselines from the cumulative value 100.
	source.requests = 100
	if _, err := minuteReader.Read(); err != nil {
		panic(err)
	}
	if _, err := fiveMinuteReader.Read(); err != nil {
		panic(err)
	}

	// Moment 1: the cumulative counter is now 130.
	source.requests = 130
	minuteReport, _ := minuteReader.Read()
	fmt.Printf("minute reader: %s delta %.0f status %s\n",
		"requests_total",
		findCounter(minuteReport, "requests_total", "api").Delta,
		findCounter(minuteReport, "requests_total", "api").Status)

	// Moment 2: the process restarted and the counter begins at 7 again.
	source.requests = 7
	minuteReport, _ = minuteReader.Read()
	fmt.Printf("minute reader: status %s delta %.0f\n",
		findCounter(minuteReport, "requests_total", "api").Status,
		findCounter(minuteReport, "requests_total", "api").Delta)

	// The slower reader skips moment 1 and sees the restarted value
	// directly. It reports the discontinuity itself, never a negative
	// subtraction.
	source.requests = 22
	fiveMinuteReport, _ := fiveMinuteReader.Read()
	fmt.Printf("five-minute reader: status %s delta %.0f\n",
		findCounter(fiveMinuteReport, "requests_total", "api").Status,
		findCounter(fiveMinuteReport, "requests_total", "api").Delta)

	// Output:
	// minute reader: requests_total delta 30 status ok
	// minute reader: status reset delta 0
	// five-minute reader: status reset delta 0
}
