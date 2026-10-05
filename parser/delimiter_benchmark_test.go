package parser

import "testing"

var benchmarkSource = []byte("***foo*** and **bar** ~~strike~~ *a **b** c* `code` [link](http://example.com)")

func BenchmarkDelimiterTraceOff(b *testing.B) {
	p := New()
	for b.Loop() {
		p.Parse(benchmarkSource)
	}
}

func BenchmarkDelimiterTraceOn(b *testing.B) {
	p := New(WithDelimiterTrace(func(DelimiterTraceEvent) {}))
	for b.Loop() {
		p.Parse(benchmarkSource)
	}
}
