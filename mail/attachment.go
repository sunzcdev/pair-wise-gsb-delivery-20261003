package mail

import (
	"github.com/emersion/go-message"
)

// An AttachmentHeader represents an attachment's header.
type AttachmentHeader struct {
	message.Header
}

var _ PartHeader = (*AttachmentHeader)(nil)

func (*AttachmentHeader) partHeader() {}

// Filename parses the attachment's filename.
func (h *AttachmentHeader) Filename() (string, error) {
	_, params, err := h.ContentDisposition()

	filename, ok := params["filename"]
	if !ok {
		// Using "name" in Content-Type is discouraged
		_, params, err = h.ContentType()
		filename = params["name"]
	}

	return filename, err
}

// SetFilename formats the attachment's filename.
//
// Non-ASCII filenames are encoded using RFC 2231 and split across
// continuation lines according to the parameter line length budget, which can
// be configured via message.WithParameterLineLen. A plain ASCII fallback
// filename is emitted as well.
func (h *AttachmentHeader) SetFilename(filename string, opts ...message.Option) {
	dispParams := map[string]string{"filename": filename}
	h.SetContentDisposition("attachment", dispParams, opts...)
}
