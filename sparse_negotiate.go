package sftp

import "github.com/pkg/sftp/internal/sparse"

// sparseExtensionPair returns the ExtensionPair advertising the sparse-file
// extension with the given capabilities. It returns nil when no capabilities
// are available, in which case the extension is simply not advertised and
// peers fall back to ordinary dense transfer.
func sparseExtensionPair(caps sparse.Capability) *extensionPair {
	if caps == 0 {
		return nil
	}
	return &extensionPair{
		Name: sparse.ExtensionName,
		Data: sparse.ExtensionData(caps),
	}
}

// withSparseExtension appends the sparse extension for the client INIT packet.
func withSparseExtension(exts []extensionPair, caps sparse.Capability) []extensionPair {
	for i := range exts {
		if exts[i].Name == sparse.ExtensionName {
			return exts
		}
	}
	if pair := sparseExtensionPair(caps); pair != nil {
		return append(exts, *pair)
	}
	return exts
}

// withSparseVersionExtension appends the sparse extension for the server
// VERSION packet. extensionPair (INIT) and sshExtensionPair (VERSION) are
// distinct named types with the identical {Name, Data string} layout.
func withSparseVersionExtension(exts []sshExtensionPair, caps sparse.Capability) []sshExtensionPair {
	out := make([]sshExtensionPair, 0, len(exts)+1)
	out = append(out, exts...)
	for i := range out {
		if out[i].Name == sparse.ExtensionName {
			return out
		}
	}
	if pair := sparseExtensionPair(caps); pair != nil {
		out = append(out, sshExtensionPair{Name: pair.Name, Data: pair.Data})
	}
	return out
}
