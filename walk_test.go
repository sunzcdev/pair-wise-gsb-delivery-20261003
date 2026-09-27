package message

import (
	"errors"
	"io"
	"io/ioutil"
	"reflect"
	"strings"
	"testing"
)

// testMessageRFC822Text is a multipart message embedding another message as
// a message/rfc822 part. The embedded message is itself multipart, and the
// outer message has an attachment after the message/rfc822 part.
const testMessageRFC822Text = "MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=OUTER\r\n" +
	"\r\n" +
	"--OUTER\r\n" +
	"Content-Type: text/plain\r\n" +
	"\r\n" +
	"outer text\r\n" +
	"--OUTER\r\n" +
	"Content-Type: message/rfc822\r\n" +
	"\r\n" +
	"Subject: embedded\r\n" +
	"Content-Type: multipart/alternative; boundary=INNER\r\n" +
	"\r\n" +
	"--INNER\r\n" +
	"Content-Type: text/plain\r\n" +
	"\r\n" +
	"inner text\r\n" +
	"--INNER\r\n" +
	"Content-Type: text/html\r\n" +
	"\r\n" +
	"<p>inner html</p>\r\n" +
	"--INNER--\r\n" +
	"\r\n" +
	"--OUTER\r\n" +
	"Content-Type: application/octet-stream\r\n" +
	"Content-Disposition: attachment; filename=data.bin\r\n" +
	"\r\n" +
	"attachment data\r\n" +
	"--OUTER--\r\n"

// testMessageRFC822BrokenText is the same message, but the embedded
// message's header is malformed.
const testMessageRFC822BrokenText = "MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=OUTER\r\n" +
	"\r\n" +
	"--OUTER\r\n" +
	"Content-Type: text/plain\r\n" +
	"\r\n" +
	"outer text\r\n" +
	"--OUTER\r\n" +
	"Content-Type: message/rfc822\r\n" +
	"\r\n" +
	" broken header line\r\n" +
	"\r\n" +
	"broken body\r\n" +
	"--OUTER\r\n" +
	"Content-Type: application/octet-stream\r\n" +
	"Content-Disposition: attachment; filename=data.bin\r\n" +
	"\r\n" +
	"attachment data\r\n" +
	"--OUTER--\r\n"

func partStep(i int) WalkStep {
	return WalkStep{Part: i}
}

func msgStep() WalkStep {
	return WalkStep{Part: -1, Message: true}
}

type testWalk2Part struct {
	path      []WalkStep
	mediaType string
	body      string
	err       error
}

// walkCollect2 walks e with WalkWithOptions. For entities the walker is
// expected to descend into, the body is left unread; for leaf entities it is
// read in full. readBody, if non-nil, overrides how much of a leaf body is
// consumed.
func walkCollect2(e *Entity, opts *WalkOptions, decide func(path []WalkStep, mediaType string) WalkDecision, readBody func(path []WalkStep, body io.Reader) (string, error)) ([]testWalk2Part, error) {
	var l []testWalk2Part
	err := e.WalkWithOptions(func(path []WalkStep, part *Entity, err error) (WalkDecision, error) {
		rec := testWalk2Part{path: path, err: err}
		decision := WalkDescend
		if part != nil {
			rec.mediaType, _, _ = part.Header.ContentType()
			if decide != nil {
				decision = decide(path, rec.mediaType)
			}
			willDescend := decision == WalkDescend &&
				(part.MultipartReader() != nil ||
					(opts != nil && opts.EnterMessage && rec.mediaType == "message/rfc822"))
			if !willDescend {
				var b []byte
				var rerr error
				if readBody != nil {
					rec.body, rerr = readBody(path, part.Body)
				} else {
					b, rerr = ioutil.ReadAll(part.Body)
					rec.body = string(b)
				}
				if rerr != nil {
					return WalkStop, rerr
				}
			}
		}
		l = append(l, rec)
		return decision, nil
	}, opts)
	return l, err
}

func TestWalkWithOptions_enterMessage(t *testing.T) {
	e, err := Read(strings.NewReader(testMessageRFC822Text))
	if err != nil {
		t.Fatalf("Read() = %v", err)
	}

	want := []testWalk2Part{
		{path: nil, mediaType: "multipart/mixed"},
		{path: []WalkStep{partStep(0)}, mediaType: "text/plain", body: "outer text"},
		{path: []WalkStep{partStep(1)}, mediaType: "message/rfc822"},
		{path: []WalkStep{partStep(1), msgStep()}, mediaType: "multipart/alternative"},
		{path: []WalkStep{partStep(1), msgStep(), partStep(0)}, mediaType: "text/plain", body: "inner text"},
		{path: []WalkStep{partStep(1), msgStep(), partStep(1)}, mediaType: "text/html", body: "<p>inner html</p>"},
		{path: []WalkStep{partStep(2)}, mediaType: "application/octet-stream", body: "attachment data"},
	}

	got, err := walkCollect2(e, &WalkOptions{EnterMessage: true}, nil, nil)
	if err != nil {
		t.Fatalf("Entity.WalkWithOptions() = %v", err)
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("Entity.WalkWithOptions() =\n%#v\nbut want:\n%#v", got, want)
	}
}

// Checks that Walk doesn't enter message/rfc822 parts by default.
func TestWalk_messageRFC822(t *testing.T) {
	e, err := Read(strings.NewReader(testMessageRFC822Text))
	if err != nil {
		t.Fatalf("Read() = %v", err)
	}

	var got []testWalkPart
	err = e.Walk(func(path []int, part *Entity, err error) error {
		mediaType, _, _ := part.Header.ContentType()
		got = append(got, testWalkPart{path: path, mediaType: mediaType})
		if part.MultipartReader() == nil {
			if _, err := ioutil.ReadAll(part.Body); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Entity.Walk() = %v", err)
	}

	want := []testWalkPart{
		{path: nil, mediaType: "multipart/mixed"},
		{path: []int{0}, mediaType: "text/plain"},
		{path: []int{1}, mediaType: "message/rfc822"},
		{path: []int{2}, mediaType: "application/octet-stream"},
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("Entity.Walk() =\n%#v\nbut want:\n%#v", got, want)
	}
}

// Checks that skipping a message/rfc822 part skips its whole subtree and
// that the stream is still positioned correctly for the next outer part.
func TestWalkWithOptions_skipMessage(t *testing.T) {
	e, err := Read(strings.NewReader(testMessageRFC822Text))
	if err != nil {
		t.Fatalf("Read() = %v", err)
	}

	got, err := walkCollect2(e, &WalkOptions{EnterMessage: true}, func(path []WalkStep, mediaType string) WalkDecision {
		if mediaType == "message/rfc822" {
			return WalkSkip
		}
		return WalkDescend
	}, nil)
	if err != nil {
		t.Fatalf("Entity.WalkWithOptions() = %v", err)
	}

	want := []testWalk2Part{
		{path: nil, mediaType: "multipart/mixed"},
		{path: []WalkStep{partStep(0)}, mediaType: "text/plain", body: "outer text"},
		{path: []WalkStep{partStep(1)}, mediaType: "message/rfc822", body: "Subject: embedded\r\n" +
			"Content-Type: multipart/alternative; boundary=INNER\r\n" +
			"\r\n" +
			"--INNER\r\n" +
			"Content-Type: text/plain\r\n" +
			"\r\n" +
			"inner text\r\n" +
			"--INNER\r\n" +
			"Content-Type: text/html\r\n" +
			"\r\n" +
			"<p>inner html</p>\r\n" +
			"--INNER--\r\n"},
		{path: []WalkStep{partStep(2)}, mediaType: "application/octet-stream", body: "attachment data"},
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("Entity.WalkWithOptions() =\n%#v\nbut want:\n%#v", got, want)
	}
}

// Checks that skipping the embedded message's multipart root skips the inner
// parts but leaves the outer stream position intact.
func TestWalkWithOptions_skipInnerMultipart(t *testing.T) {
	e, err := Read(strings.NewReader(testMessageRFC822Text))
	if err != nil {
		t.Fatalf("Read() = %v", err)
	}

	got, err := walkCollect2(e, &WalkOptions{EnterMessage: true}, func(path []WalkStep, mediaType string) WalkDecision {
		if mediaType == "multipart/alternative" {
			return WalkSkip
		}
		return WalkDescend
	}, nil)
	if err != nil {
		t.Fatalf("Entity.WalkWithOptions() = %v", err)
	}

	want := []testWalk2Part{
		{path: nil, mediaType: "multipart/mixed"},
		{path: []WalkStep{partStep(0)}, mediaType: "text/plain", body: "outer text"},
		{path: []WalkStep{partStep(1)}, mediaType: "message/rfc822"},
		{path: []WalkStep{partStep(1), msgStep()}, mediaType: "multipart/alternative", body: "--INNER\r\n" +
			"Content-Type: text/plain\r\n" +
			"\r\n" +
			"inner text\r\n" +
			"--INNER\r\n" +
			"Content-Type: text/html\r\n" +
			"\r\n" +
			"<p>inner html</p>\r\n" +
			"--INNER--\r\n"},
		{path: []WalkStep{partStep(2)}, mediaType: "application/octet-stream", body: "attachment data"},
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("Entity.WalkWithOptions() =\n%#v\nbut want:\n%#v", got, want)
	}
}

func TestWalkWithOptions_stop(t *testing.T) {
	e, err := Read(strings.NewReader(testMessageRFC822Text))
	if err != nil {
		t.Fatalf("Read() = %v", err)
	}

	got, err := walkCollect2(e, &WalkOptions{EnterMessage: true}, func(path []WalkStep, mediaType string) WalkDecision {
		if len(path) == 1 && path[0].Part == 0 {
			return WalkStop
		}
		return WalkDescend
	}, nil)
	if err != nil {
		t.Fatalf("Entity.WalkWithOptions() = %v", err)
	}

	want := []testWalk2Part{
		{path: nil, mediaType: "multipart/mixed"},
		{path: []WalkStep{partStep(0)}, mediaType: "text/plain", body: "outer text"},
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("Entity.WalkWithOptions() =\n%#v\nbut want:\n%#v", got, want)
	}
}

func TestWalkWithOptions_maxDepth(t *testing.T) {
	tests := []struct {
		maxDepth int
		want     []testWalk2Part
	}{
		{
			maxDepth: 1,
			want: []testWalk2Part{
				{path: nil, mediaType: "multipart/mixed"},
				{path: []WalkStep{partStep(0)}, mediaType: "text/plain", body: "outer text"},
				{path: []WalkStep{partStep(1)}, mediaType: "message/rfc822"},
				{path: []WalkStep{partStep(2)}, mediaType: "application/octet-stream", body: "attachment data"},
			},
		},
		{
			maxDepth: 2,
			want: []testWalk2Part{
				{path: nil, mediaType: "multipart/mixed"},
				{path: []WalkStep{partStep(0)}, mediaType: "text/plain", body: "outer text"},
				{path: []WalkStep{partStep(1)}, mediaType: "message/rfc822"},
				{path: []WalkStep{partStep(1), msgStep()}, mediaType: "multipart/alternative"},
				{path: []WalkStep{partStep(2)}, mediaType: "application/octet-stream", body: "attachment data"},
			},
		},
	}

	for _, test := range tests {
		e, err := Read(strings.NewReader(testMessageRFC822Text))
		if err != nil {
			t.Fatalf("Read() = %v", err)
		}

		got, err := walkCollect2(e, &WalkOptions{EnterMessage: true, MaxDepth: test.maxDepth}, nil, nil)
		if err != nil {
			t.Fatalf("Entity.WalkWithOptions(MaxDepth=%v) = %v", test.maxDepth, err)
		}

		if !reflect.DeepEqual(got, test.want) {
			t.Errorf("Entity.WalkWithOptions(MaxDepth=%v) =\n%#v\nbut want:\n%#v", test.maxDepth, got, test.want)
		}
	}
}

func TestWalkWithOptions_maxBytes(t *testing.T) {
	e, err := Read(strings.NewReader(testMessageRFC822Text))
	if err != nil {
		t.Fatalf("Read() = %v", err)
	}

	_, err = walkCollect2(e, &WalkOptions{EnterMessage: true, MaxBytes: 10}, nil, nil)
	if !errors.Is(err, ErrWalkLimit) {
		t.Fatalf("Entity.WalkWithOptions(MaxBytes=10) = %v, want ErrWalkLimit", err)
	}
}

func TestWalkWithOptions_maxBytesGenerous(t *testing.T) {
	e, err := Read(strings.NewReader(testMessageRFC822Text))
	if err != nil {
		t.Fatalf("Read() = %v", err)
	}

	got, err := walkCollect2(e, &WalkOptions{EnterMessage: true, MaxBytes: 1 << 20}, nil, nil)
	if err != nil {
		t.Fatalf("Entity.WalkWithOptions() = %v", err)
	}
	if n := len(got); n != 7 {
		t.Fatalf("Entity.WalkWithOptions() visited %v entities, want 7", n)
	}
}

// Checks that a broken embedded message is reported and that the outer
// stream is still positioned correctly for the next part.
func TestWalkWithOptions_brokenMessage(t *testing.T) {
	e, err := Read(strings.NewReader(testMessageRFC822BrokenText))
	if err != nil {
		t.Fatalf("Read() = %v", err)
	}

	got, err := walkCollect2(e, &WalkOptions{EnterMessage: true}, nil, nil)
	if err != nil {
		t.Fatalf("Entity.WalkWithOptions() = %v", err)
	}

	if len(got) != 5 {
		t.Fatalf("Entity.WalkWithOptions() visited %v entities, want 5", len(got))
	}

	broken := got[3]
	wantPath := []WalkStep{partStep(1), msgStep()}
	if !reflect.DeepEqual(broken.path, wantPath) {
		t.Errorf("broken embedded message path = %#v, want %#v", broken.path, wantPath)
	}
	if broken.err == nil {
		t.Errorf("broken embedded message: expected an error, got nil")
	}

	last := got[4]
	if last.mediaType != "application/octet-stream" || last.body != "attachment data" {
		t.Errorf("part after broken embedded message = %#v, want application/octet-stream attachment", last)
	}
}

// Checks that leaving inner parts partially read doesn't corrupt the stream
// position of the remaining outer parts.
func TestWalkWithOptions_innerEarlyStop(t *testing.T) {
	e, err := Read(strings.NewReader(testMessageRFC822Text))
	if err != nil {
		t.Fatalf("Read() = %v", err)
	}

	// Only read the first 4 bytes of each inner leaf body, read outer leaf
	// bodies in full
	readHead := func(path []WalkStep, r io.Reader) (string, error) {
		inner := false
		for _, step := range path {
			if step.Message {
				inner = true
			}
		}
		if !inner {
			b, err := ioutil.ReadAll(r)
			return string(b), err
		}
		b := make([]byte, 4)
		n, err := io.ReadFull(r, b)
		if err != nil && err != io.ErrUnexpectedEOF {
			return "", err
		}
		return string(b[:n]), nil
	}

	got, err := walkCollect2(e, &WalkOptions{EnterMessage: true}, nil, readHead)
	if err != nil {
		t.Fatalf("Entity.WalkWithOptions() = %v", err)
	}

	want := []testWalk2Part{
		{path: nil, mediaType: "multipart/mixed"},
		{path: []WalkStep{partStep(0)}, mediaType: "text/plain", body: "outer text"},
		{path: []WalkStep{partStep(1)}, mediaType: "message/rfc822"},
		{path: []WalkStep{partStep(1), msgStep()}, mediaType: "multipart/alternative"},
		{path: []WalkStep{partStep(1), msgStep(), partStep(0)}, mediaType: "text/plain", body: "inne"},
		{path: []WalkStep{partStep(1), msgStep(), partStep(1)}, mediaType: "text/html", body: "<p>i"},
		{path: []WalkStep{partStep(2)}, mediaType: "application/octet-stream", body: "attachment data"},
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("Entity.WalkWithOptions() =\n%#v\nbut want:\n%#v", got, want)
	}
}
