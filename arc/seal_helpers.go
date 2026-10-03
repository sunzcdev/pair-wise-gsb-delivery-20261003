package arc

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/emersion/go-msgauth/dkim"
)

func validateSealOptions(options *SealOptions) error {
	if options == nil {
		return errors.New("arc: no options specified")
	}
	if options.Instance <= 0 {
		return errors.New("arc: instance number must be positive")
	}
	if options.Domain == "" {
		return errors.New("arc: no domain specified")
	}
	if options.Selector == "" {
		return errors.New("arc: no selector specified")
	}
	if options.Signer == nil {
		return errors.New("arc: no signer specified")
	}
	if options.AuthServID == "" {
		return errors.New("arc: no authentication service identifier specified")
	}
	switch options.Chain {
	case ChainNone, ChainPass, ChainFail:
	default:
		return fmt.Errorf("arc: invalid chain value %q", options.Chain)
	}
	if options.Instance > 1 && options.Chain == ChainNone {
		return errors.New("arc: cv=none can only be used on the first ARC set")
	}
	if options.Instance == 1 && options.Chain != ChainNone {
		return errors.New("arc: the first ARC set must use cv=none")
	}
	return nil
}

func checkSealInstance(fields dkim.Header, instance int) error {
	seen := map[int]struct{}{}
	for _, field := range fields {
		name, value := dkim.ParseHeaderField(field)
		if !isARCHeader(name) {
			continue
		}
		n, err := parseInstance(value)
		if err != nil {
			return fmt.Errorf("arc: malformed existing set: %w", err)
		}
		seen[n] = struct{}{}
	}
	if instance > maxInstances {
		return fmt.Errorf("arc: instance number %d exceeds maximum %d", instance, maxInstances)
	}
	for i := 1; i < instance; i++ {
		if _, ok := seen[i]; !ok {
			return fmt.Errorf("arc: instance %d is missing", i)
		}
	}
	if _, ok := seen[instance]; ok {
		return fmt.Errorf("arc: instance %d already exists", instance)
	}
	return nil
}

func amsHeaderKeys(fields dkim.Header, configured []string, instance int) []string {
	keys := reverseRegularHeaderKeys(fields)
	for i := instance - 1; i >= 1; i-- {
		keys = append(keys, asHeaderName, amsHeaderName, aarHeaderName)
	}
	keys = append(keys, aarHeaderName)

	configured = append(configured, "From")
	for _, key := range configured {
		if !containsKey(keys, key) {
			keys = append(keys, key)
		}
	}
	if !containsKey(keys, "From") {
		keys = append(keys, "From")
	}
	return keys
}

func sealHeaderKeys(fields dkim.Header, instance int) []string {
	keys := reverseRegularHeaderKeys(fields)
	for i := instance; i >= 1; i-- {
		keys = append(keys, amsHeaderName, aarHeaderName)
	}
	for i := instance - 1; i >= 1; i-- {
		keys = append(keys, asHeaderName)
	}
	return keys
}

func reverseRegularHeaderKeys(fields dkim.Header) []string {
	seen := map[string]bool{}
	var keys []string
	for i := len(fields) - 1; i >= 0; i-- {
		name, _ := dkim.ParseHeaderField(fields[i])
		lower := strings.ToLower(name)
		if isARCHeader(name) || seen[lower] {
			continue
		}
		seen[lower] = true
		keys = append(keys, name)
	}
	return keys
}

func containsKey(keys []string, want string) bool {
	for _, key := range keys {
		if strings.EqualFold(key, want) {
			return true
		}
	}
	return false
}

func countHeaderKeys(keys []string, want string) int {
	count := 0
	for _, key := range keys {
		if strings.EqualFold(key, want) {
			count++
		}
	}
	return count
}

func isARCHeader(name string) bool {
	return strings.EqualFold(name, aarHeaderName) ||
		strings.EqualFold(name, amsHeaderName) ||
		strings.EqualFold(name, asHeaderName)
}

func formatHeaderField(name, value string) string {
	return name + ": " + value + crlf
}

func parseInstance(value string) (int, error) {
	flat := strings.NewReplacer("\r\n ", " ", "\r\n\t", " ", "\n ", " ", "\n\t", " ").Replace(value)
	for _, tag := range strings.Split(flat, ";") {
		name, raw, ok := strings.Cut(tag, "=")
		if ok && strings.EqualFold(strings.TrimSpace(name), "i") {
			instance, err := strconv.Atoi(strings.TrimSpace(raw))
			if err != nil {
				return 0, fmt.Errorf("malformed instance tag: %w", err)
			}
			if instance <= 0 {
				return 0, errors.New("instance number must be positive")
			}
			return instance, nil
		}
	}
	return 0, errors.New("malformed instance tag")
}

func hashBody(c dkim.Canonicalization, body []byte) ([]byte, error) {
	hasher := sha256.New()
	canonicalizer, err := dkim.CanonicalizeBody(c, hasher)
	if err != nil {
		return nil, err
	}
	if _, err := canonicalizer.Write(body); err != nil {
		return nil, err
	}
	if err := canonicalizer.Close(); err != nil {
		return nil, err
	}
	return hasher.Sum(nil), nil
}

func hashHeaders(c dkim.Canonicalization, fields dkim.Header, keys []string) ([]byte, error) {
	hasher := sha256.New()
	picker := dkim.NewHeaderPicker(fields)
	for _, key := range keys {
		field := picker.Pick(key)
		if field == "" {
			continue
		}
		if _, err := io.WriteString(hasher, dkim.CanonicalizeHeader(c, field)); err != nil {
			return nil, err
		}
	}
	return hasher.Sum(nil), nil
}

func hashMessageSignature(c dkim.Canonicalization, fields dkim.Header, keys string, sigField string) ([]byte, error) {
	hashed, err := hashHeaders(c, fields, strings.Split(keys, ":"))
	if err != nil {
		return nil, err
	}
	sigField = removeSignatureValue(sigField)
	canonical := strings.TrimRight(dkim.CanonicalizeHeader(c, sigField), crlf)
	return append(hashed, []byte(canonical)...), nil
}

func removeSignatureValue(field string) string {
	lines := strings.Split(field, crlf)
	var kept []string
	removing := false
	for _, line := range lines {
		if idx := indexSignatureTag(line); idx >= 0 {
			prefix := strings.TrimRight(line[:idx+2], " \t")
			if prefix != "" {
				kept = append(kept, prefix)
			}
			removing = !strings.Contains(line[idx:], ";")
			continue
		}
		if removing && (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) {
			continue
		}
		removing = false
		kept = append(kept, line)
	}
	return strings.Join(kept, crlf)
}

func indexSignatureTag(line string) int {
	lower := strings.ToLower(line)
	idx := strings.Index(lower, "b=")
	if idx < 0 {
		return -1
	}
	if idx == 0 {
		return idx
	}
	if previous := lower[idx-1]; previous == ';' || previous == ' ' || previous == '\t' {
		return idx
	}
	return -1
}

func hashSeal(c dkim.Canonicalization, fields dkim.Header, keys string, sigField string) ([]byte, error) {
	return hashMessageSignature(c, fields, keys, sigField)
}

func decodeBase64Tag(value string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == '\r' || r == '\n' {
			return -1
		}
		return r
	}, value))
}
