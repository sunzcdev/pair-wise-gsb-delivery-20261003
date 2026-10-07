# go-vcard

[![Go Reference](https://pkg.go.dev/badge/github.com/emersion/go-vcard.svg)](https://pkg.go.dev/github.com/emersion/go-vcard)

A Go library to parse and format [vCard](https://tools.ietf.org/html/rfc6350).

## Usage

```go
f, err := os.Open("cards.vcf")
if err != nil {
	log.Fatal(err)
}
defer f.Close()

dec := vcard.NewDecoder(f)
for {
	card, err := dec.Decode()
	if err == io.EOF {
		break
	} else if err != nil {
		log.Fatal(err)
	}

	log.Println(card.PreferredValue(vcard.FieldFormattedName))
}
```

## Selecting localized properties

A card can carry several translations of the same property (linked by the
`ALTID` parameter) as well as several `TYPE` purposes (e.g. `work`/`home`)
with different `PREF` values. `Selector` picks the right field instead of
leaving the caller to guess:

```go
// Prefer Simplified Chinese with English fallback, and prefer work numbers.
sel := &vcard.Selector{
	Languages: []string{"zh-Hans", "en"},
	Types:     []string{"work"},
}

res := sel.Select(card, vcard.FieldAddress)
if res.Match == vcard.LangNone {
	log.Println("no address in a preferred language; using a fallback")
}
// res.Fields holds the selected ADR as a whole, never a mix of translations.

// Fully co-equal candidates can either all be returned (the default) or be
// collapsed by a caller-selected stable rule.
first := (&vcard.Selector{Ties: vcard.TieFirst}).Select(card, vcard.FieldTelephone)
log.Println(first.Field().Value)
```

Language matching follows [BCP 47](https://tools.ietf.org/html/rfc4647):
basic filtering (`zh` matches `zh-Hans-CN`), lookup fallback (`zh-Hans`
preference falls back to a `zh` field), the `*` wildcard, and case-insensitive
comparison. All other parameters are preserved on the returned fields.

## License

MIT
