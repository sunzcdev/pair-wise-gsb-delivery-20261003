package message

import (
	"errors"
	"io"
)

// WalkStep is a single step in the path of an entity visited by
// Entity.WalkWithOptions.
type WalkStep struct {
	// Part is the index of the entity within its parent multipart entity.
	// It is -1 when Message is true.
	Part int
	// Message is true when the step enters a message/rfc822 embedded
	// message, instead of moving to the next MIME part of a multipart
	// entity.
	Message bool
}

// WalkDecision indicates how Entity.WalkWithOptions should proceed after
// visiting an entity.
type WalkDecision int

const (
	// WalkDescend descends into the entity's children, if any. For
	// multipart entities, children are the MIME parts. For message/rfc822
	// entities, the child is the embedded message (provided
	// WalkOptions.EnterMessage is set).
	WalkDescend WalkDecision = iota
	// WalkSkip skips the entity's children: the whole subtree rooted at
	// the entity is skipped and the walk proceeds with the next sibling.
	WalkSkip
	// WalkStop stops the whole walk.
	WalkStop
)

// WalkFunc2 is the type of the function called for each entity visited by
// Entity.WalkWithOptions.
//
// The path argument is a list of steps leading to the entity. The root
// entity has a nil path. A step with Message set to false moves to the next
// MIME part of a multipart entity, a step with Message set to true enters a
// message/rfc822 embedded message.
//
// If there was an error walking to an entity, the incoming error describes
// the problem and the function can decide how to handle it. The entity may
// be nil if it couldn't be parsed at all.
//
// The returned decision indicates whether the walk should descend into the
// entity's children, skip them, or stop entirely. If a non-nil error is
// returned, the walk stops and the error is returned by
// Entity.WalkWithOptions.
type WalkFunc2 func(path []WalkStep, entity *Entity, err error) (WalkDecision, error)

// WalkOptions are options for Entity.WalkWithOptions.
//
// The zero value is valid and matches the behavior of Entity.Walk.
type WalkOptions struct {
	// EnterMessage indicates whether the walk descends into message/rfc822
	// entities, parsing the embedded message. If false, message/rfc822
	// entities are treated as leaf entities.
	EnterMessage bool

	// MaxDepth limits how deep the walk descends. The root entity is at
	// depth 0. Entities at the maximum depth are still visited, but their
	// children are skipped. Zero means no limit.
	MaxDepth int

	// MaxBytes limits the cumulative number of bytes read from the message
	// stream while walking. Once the limit is reached, the walk stops and
	// ErrWalkLimit is returned. Zero means no limit.
	MaxBytes int64
}

// ErrWalkLimit is returned by Entity.WalkWithOptions when a limit set in
// WalkOptions is exceeded.
var ErrWalkLimit = errors.New("message: walk limit exceeded")

// walkFrame is a frame in the WalkWithOptions entity stack. A nil
// MultipartReader indicates a frame for a message/rfc822 embedded message.
type walkFrame struct {
	mr MultipartReader
}

type walker struct {
	opts WalkOptions
	n    int64 // number of bytes read from the message stream so far
}

func (w *walker) limited() bool {
	return w.opts.MaxBytes > 0 && w.n >= w.opts.MaxBytes
}

// walkReader counts the bytes read from the underlying message stream and
// refuses to read more once WalkOptions.MaxBytes is reached.
type walkReader struct {
	r io.Reader
	w *walker
}

func (rd *walkReader) Read(p []byte) (int, error) {
	if rd.w.limited() {
		return 0, ErrWalkLimit
	}
	n, err := rd.r.Read(p)
	rd.w.n += int64(n)
	return n, err
}

// WalkWithOptions walks the entity's tree, calling walkFunc for each entity
// in the tree, including the root entity. It behaves like Walk, but the
// traversal can be customized with WalkOptions and WalkDecision.
//
// WalkWithOptions consumes the entity.
func (e *Entity) WalkWithOptions(walkFunc WalkFunc2, opts *WalkOptions) error {
	w := walker{}
	if opts != nil {
		w.opts = *opts
	}

	root := e
	if w.opts.MaxBytes > 0 {
		if _, ok := e.Body.(*multipartBody); !ok {
			rootCopy := *e
			rootCopy.Body = &walkReader{r: e.Body, w: &w}
			root = &rootCopy
		}
	}

	var frames []walkFrame
	var path []WalkStep
	part := root
	var partErr error
	for {
		if part == nil {
			if len(frames) == 0 {
				break
			}

			f := frames[len(frames)-1]
			if f.mr == nil {
				// The embedded message has been fully walked
				frames = frames[:len(frames)-1]
				path = path[:len(path)-1]
				continue
			}

			p, err := f.mr.NextPart()
			if err == io.EOF {
				frames = frames[:len(frames)-1]
				path = path[:len(path)-1]
				continue
			} else if IsUnknownEncoding(err) || IsUnknownCharset(err) {
				// Forward the error to walkFunc
				part, partErr = p, err
			} else if err != nil {
				if w.limited() {
					return ErrWalkLimit
				}
				return err
			} else {
				part = p
			}

			path[len(path)-1].Part++
		}

		if w.limited() {
			return ErrWalkLimit
		}

		// Copy the path since we'll mutate it on the next iteration
		var pathCopy []WalkStep
		if len(path) > 0 {
			pathCopy = make([]WalkStep, len(path))
			copy(pathCopy, path)
		}

		decision, err := walkFunc(pathCopy, part, partErr)
		if err != nil {
			return err
		}
		if decision == WalkStop {
			return nil
		}

		cur := part
		part, partErr = nil, nil

		descend := decision == WalkDescend &&
			(w.opts.MaxDepth <= 0 || len(path) < w.opts.MaxDepth)
		if !descend {
			continue
		}

		if mr := cur.MultipartReader(); mr != nil {
			frames = append(frames, walkFrame{mr: mr})
			path = append(path, WalkStep{Part: -1})
			continue
		}

		if w.opts.EnterMessage && cur.mediaType == "message/rfc822" {
			inner, err := Read(cur.Body)
			if inner != nil {
				frames = append(frames, walkFrame{})
				path = append(path, WalkStep{Part: -1, Message: true})
				part, partErr = inner, err
				continue
			}

			// The embedded message couldn't be parsed at all: report
			// the error and skip its subtree.
			if w.limited() {
				return ErrWalkLimit
			}
			msgPath := make([]WalkStep, len(path)+1)
			copy(msgPath, path)
			msgPath[len(path)] = WalkStep{Part: -1, Message: true}
			decision, err := walkFunc(msgPath, nil, err)
			if err != nil {
				return err
			}
			if decision == WalkStop {
				return nil
			}
		}
	}

	return nil
}
