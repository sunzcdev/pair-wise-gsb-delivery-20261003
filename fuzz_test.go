package mbox

import (
	"bytes"
	"io/ioutil"
	"math/rand"
	"strings"
	"testing"
	"time"
)

func TestMboxrdFuzzRoundTrip(t *testing.T) {
	// Build bodies from line types whose quoting is well understood, with
	// arbitrary ">" depths and random newline styles.
	mkLine := func(r *rand.Rand) string {
		switch r.Intn(6) {
		case 0:
			return "From zero"
		case 1:
			return strings.Repeat(">", 1+r.Intn(6)) + "From deep"
		case 2:
			return "text " + strings.Repeat("ab", r.Intn(20))
		case 3:
			return "middle From text"
		case 4:
			return ">notfrom"
		default:
			return "Fromx"
		}
	}

	seed := time.Now().UnixNano()
	rng := rand.New(rand.NewSource(seed))

	for iter := 0; iter < 200; iter++ {
		nlines := 1 + rng.Intn(12)
		var nl string
		switch rng.Intn(2) {
		case 0:
			nl = "\n"
		default:
			nl = "\r\n"
		}

		var b bytes.Buffer
		for k := 0; k < nlines; k++ {
			b.WriteString(mkLine(rng))
			b.WriteString(nl)
		}
		input := b.String()

		var archive bytes.Buffer
		mw := NewWriterFormat(&archive, Mboxrd)
		msg, err := mw.CreateMessage("s", time.Unix(0, 0).UTC())
		if err != nil {
			t.Fatal(err)
		}
		p := []byte(input)
		for off := 0; off < len(p); {
			n := 1 + rng.Intn(7)
			if off+n > len(p) {
				n = len(p) - off
			}
			if _, err := msg.Write(p[off : off+n]); err != nil {
				t.Fatal(err)
			}
			off += n
		}
		if err := mw.Close(); err != nil {
			t.Fatal(err)
		}

		rr := NewReaderFormat(bytes.NewReader(archive.Bytes()), Mboxrd)
		rm, err := rr.NextMessage()
		if err != nil {
			t.Fatalf("seed=%d iter=%d: %v\narchive=%q", seed, iter, err, archive.String())
		}
		got, err := ioutil.ReadAll(rm)
		if err != nil {
			t.Fatal(err)
		}

		// Canonicalize input newlines the same way the writer/reader do: writer
		// maps CR and CRLF to LF, reader renders every LF as CRLF.
		canon := strings.Replace(input, "\r\n", "\n", -1)
		want := strings.Replace(canon, "\n", "\r\n", -1) + "\r\n\r\n"
		if string(got) != want {
			t.Fatalf("seed=%d iter=%d mismatch\n in=%q\nwant=%q\ngot =%q",
				seed, iter, input, want, got)
		}
	}
}

func TestMboxoFuzzRoundTrip(t *testing.T) {
	mkLine := func(r *rand.Rand) string {
		switch r.Intn(5) {
		case 0:
			return "From zero"
		case 1:
			return ">From one"
		case 2:
			return ">>From kept"
		case 3:
			return "middle From text"
		default:
			return "plain line"
		}
	}

	seed := time.Now().UnixNano()
	rng := rand.New(rand.NewSource(seed))

	for iter := 0; iter < 200; iter++ {
		var b bytes.Buffer
		for k := 0; k < 1+rng.Intn(12); k++ {
			b.WriteString(mkLine(rng))
			if rng.Intn(2) == 0 {
				b.WriteString("\n")
			} else {
				b.WriteString("\r\n")
			}
		}
		input := b.String()

		var archive bytes.Buffer
		mw := NewWriterFormat(&archive, Mboxo)
		msg, _ := mw.CreateMessage("s", time.Unix(0, 0).UTC())
		p := []byte(input)
		for off := 0; off < len(p); {
			n := 1 + rng.Intn(7)
			if off+n > len(p) {
				n = len(p) - off
			}
			msg.Write(p[off : off+n])
			off += n
		}
		mw.Close()

		rr := NewReader(bytes.NewReader(archive.Bytes()))
		rm, err := rr.NextMessage()
		if err != nil {
			t.Fatalf("seed=%d iter=%d: %v", seed, iter, err)
		}
		got, _ := ioutil.ReadAll(rm)
		canon := strings.Replace(input, "\r\n", "\n", -1)
		// mboxo reader removes one leading ">From " layer.
		canon = strings.Replace(canon, "\n>From ", "\nFrom ", -1)
		if strings.HasPrefix(canon, ">From ") {
			canon = "From " + canon[len(">From "):]
		}
		want := strings.Replace(canon, "\n", "\r\n", -1) + "\r\n\r\n"
		if string(got) != want {
			t.Fatalf("seed=%d iter=%d\n in=%q\ngot=%q", seed, iter, input, got)
		}
	}
}
