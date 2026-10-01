# Tomlrb

[![Code Climate](https://codeclimate.com/github/fbernier/tomlrb/badges/gpa.svg)](https://codeclimate.com/github/fbernier/tomlrb)
[![Gem Version](https://badge.fury.io/rb/tomlrb.svg)](http://badge.fury.io/rb/tomlrb)
[![Build status](https://github.com/fbernier/tomlrb/workflows/ci/badge.svg)](https://github.com/fbernier/tomlrb/actions)

A Racc based [TOML](https://github.com/toml-lang/toml) Ruby parser supporting the 1.0.0 version of the spec.

## TODO

* Dumper

## Installation

Add this line to your application's Gemfile:

```ruby
gem 'tomlrb'
```

And then execute:

    $ bundle

Or install it yourself as:

    $ gem install tomlrb

## Usage

```ruby
Tomlrb.parse("[toml]\na = [\"array\", 123]")
```

or

```ruby
Tomlrb.load_file('my_file', symbolize_keys: true)
```

## Streaming events

For use cases such as configuration auditing you usually only need to inspect
each declaration, its path and its completed value - building the whole
document Hash first is wasteful. `Tomlrb.each_event` reads a chunked IO
(anything answering to `read`, or one byte at a time through `getbyte`) and
yields structural events without materializing the output tree:

```ruby
File.open('huge.toml', 'rb') do |io|
  Tomlrb.each_event(io) do |event|
    name, path, value = event
    audit(path, value) if name == :key
  end
end
```

The chunk boundaries are arbitrary: a chunk may split a multi-byte UTF-8
character, an escape sequence or a (multi-line) string. Errors that can only
be decided once the input ends (unterminated string, dangling escape,
unclosed array/inline table/header, incomplete UTF-8 sequence) raise
`Tomlrb::ParseError` from the final read instead of being reported early.

### Event reference

Paths are arrays: strings name tables/keys, integers index array elements.

| Event | Meaning |
| --- | --- |
| `[:table_declare, path, kind]` | A table header. `kind` is `:standard` or `:array_element`. |
| `[:table_enter, path, mode]` | The active table becomes `path`. `mode` is `:standard`, `:array_element` or `:implicit` (a parent table created on behalf of a dotted header). |
| `[:table_leave, path]` | The active table is switched away from `path`. This is **not** the end of that table: a shared parent stays alive and may be re-entered, and an array-of-tables table is simply appended to when a later `[[name]]` header arrives. |
| `[:key, path, value]` | A completed scalar pair in the current table. |
| `[:array_start, path]` / `[:array_end, path]` | A value array; its scalar children follow as `:array_element` events (with integer indices in the path). |
| `[:array_element, path, value]` | A scalar element of an array. Nested arrays/inline tables carry their own start/end events. |
| `[:inline_table_start, path]` / `[:inline_table_end, path]` | An inline value table. |
| `[:inline_table_entry, path, value]` | A completed scalar pair inside an inline table. |

Container nesting is expressed purely through paths: a nested container's
path extends its parent's path. An inline table nested in an array has an
integer segment, and a normal table defined with a dotted header under an
array-of-tables keeps that array's element index in its path.

### Memory behaviour

Every completed value is handed to the block exactly once and then dropped
internally; the parser keeps only the state needed to finish validation:

- the duplicate-declaration key trie (key names only, never values),
- the active table path and array-of-tables element counters,
- currently open arrays/inline tables with their indices,
- grammar markers for the constructs still being parsed.

So a consumer that processes and forgets each event can stream arbitrarily
large values with bounded retained memory. `Tomlrb.parse` is unchanged and
still builds and returns the complete Hash.

## Benchmark

You can run the benchmark against the only other v0.5.0 compliant parser to my knowledge with `ruby benchmarks/bench.rb`.

Here are the results on my machine:

```
Warming up --------------------------------------
      emancu/toml-rb     1.000  i/100ms
     fbernier/tomlrb    33.000  i/100ms
Calculating -------------------------------------
      emancu/toml-rb     15.597  (± 6.4%) i/s -     78.000  in   5.020321s
     fbernier/tomlrb    348.307  (± 5.2%) i/s -      1.749k in   5.034878s

Comparison:
     fbernier/tomlrb:      348.3 i/s
      emancu/toml-rb:       15.6 i/s - 22.33x  (± 0.00) slower

```

## Development

After checking out the repo, run `bin/setup` to install dependencies. Then, run `bin/console` for an interactive prompt that will allow you to experiment.

Do not forget to regenerate the parser when you modify rules in the `parser.y` file using `rake compile`.

Run the tests using `rake test`.

To install this gem onto your local machine, run `bundle exec rake install`. To release a new version, update the version number in `version.rb`, and then run `bundle exec rake release` to create a git tag for the version, push git commits and tags, and push the `.gem` file to [rubygems.org](https://rubygems.org).

## Contributing

1. Fork it ( https://github.com/[my-github-username]/tomlrb/fork )
2. Create your feature branch (`git checkout -b my-new-feature`)
3. Commit your changes (`git commit -am 'Add some feature'`)
4. Push to the branch (`git push origin my-new-feature`)
5. Create a new Pull Request

## Thanks

Thanks to [@jpbougie](https://github.com/jpbougie) for the crash course on  the Chomsky hierarchy and general tips.
