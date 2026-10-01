# MultiJSON has moved

This project has moved to https://github.com/sferik/multi_json

## Preserving number precision (`:preserve_numbers`)

JSON numbers that exceed Float precision (such as
`123456789012345678901234567890.123456789`) or whose spelling carries meaning
(`1.2300` vs `123e-2`) are silently rounded the moment they become a Float.
Pass `preserve_numbers: true` to `MultiJson.load` to receive
`MultiJson::LexicalNumber` objects instead:

```ruby
value = MultiJson.load("1.2300", preserve_numbers: true)
value.class    # MultiJson::LexicalNumber
value.lexeme   # "1.2300" (the exact source text)
value.is_a?(Numeric) # true, but it is not a String

# Exact, explicit conversions
value.to_r     # (123/100) Rational
value.to_d     # 0.123e1   BigDecimal (require "bigdecimal")
value.to_i     # RangeError unless the value is an integer
value.to_f     # 1.23 (may round, like any Float)
```

LexicalNumber is a Numeric (never a String), so `MultiJson.dump` writes it back
as a legal, **unquoted** number. Only text matching the JSON number grammar can
be stored, so arbitrary text cannot be injected through this channel:

```ruby
MultiJson::LexicalNumber.new('1.2, "evil"') # raises ArgumentError

doc = MultiJson.load('{"price":1.2300}', preserve_numbers: true)
MultiJson.dump(doc) # '{"price":1.2300}'
```

`==` compares exact mathematical values (`1.0 == 1.00`), while `eql?`/`hash`
keep the spellings distinct (`"1.0"` and `"1.00"` are different Hash keys).

### Adapter support

| Adapter | `:preserve_numbers` |
|---------|--------------------|
| `json_gem`, `json_pure`, `ok_json` | Supported |
| `oj`, `yajl`, `gson`, `jr_jackson` | Raises `MultiJson::UnsupportedOptionError` |

A backend that can only hand back Floats refuses the option rather than
rounding first and claiming the precision survived. For `json_gem`/`json_pure`,
decimal and exponent numbers keep their exact source text; integral numbers
are parsed by the backend's own arbitrary-precision Integer and wrapped with a
canonical spelling (still exact, never a Float).

