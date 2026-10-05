package httphead

// feed processes a single chunk. final reports that no more data will ever
// be fed, so unterminated structures are reported here.
func (s *OptionScanner) feed(p []byte, final bool) (int, error) {
	base := s.total
	s.total += len(p)
	n := 0

	if final && len(p) == 0 {
		switch s.mode {
		case omString:
			s.consumed = s.pendOffset
			return 0, s.lexerError(s.pendOffset)
		case omComment:
			s.consumed = s.pendOffset
			return 0, s.lexerError(s.pendOffset)
		case omCR, omCRLF:
			s.consumed = s.pendOffset
			return 0, s.lexerError(s.pendOffset + 1)
		case omToken:
			tok := s.pending
			s.pending = nil
			s.mode = omNone
			if err := s.gateOption(s.total); err != nil {
				return 0, err
			}
			if err := s.itemToken(tok, s.pendOffset, false, s.total); err != nil {
				return 0, err
			}
		}
	}

	for n < len(p) {
		switch s.mode {
		case omSkip:
			i := n
			for i < len(p) {
				if p[i] == ',' && !s.escaped {
					break
				}
				s.escaped = p[i] == '\\'
				i++
			}
			s.consumed = base + i
			if i == len(p) {
				// The unescaped comma may arrive in the next chunk.
				return i, nil
			}
			s.escaped = false
			s.mode = omNone
			s.state = osKey
			s.mustCall = false
			s.optionStart = -1
			n = i + 1
			s.consumed = base + n
			continue

		case omToken:
			i := n
			for i < len(p) && OctetTypes[p[i]].IsToken() {
				i++
			}
			s.pending = append(s.pending, p[n:i]...)
			n = i
			if err := s.gateOption(base + n); err != nil {
				return n, err
			}
			if n == len(p) && !final {
				// The token may continue in the next chunk.
				return n, nil
			}
			tok := s.pending
			s.pending = nil
			s.mode = omNone
			consumed := s.pendOffset + len(tok)
			s.consumed = consumed
			if err := s.itemToken(tok, s.pendOffset, false, consumed); err != nil {
				return n, err
			}
			s.pendOffset = 0
			if i < len(p) {
				// p[n] is the byte that ended the token run.
				c := p[n]
				if c != ' ' && c != '\r' && !OctetTypes[c].IsSeparator() {
					s.consumed = consumed
					return n, s.lexerError(consumed + 1)
				}
			}
			if s.stopped {
				return n, nil
			}
			continue

		case omString:
			i := n
			for i < len(p) {
				if p[i] == '"' && !s.escaped {
					break
				}
				s.escaped = p[i] == '\\'
				i++
			}
			s.pending = append(s.pending, p[n:i]...)
			n = i
			if i < len(p) {
				// Closing quote at base+i.
				n = i + 1
				if err := s.gateOption(base + n); err != nil {
					return n, err
				}
				raw := s.pending
				s.pending = nil
				s.escaped = false
				s.mode = omNone
				if err := s.itemString(RemoveByte(raw, '\\'), base+n); err != nil {
					return n, err
				}
				s.pendOffset = 0
				if s.stopped {
					return n, nil
				}
				continue
			}
			if final {
				s.consumed = s.pendOffset
				return n, s.lexerError(s.pendOffset)
			}
			if err := s.gateOption(base + n); err != nil {
				return n, err
			}
			return n, nil

		case omComment:
			// Only bytes from the current position belong to the comment;
			// pending already holds the content of earlier chunks.
			s.pending = append(s.pending, p[n:]...)
			n = len(p)
			close := ScanPairGreedy(s.pending, '(', ')')
			if close == -1 {
				if err := s.gateOption(base + n); err != nil {
					return n, err
				}
				if final {
					s.consumed = s.pendOffset
					return n, s.lexerError(s.pendOffset)
				}
				return n, nil
			}
			// Index of the byte past the closing ")" within this chunk.
			n = s.pendOffset + close + 1 - base
			end := s.pendOffset + close + 1
			if err := s.gateOption(end); err != nil {
				return n, err
			}
			s.pending = nil
			s.mode = omNone
			s.pendOffset = 0
			// A closed comment is a valid ItemComment lexeme; the
			// option-level grammar has no use for it, so this mirrors the
			// ScanOptions default branch.
			return n, s.syntaxErrorAt(end)

		case omCR:
			if p[n] != '\n' {
				s.consumed = s.pendOffset
				return n, s.lexerError(s.pendOffset + 1)
			}
			s.mode = omCRLF
			n++
			continue

		case omCRLF:
			c := p[n]
			if c != ' ' {
				s.consumed = s.pendOffset
				return n, s.lexerError(s.pendOffset + 1)
			}
			s.mode = omNone
			n++

		case omNone:
			if s.mode != omNone {
				continue
			}
			// Skip SP and LWS sequences, mirroring SkipSpace. In the package
			// octet table HT(9) is unclassified (the table starts at 32), so
			// it is an illegal octet rather than a space.
			sk := skipSpaces(p, n)
			n = sk.n
			if sk.errAt >= 0 {
				s.consumed = base + sk.errAt
				return n, s.lexerError(base + sk.errAt + 1)
			}
			if sk.dangling {
				// CR or CRLF at the chunk edge; remember where the CR was.
				s.pendOffset = base + sk.crAt
				s.consumed = base + n
				if final {
					s.consumed = s.pendOffset
					return n, s.lexerError(s.pendOffset + 1)
				}
				if sk.sawLF {
					s.mode = omCRLF
				} else {
					s.mode = omCR
				}
				return n, nil
			}
			if n == len(p) {
				// Trailing spaces are not consumed by the one-shot lexer:
				// its nextChar() skips them only when another lexeme
				// follows. Keep the processed position where the spaces
				// began.
				return n, nil
			}
			s.consumed = base + n

			c := p[n]
			switch {
			case c == '"':
				s.mode = omString
				s.escaped = false
				s.pendOffset = base + n + 1
				s.pending = s.pending[:0]
				n++
				continue

			case c == '(':
				s.mode = omComment
				s.pendOffset = base + n + 1
				s.pending = s.pending[:0]
				n++
				continue

			case c == '\\' || c == ')':
				s.consumed = base + n
				return n, s.lexerError(base + n + 1)

			case OctetTypes[c].IsSeparator():
				end := base + n + 1
				s.consumed = end
				if err := s.itemSeparator(c); err != nil {
					return n + 1, err
				}
				n++
				if s.stopped {
					return n, nil
				}
				continue

			case OctetTypes[c].IsToken():
				i := n + 1
				for i < len(p) && OctetTypes[p[i]].IsToken() {
					i++
				}
				if i == len(p) && !final {
					// Token runs to the chunk edge; carry its bytes.
					s.pending = append(s.pending[:0], p[n:i]...)
					s.pendOffset = base + n
					s.mode = omToken
					n = i
					if err := s.gateOption(base + n); err != nil {
						return i, err
					}
					return n, nil
				}
				tok := p[n:i]
				start := base + n
				n = i
				if err := s.gateOption(base + n); err != nil {
					return n, err
				}
				if err := s.itemToken(tok, start, true, base+n); err != nil {
					return n, err
				}
				if s.stopped {
					return n, nil
				}
				if n < len(p) {
					if c := p[n]; !OctetTypes[c].IsSeparator() {
						if c != ' ' && c != '\r' {
							s.consumed = base + n
							return n, s.lexerError(base + n + 1)
						}
					}
				}
				continue

			default:
				// CTL or other invalid octet at a lexeme start.
				s.consumed = base + n
				return n, s.lexerError(base + n + 1)
			}
		}
	}

	return n, nil
}

// spaceSkip is the result of skipSpaces.
type spaceSkip struct {
	n        int
	errAt    int
	dangling bool
	sawLF    bool
	crAt     int
}

// skipSpaces scans SP and LWS sequences starting at p[at], mirroring the
// semantics of SkipSpace. It reports a dangling CR or CRLF at the end of p
// instead of treating it as an error, because the deciding byte may arrive in
// the next chunk.
func skipSpaces(p []byte, at int) spaceSkip {
	i := at
	for i < len(p) {
		c := p[i]
		if c == ' ' {
			i++
			continue
		}
		if c != '\r' {
			break
		}
		crAt := i
		if i+1 >= len(p) {
			return spaceSkip{n: i + 1, errAt: -1, dangling: true, crAt: crAt}
		}
		if p[i+1] != '\n' {
			return spaceSkip{n: i, errAt: crAt}
		}
		if i+2 >= len(p) {
			return spaceSkip{n: i + 2, errAt: -1, dangling: true, sawLF: true, crAt: crAt}
		}
		if p[i+2] != ' ' {
			return spaceSkip{n: i + 2, errAt: crAt}
		}
		i += 3
	}
	return spaceSkip{n: i, errAt: -1}
}

// itemToken routes a complete token to the option-level state machine.
// fromChunk reports whether v aliases the bytes of the caller's current
// chunk (and therefore has to be baked when Write returns).
func (s *OptionScanner) itemToken(v []byte, start int, fromChunk bool, end int) error {
	switch s.state {
	case osKey, osParamBeforeName:
		if s.state == osKey {
			s.optionStart = start
			if s.maxOption > 0 && end > s.optionStart+s.maxOption {
				s.consumed = s.optionStart + s.maxOption
				return &LimitError{Offset: s.optionStart + s.maxOption, Input: false}
			}
		}
		s.arena = s.arena[:0]
		if fromChunk {
			s.key = v
			s.keyChunk = true
		} else {
			// The token was assembled across chunks into scanner memory.
			s.key = s.own(v)
			s.keyChunk = false
		}
		s.state = osParamBeforeName
		s.mustCall = true

	case osParamName:
		if fromChunk {
			s.param = v
			s.paramChunk = true
		} else {
			s.param = s.own(v)
			s.paramChunk = false
		}
		s.state = osParamBeforeValue
		s.mustCall = true

	case osParamValue:
		if fromChunk {
			s.value = v
			s.valueChunk = true
		} else {
			s.value = s.own(v)
			s.valueChunk = false
		}
		s.state = osParamBeforeName
		if err := s.fire(); err != nil {
			return err
		}

	default:
		return s.syntaxErrorAt(start + len(v))
	}
	return nil
}

// itemString routes a complete quoted parameter value. v is always
// scanner-owned because it was assembled from pending bytes. end is the
// absolute offset of the byte past the closing quote.
func (s *OptionScanner) itemString(v []byte, end int) error {
	if s.state != osParamValue {
		return s.syntaxErrorAt(end)
	}
	s.value = s.own(v)
	s.state = osParamBeforeName
	return s.fire()
}

func (s *OptionScanner) itemSeparator(c byte) error {
	switch {
	case c == ',' && s.state == osKey:
		// Empty alternative; ignored.

	case c == ',' && s.state == osParamBeforeName:
		s.state = osKey
		if !s.mustCall {
			s.index++
		} else {
			if err := s.fire(); err != nil {
				return err
			}
			s.index++
		}
		s.optionStart = -1

	case c == ',' && s.state == osParamBeforeValue:
		s.state = osKey
		if err := s.fire(); err != nil {
			return err
		}
		s.index++
		s.optionStart = -1

	case c == ';' && s.state == osParamBeforeName:
		s.state = osParamName

	case c == ';' && s.state == osParamBeforeValue:
		s.state = osParamName
		if err := s.fire(); err != nil {
			return err
		}

	case c == '=' && s.state == osParamBeforeValue:
		s.state = osParamValue

	default:
		return s.syntaxErrorAt(s.consumed)
	}
	return nil
}

// fire invokes the callback for the currently assembled key/param/value and
// applies the returned Control value, mirroring ScanOptions.
func (s *OptionScanner) fire() error {
	s.seen = true
	switch s.fn(s.index, s.key, s.param, s.value) {
	case ControlBreak:
		s.stopped = true
	case ControlSkip:
		s.state = osKey
		s.mustCall = false
		s.optionStart = -1
		s.mode = omSkip
		s.escaped = false
	case ControlContinue:
	default:
		panic("unexpected control value")
	}
	s.param = nil
	s.value = nil
	s.paramChunk = false
	s.valueChunk = false
	s.mustCall = false
	return nil
}

// gateOption records the processed position and enforces the single-option
// size limit. end is the absolute offset up to which content was scanned; the
// offending offset is the first byte past the configured option size.
func (s *OptionScanner) gateOption(end int) error {
	s.consumed = end
	if s.maxOption > 0 && s.optionStart >= 0 {
		if end > s.optionStart+s.maxOption {
			return &LimitError{Offset: s.optionStart + s.maxOption, Input: false}
		}
	}
	return nil
}

func (s *OptionScanner) syntaxErrorAt(offset int) error {
	return &SyntaxError{Offset: offset}
}

// lexerError reports a low-level lexer error. ScanOptions flushes a pending
// option key after such an error because the token was already scanned.
func (s *OptionScanner) lexerError(offset int) error {
	s.lexical = true
	return &SyntaxError{Offset: offset}
}
