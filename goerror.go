package lua

// RaiseGoError raises a protected Lua error that carries err as a Go
// error chain. Call it from a registered Go function instead of
// RaiseError when the host must be able to inspect the failure with
// errors.Is or errors.As on the value returned by PCall, GPCall or
// CallByParam.
//
// Inside Lua the raised value behaves like any other error object:
// pcall and xpcall propagate it untouched and tostring reports
// err.Error(). The error chain survives nested protected calls, but it
// is severed as soon as the script replaces the value, for example by
// passing a plain string to the Lua error function.
func (ls *LState) RaiseGoError(err error) {
	ls.Error(NewGoError(err), 0)
}

// Unwrap exposes the error chain carried by an LGoError object. Errors
// reported for syntax or file failures still unwrap to their original
// Go cause. Plain Lua errors, including strings, carry no Go chain.
func (e *ApiError) Unwrap() error {
	if gerr, ok := e.Object.(*LGoError); ok {
		return gerr
	}
	return e.Cause
}
