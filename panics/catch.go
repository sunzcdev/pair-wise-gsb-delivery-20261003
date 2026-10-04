package panics

// nilPanicValue is how newer Go runtimes describe a panic(nil).
const nilPanicValue = "panic called with nil argument"

// nilPanic is the sentinel used to distinguish panic(nil) from a goroutine
// that did not panic. Under the go1.20 language version, recover() returns nil
// for a panic(nil); the only way to detect one on those toolchains is to
// "re-panic" inside a nested deferred call and detect that sentinel from an
// outer deferred call. On go1.21 and newer, recover() returns a non-nil
// *runtime.PanicNilError, so the sentinel path is not needed.
type nilPanic struct{}

// String implements fmt.Stringer so that a recovered panic(nil) renders the
// same way it would with a runtime-generated value.
func (nilPanic) String() string { return nilPanicValue }

// Catch executes f, catching any panic it might spawn (including panic(nil))
// and returning a *Recovered describing it. It is safe to call from multiple
// goroutines simultaneously.
//
// If f returns normally, Catch returns nil. If f terminates the goroutine with
// runtime.Goexit, Catch does not return; like any other deferred code it runs
// while the goroutine unwinds, but no *Recovered is produced.
func Catch(f func()) (recovered *Recovered) {
	defer func() {
		// Swallow the panic re-thrown by catch so that Catch returns. Non-nil
		// panics and the nilPanic sentinel have already been recorded in the
		// named return value. A runtime.Goexit unwind is not recoverable:
		// recover() returns nil and the goroutine keeps unwinding without ever
		// returning the recorded value.
		_ = recover()
	}()

	catch(f, &recovered)
	return recovered
}

func catch(f func(), recovered **Recovered) {
	completed := false
	defer func() {
		val := recover()
		if completed {
			// f returned normally, there is nothing to record.
			return
		}

		// f did not return normally. With a non-nil value this is a regular
		// panic; with a nil value it is either panic(nil) or runtime.Goexit.
		// Record either way - if this turns out to be a runtime.Goexit, Catch
		// never resumes and the record is discarded along with the goroutine.
		rp := NewRecovered(2, val)
		*recovered = &rp

		if val == nil {
			// Mark the unwind with an unforgeable sentinel so the outer
			// deferred call in Catch can tell panic(nil) from a normal
			// return. For runtime.Goexit the sentinel is swallowed (and
			// ignored) while the goroutine keeps unwinding.
			panic(nilPanic{})
		}

		// Keep unwinding with the original panic value. The outer deferred
		// call in Catch swallows it after it has been recorded.
		panic(val)
	}()
	f()
	completed = true
}
