package gjson

import "testing"

var benchmarkSink Result

func BenchmarkGetComplex(b *testing.B) {
	path := `friends.#(nets.#(=="fb"))#.first`
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		benchmarkSink = Get(diffJSON, path)
	}
}

func BenchmarkPlanComplex(b *testing.B) {
	path := `friends.#(nets.#(=="fb"))#.first`
	plan, err := Compile(path)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		benchmarkSink = plan.Get(diffJSON)
	}
}

func BenchmarkGetSimple(b *testing.B) {
	path := `friends.1.nets.0`
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		benchmarkSink = Get(diffJSON, path)
	}
}

func BenchmarkPlanSimple(b *testing.B) {
	path := `friends.1.nets.0`
	plan, err := Compile(path)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		benchmarkSink = plan.Get(diffJSON)
	}
}

func BenchmarkGetQueryOne(b *testing.B) {
	path := `friends.#(age==68).first`
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		benchmarkSink = Get(diffJSON, path)
	}
}

func BenchmarkPlanQueryOne(b *testing.B) {
	path := `friends.#(age==68).first`
	plan, err := Compile(path)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		benchmarkSink = plan.Get(diffJSON)
	}
}

func buildLargeJSON(n int) string {
	s := `{"items":[`
	for i := 0; i < n; i++ {
		if i > 0 {
			s += ","
		}
		s += `{"id":` + itoa(i) + `,"meta":{"tags":["a","b"],"active":true},"name":"item"}`
	}
	return s + "]}"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func BenchmarkGetLarge(b *testing.B) {
	j := buildLargeJSON(2000)
	path := `items.#(meta.tags.1=="b")#.name`
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		benchmarkSink = Get(j, path)
	}
}

func BenchmarkPlanLarge(b *testing.B) {
	j := buildLargeJSON(2000)
	path := `items.#(meta.tags.1=="b")#.name`
	plan, err := Compile(path)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		benchmarkSink = plan.Get(j)
	}
}
