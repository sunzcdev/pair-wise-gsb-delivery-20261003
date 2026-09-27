package message

import (
	"errors"
	"io"
	"io/ioutil"
	"strings"

	"github.com/emersion/go-message/textproto"
)

// SkipChildren can be returned from a WalkEntityFunc to skip the children of
// the entity currently being visited. The walk continues with the entity's
// siblings.
var SkipChildren = errors.New("message: skip entity children")

var errWalkTooBig = errors.New("message: walk exceeds maximum cumulative body size")

// A WalkStep is a single element of a WalkPath.
type WalkStep struct {
	// Index is the index of the part within its parent multipart entity. It
	// is -1 for steps which enter an embedded message.
	Index int
	// Embedded is true if this step enters a message/rfc822 embedded
	// message instead of descending into a multipart part.
	Embedded bool
}

// A WalkPath is a list of steps leading to an entity in a message tree. A
// step either descends into the next MIME part of a multipart entity
// (Index >= 0) or enters a message/rfc822 embedded message (Embedded ==
// true). The root entity has a nil path.
type WalkPath []WalkStep

// WalkEntityFunc is the type of the function called for each entity visited
// by Entity.WalkWithOptions.
//
// The path argument is a list of steps leading to the entity. The root
// entity has a nil path.
//
// If there was an error walking to an entity, the incoming error describes
// the problem and the function can decide how to handle it. The entity
// argument is nil if a message/rfc822 embedded message couldn't be parsed at
// all.
//
// If the function returns SkipChildren, the entity's children are not
// visited and the walk continues with the entity's siblings. If it returns
// any other non-nil error, the walk stops.
type WalkEntityFunc func(path WalkPath, entity *Entity, err error) error

// WalkOptions are options for Entity.WalkWithOptions. The zero value
// behaves like Entity.Walk.
type WalkOptions struct {
	// EnterEmbedded controls whether message/rfc822 parts are parsed and
	// walked as embedded messages. If false, they are treated as leaf
	// parts.
	EnterEmbedded bool
	// MaxDepth limits how deep the walk descends into the entity tree:
	// entities with a path longer than MaxDepth are not visited. Zero or
	// negative means no limit.
	MaxDepth int
	// MaxBytes limits the cumulative number of bytes consumed from entity
	// bodies while walking, including bytes discarded to skip over unread
	// entities. If the limit is exceeded, the walk stops with an error.
	// Zero or negative means no limit.
	MaxBytes int64
}

// WalkWithOptions walks the entity's tree like Walk, but reports positions
// as WalkPath values and allows the caller to control the walk with
// WalkOptions.
//
// WalkWithOptions consumes the entity.
func (e *Entity) WalkWithOptions(walkFunc WalkEntityFunc, opts *WalkOptions) error {
	w := &walker{fn: walkFunc}
	if opts != nil {
		w.opts = *opts
	}
	return w.visit(nil, e, nil)
}

type walker struct {
	fn   WalkEntityFunc
	opts WalkOptions
	n    int64 // cumulative number of body bytes consumed
}

// count wraps r so that bytes read from it are accounted against
// w.opts.MaxBytes.
func (w *walker) count(r io.Reader) io.Reader {
	if w.opts.MaxBytes <= 0 {
		return r
	}
	return &walkReader{r: r, w: w}
}

// multipartReader is like Entity.MultipartReader, but accounts bytes read
// from the entity's body against w.opts.MaxBytes.
func (w *walker) multipartReader(e *Entity) MultipartReader {
	if !strings.HasPrefix(e.mediaType, "multipart/") {
		return nil
	}
	if mb, ok := e.Body.(*multipartBody); ok {
		return mb
	}
	e.Body = w.count(e.Body)
	return &multipartReader{textproto.NewMultipartReader(e.Body, e.mediaParams["boundary"])}
}

func (w *walker) visit(path WalkPath, e *Entity, walkErr error) error {
	if walkErr != nil && !IsUnknownEncoding(walkErr) && !IsUnknownCharset(walkErr) {
		return walkErr
	}

	mr := w.multipartReader(e)
	if mr == nil {
		e.Body = w.count(e.Body)
	}
	embedded := w.opts.EnterEmbedded && e.mediaType == "message/rfc822"
	descend := (mr != nil || embedded) && (w.opts.MaxDepth <= 0 || len(path) < w.opts.MaxDepth)

	skip := false
	if err := w.fn(path, e, walkErr); err != nil {
		if errors.Is(err, SkipChildren) {
			skip = true
		} else {
			return err
		}
	}
	if !descend || skip {
		return nil
	}

	if mr != nil {
		for i := 0; ; i++ {
			child, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			childPath := appendWalkStep(path, WalkStep{Index: i})
			if err := w.visit(childPath, child, err); err != nil {
				return err
			}
		}
		return nil
	}

	// Enter a message/rfc822 embedded message.
	childPath := appendWalkStep(path, WalkStep{Index: -1, Embedded: true})
	child, err := Read(e.Body)
	if err != nil {
		// The embedded message is malformed: forward the error, then
		// discard its remains to keep the stream position.
		if err := w.fn(childPath, nil, err); err != nil {
			return err
		}
		if _, err := io.Copy(ioutil.Discard, e.Body); err != nil {
			return err
		}
		return nil
	}
	if err := w.visit(childPath, child, nil); err != nil {
		return err
	}
	// Discard the embedded message's remains (e.g. the multipart epilogue)
	// so that the enclosing multipart reader stays in sync.
	if _, err := io.Copy(ioutil.Discard, child.Body); err != nil {
		return err
	}
	return nil
}

func appendWalkStep(path WalkPath, step WalkStep) WalkPath {
	childPath := make(WalkPath, len(path)+1)
	copy(childPath, path)
	childPath[len(path)] = step
	return childPath
}

type walkReader struct {
	r io.Reader
	w *walker
}

func (rd *walkReader) Read(p []byte) (int, error) {
	if rd.w.n >= rd.w.opts.MaxBytes {
		return 0, errWalkTooBig
	}
	n, err := rd.r.Read(p)
	rd.w.n += int64(n)
	return n, err
}
