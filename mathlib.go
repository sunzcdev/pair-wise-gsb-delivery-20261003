package lua

import (
	"math"
	"math/bits"
	"math/rand"
	"sync"
)

const (
	maxInt64RandomFloat = 9223372036854774784.0
)

type stateRandom struct {
	mu     sync.Mutex
	source rand.Source
}

func newStateRandom(source rand.Source) *stateRandom {
	return &stateRandom{source: source}
}

func (r *stateRandom) seed(seed int64) {
	r.mu.Lock()
	r.source.Seed(seed)
	r.mu.Unlock()
}

func (r *stateRandom) int63() int64 {
	return r.source.Int63()
}

func (r *stateRandom) uint64() uint64 {
	if source, ok := r.source.(rand.Source64); ok {
		return source.Uint64()
	}
	return uint64(r.int63())>>31 | uint64(r.int63())<<32
}

func (r *stateRandom) int63n(n int64) int64 {
	if n&(n-1) == 0 {
		return r.int63() & (n - 1)
	}
	max := int64((1 << 63) - 1 - (1<<63)%uint64(n))
	value := r.int63()
	for value > max {
		value = r.int63()
	}
	return value % n
}

func (r *stateRandom) uint64n(n uint64) uint64 {
	if n <= math.MaxInt64 {
		return uint64(r.int63n(int64(n)))
	}
	if n&(n-1) == 0 {
		return r.uint64() & (n - 1)
	}
	hi, lo := bits.Mul64(r.uint64(), n)
	if lo < n {
		threshold := uint64(0) - n
		threshold %= n
		for lo < threshold {
			hi, lo = bits.Mul64(r.uint64(), n)
		}
	}
	return hi
}

func (r *stateRandom) float64() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()

again:
	value := float64(r.int63()) / (1 << 63)
	if value == 1 {
		goto again
	}
	return value
}

func (r *stateRandom) interval(low, high int64) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()

	if low == math.MinInt64 && high == math.MaxInt64 {
		return int64(r.uint64())
	}
	span := uint64(high-low) + 1
	return low + int64(r.uint64n(span))
}

func checkRandomInt(L *LState, n int) int64 {
	value, ok := L.Get(n).(LNumber)
	if !ok {
		L.TypeError(n, LTNumber)
		return 0
	}
	number := float64(value)
	if math.IsNaN(number) || math.IsInf(number, 0) || number < float64(math.MinInt64) || number > maxInt64RandomFloat {
		L.ArgError(n, "integer has no valid representation")
		return 0
	}
	return int64(number)
}

func OpenMath(L *LState) int {
	mod := L.RegisterModule(MathLibName, mathFuncs).(*LTable)
	mod.RawSetString("pi", LNumber(math.Pi))
	mod.RawSetString("huge", LNumber(math.MaxFloat64))
	L.Push(mod)
	return 1
}

var mathFuncs = map[string]LGFunction{
	"abs":        mathAbs,
	"acos":       mathAcos,
	"asin":       mathAsin,
	"atan":       mathAtan,
	"atan2":      mathAtan2,
	"ceil":       mathCeil,
	"cos":        mathCos,
	"cosh":       mathCosh,
	"deg":        mathDeg,
	"exp":        mathExp,
	"floor":      mathFloor,
	"fmod":       mathFmod,
	"frexp":      mathFrexp,
	"ldexp":      mathLdexp,
	"log":        mathLog,
	"log10":      mathLog10,
	"max":        mathMax,
	"min":        mathMin,
	"mod":        mathMod,
	"modf":       mathModf,
	"pow":        mathPow,
	"rad":        mathRad,
	"random":     mathRandom,
	"randomseed": mathRandomseed,
	"sin":        mathSin,
	"sinh":       mathSinh,
	"sqrt":       mathSqrt,
	"tan":        mathTan,
	"tanh":       mathTanh,
}

func mathAbs(L *LState) int {
	L.Push(LNumber(math.Abs(float64(L.CheckNumber(1)))))
	return 1
}

func mathAcos(L *LState) int {
	L.Push(LNumber(math.Acos(float64(L.CheckNumber(1)))))
	return 1
}

func mathAsin(L *LState) int {
	L.Push(LNumber(math.Asin(float64(L.CheckNumber(1)))))
	return 1
}

func mathAtan(L *LState) int {
	L.Push(LNumber(math.Atan(float64(L.CheckNumber(1)))))
	return 1
}

func mathAtan2(L *LState) int {
	L.Push(LNumber(math.Atan2(float64(L.CheckNumber(1)), float64(L.CheckNumber(2)))))
	return 1
}

func mathCeil(L *LState) int {
	L.Push(LNumber(math.Ceil(float64(L.CheckNumber(1)))))
	return 1
}

func mathCos(L *LState) int {
	L.Push(LNumber(math.Cos(float64(L.CheckNumber(1)))))
	return 1
}

func mathCosh(L *LState) int {
	L.Push(LNumber(math.Cosh(float64(L.CheckNumber(1)))))
	return 1
}

func mathDeg(L *LState) int {
	L.Push(LNumber(float64(L.CheckNumber(1)) * 180 / math.Pi))
	return 1
}

func mathExp(L *LState) int {
	L.Push(LNumber(math.Exp(float64(L.CheckNumber(1)))))
	return 1
}

func mathFloor(L *LState) int {
	L.Push(LNumber(math.Floor(float64(L.CheckNumber(1)))))
	return 1
}

func mathFmod(L *LState) int {
	L.Push(LNumber(math.Mod(float64(L.CheckNumber(1)), float64(L.CheckNumber(2)))))
	return 1
}

func mathFrexp(L *LState) int {
	v1, v2 := math.Frexp(float64(L.CheckNumber(1)))
	L.Push(LNumber(v1))
	L.Push(LNumber(v2))
	return 2
}

func mathLdexp(L *LState) int {
	L.Push(LNumber(math.Ldexp(float64(L.CheckNumber(1)), L.CheckInt(2))))
	return 1
}

func mathLog(L *LState) int {
	L.Push(LNumber(math.Log(float64(L.CheckNumber(1)))))
	return 1
}

func mathLog10(L *LState) int {
	L.Push(LNumber(math.Log10(float64(L.CheckNumber(1)))))
	return 1
}

func mathMax(L *LState) int {
	if L.GetTop() == 0 {
		L.RaiseError("wrong number of arguments")
	}
	max := L.CheckNumber(1)
	top := L.GetTop()
	for i := 2; i <= top; i++ {
		v := L.CheckNumber(i)
		if v > max {
			max = v
		}
	}
	L.Push(max)
	return 1
}

func mathMin(L *LState) int {
	if L.GetTop() == 0 {
		L.RaiseError("wrong number of arguments")
	}
	min := L.CheckNumber(1)
	top := L.GetTop()
	for i := 2; i <= top; i++ {
		v := L.CheckNumber(i)
		if v < min {
			min = v
		}
	}
	L.Push(min)
	return 1
}

func mathMod(L *LState) int {
	lhs := L.CheckNumber(1)
	rhs := L.CheckNumber(2)
	L.Push(luaModulo(lhs, rhs))
	return 1
}

func mathModf(L *LState) int {
	v1, v2 := math.Modf(float64(L.CheckNumber(1)))
	L.Push(LNumber(v1))
	L.Push(LNumber(v2))
	return 2
}

func mathPow(L *LState) int {
	L.Push(LNumber(math.Pow(float64(L.CheckNumber(1)), float64(L.CheckNumber(2)))))
	return 1
}

func mathRad(L *LState) int {
	L.Push(LNumber(float64(L.CheckNumber(1)) * math.Pi / 180))
	return 1
}

func mathRandom(L *LState) int {
	switch L.GetTop() {
	case 0:
		L.Push(LNumber(L.G.rand.float64()))
	case 1:
		n := checkRandomInt(L, 1)
		if n < 1 {
			L.ArgError(1, "interval is empty")
			return 0
		}
		L.Push(LNumber(L.G.rand.interval(1, n)))
	default:
		min := checkRandomInt(L, 1)
		max := checkRandomInt(L, 2)
		if min > max {
			L.ArgError(1, "interval is empty")
			return 0
		}
		L.Push(LNumber(L.G.rand.interval(min, max)))
	}
	return 1
}

func mathRandomseed(L *LState) int {
	L.G.rand.seed(checkRandomInt(L, 1))
	return 0
}

func mathSin(L *LState) int {
	L.Push(LNumber(math.Sin(float64(L.CheckNumber(1)))))
	return 1
}

func mathSinh(L *LState) int {
	L.Push(LNumber(math.Sinh(float64(L.CheckNumber(1)))))
	return 1
}

func mathSqrt(L *LState) int {
	L.Push(LNumber(math.Sqrt(float64(L.CheckNumber(1)))))
	return 1
}

func mathTan(L *LState) int {
	L.Push(LNumber(math.Tan(float64(L.CheckNumber(1)))))
	return 1
}

func mathTanh(L *LState) int {
	L.Push(LNumber(math.Tanh(float64(L.CheckNumber(1)))))
	return 1
}

//
