package gobreaker

// Counts holds the numbers of requests and their successes/failures/exclusions.
// CircuitBreaker clears the internal Counts either
// on the change of the state or at the closed-state intervals.
// Counts ignores the results of the requests sent before clearing.
type Counts struct {
	Requests             uint32
	TotalSuccesses       uint32
	TotalFailures        uint32
	TotalExclusions      uint32
	ConsecutiveSuccesses uint32
	ConsecutiveFailures  uint32
}

func (c *Counts) onRequest() {
	c.Requests++
}

func (c *Counts) onSuccess() {
	c.TotalSuccesses++
	c.ConsecutiveSuccesses++
	c.ConsecutiveFailures = 0
}

func (c *Counts) onFailure() {
	c.TotalFailures++
	c.ConsecutiveFailures++
	c.ConsecutiveSuccesses = 0
}

func (c *Counts) onExclusion() {
	c.TotalExclusions++
}

func (c *Counts) validRequests() uint32 {
	if c.Requests < c.TotalExclusions {
		return 0
	}
	return c.Requests - c.TotalExclusions
}

func (c *Counts) clear() {
	c.Requests = 0
	c.TotalSuccesses = 0
	c.TotalFailures = 0
	c.TotalExclusions = 0
	c.ConsecutiveSuccesses = 0
	c.ConsecutiveFailures = 0
}

type rollingCounts struct {
	Counts

	age       uint64
	buckets   []Counts
	legacy    Counts
	hasLegacy bool
}

func newRollingCounts(numBuckets int64) *rollingCounts {
	if numBuckets < 0 {
		numBuckets = 0
	}
	return &rollingCounts{
		buckets: make([]Counts, numBuckets),
	}
}

func (rc *rollingCounts) index(age uint64) uint64 {
	if len(rc.buckets) == 0 {
		return 0
	}
	return age % uint64(len(rc.buckets))
}

func (rc *rollingCounts) current() uint64 {
	return rc.index(rc.age)
}

func (rc *rollingCounts) onRequest() {
	rc.Counts.onRequest()
	if len(rc.buckets) == 0 {
		return
	}
	rc.buckets[rc.current()].onRequest()
}

func (rc *rollingCounts) onSuccess(age uint64) {
	if len(rc.buckets) == 0 {
		rc.Counts.onSuccess()
		return
	}
	if age > rc.age {
		return
	}

	if rc.age-age < uint64(len(rc.buckets)) {
		rc.Counts.onSuccess()
		rc.buckets[rc.index(age)].onSuccess()
	}
}

func (rc *rollingCounts) onFailure(age uint64) {
	if len(rc.buckets) == 0 {
		rc.Counts.onFailure()
		return
	}
	if age > rc.age {
		return
	}

	if rc.age-age < uint64(len(rc.buckets)) {
		rc.Counts.onFailure()
		rc.buckets[rc.index(age)].onFailure()
	}
}

func (rc *rollingCounts) onExclusion(age uint64) {
	if len(rc.buckets) == 0 {
		rc.Counts.onExclusion()
		return
	}
	if age > rc.age {
		return
	}

	if rc.age-age < uint64(len(rc.buckets)) {
		rc.Counts.onExclusion()
		rc.buckets[rc.index(age)].onExclusion()
	}
}

func (rc *rollingCounts) removeRequestsAt(age uint64, count uint32) {
	if len(rc.buckets) == 0 {
		if count > rc.Requests {
			count = rc.Requests
		}
		rc.Requests -= count
		return
	}

	if age > rc.age || rc.age-age >= uint64(len(rc.buckets)) {
		return
	}

	index := rc.index(age)
	bucketCount := rc.buckets[index].Requests
	if count > bucketCount {
		count = bucketCount
	}
	rc.buckets[index].Requests -= count

	totalCount := rc.Requests
	if count > totalCount {
		count = totalCount
	}
	rc.Requests -= count
}

func (rc *rollingCounts) clear() {
	rc.Counts.clear()

	rc.age = 0
	rc.legacy.clear()
	rc.hasLegacy = false

	for i := range rc.buckets {
		rc.buckets[i].clear()
	}
}

func (rc *rollingCounts) carry(counts Counts) {
	rc.Counts = counts
	if counts != (Counts{}) {
		rc.legacy = counts
		rc.hasLegacy = true
	}
}

func (rc *rollingCounts) roll() {
	rc.age++
	if len(rc.buckets) == 0 {
		return
	}

	if rc.hasLegacy && rc.age >= uint64(len(rc.buckets)) {
		rc.subtractLegacy()
		rc.legacy.clear()
		rc.hasLegacy = false
	}

	current := rc.current()
	rc.subtract(current)
	rc.buckets[current].clear()
}

func (rc *rollingCounts) subtract(oldest uint64) {
	length := uint64(len(rc.buckets))
	if length == 0 {
		return
	}

	oldest = oldest % length
	bucket := rc.buckets[oldest]

	totalSuccesses := bucket.ConsecutiveSuccesses
	for i := uint64(1); i < length; i++ {
		idx := (oldest + i) % length
		totalSuccesses += rc.buckets[idx].TotalSuccesses
	}
	if rc.ConsecutiveSuccesses == totalSuccesses {
		if rc.ConsecutiveSuccesses > bucket.ConsecutiveSuccesses {
			rc.ConsecutiveSuccesses -= bucket.ConsecutiveSuccesses
		} else {
			rc.ConsecutiveSuccesses = 0
		}
	}

	totalFailures := bucket.ConsecutiveFailures
	for i := uint64(1); i < length; i++ {
		idx := (oldest + i) % length
		totalFailures += rc.buckets[idx].TotalFailures
	}
	if rc.ConsecutiveFailures == totalFailures {
		if rc.ConsecutiveFailures > bucket.ConsecutiveFailures {
			rc.ConsecutiveFailures -= bucket.ConsecutiveFailures
		} else {
			rc.ConsecutiveFailures = 0
		}
	}

	rc.subtractTotals(bucket)
}

func (rc *rollingCounts) subtractLegacy() {
	length := uint64(len(rc.buckets))
	if length == 0 {
		rc.subtractTotals(rc.legacy)
		return
	}

	totalSuccesses := rc.legacy.ConsecutiveSuccesses
	for i := uint64(0); i < length; i++ {
		totalSuccesses += rc.buckets[i].TotalSuccesses
	}
	if rc.ConsecutiveSuccesses == totalSuccesses {
		if rc.ConsecutiveSuccesses > rc.legacy.ConsecutiveSuccesses {
			rc.ConsecutiveSuccesses -= rc.legacy.ConsecutiveSuccesses
		} else {
			rc.ConsecutiveSuccesses = 0
		}
	}

	totalFailures := rc.legacy.ConsecutiveFailures
	for i := uint64(0); i < length; i++ {
		totalFailures += rc.buckets[i].TotalFailures
	}
	if rc.ConsecutiveFailures == totalFailures {
		if rc.ConsecutiveFailures > rc.legacy.ConsecutiveFailures {
			rc.ConsecutiveFailures -= rc.legacy.ConsecutiveFailures
		} else {
			rc.ConsecutiveFailures = 0
		}
	}

	rc.subtractTotals(rc.legacy)
}

func (rc *rollingCounts) subtractTotals(bucket Counts) {
	if rc.Requests > bucket.Requests {
		rc.Requests -= bucket.Requests
	} else {
		rc.Requests = 0
	}

	if rc.TotalSuccesses > bucket.TotalSuccesses {
		rc.TotalSuccesses -= bucket.TotalSuccesses
	} else {
		rc.TotalSuccesses = 0
	}

	if rc.TotalFailures > bucket.TotalFailures {
		rc.TotalFailures -= bucket.TotalFailures
	} else {
		rc.TotalFailures = 0
	}

	if rc.TotalExclusions > bucket.TotalExclusions {
		rc.TotalExclusions -= bucket.TotalExclusions
	} else {
		rc.TotalExclusions = 0
	}
}

func (rc *rollingCounts) grow(age uint64) {
	if age <= rc.age {
		return
	}

	diff := age - rc.age
	if diff >= uint64(len(rc.buckets)) {
		rc.clear()
		rc.age = age
	} else {
		for range diff {
			rc.roll()
		}
	}
}

func (rc *rollingCounts) bucketAt(index int) Counts {
	bucketLen := len(rc.buckets)
	if bucketLen == 0 {
		return Counts{}
	}

	idx := (index%bucketLen + bucketLen) % bucketLen
	if idx < 0 {
		return Counts{}
	}

	bucketIndex := (rc.current() + uint64(idx)) % uint64(bucketLen)
	return rc.buckets[bucketIndex]
}
