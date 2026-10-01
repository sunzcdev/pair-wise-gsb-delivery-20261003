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

`Tomlrb.parse` reads the whole input and builds the whole document as a
nested `Hash`. When that is not desirable - for example when auditing a
large configuration file value by value - `Tomlrb.each_event` reads from
any IO-like object (anything responding to `read(n)`, in chunks of
`chunk_size` bytes) and yields parse events instead:

```ruby
Tomlrb.each_event(File.open('my_file')) do |event|
  case event
  in [:table, path]            then # a [table] declaration
  in [:array_of_tables, path]  then # a [[table]] declaration, opening a new element
  in [:value, path, value]     then # a completed key/value pair
  end
end
```

Protocol notes:

* There is intentionally no "table closed" event. A table declaration
  only switches the context that following `:value` events belong to; it
  does not mean the previously current table is finished forever. A
  normal table may be declared *after* values under its path were
  already emitted (`[a.b]` ... `[a]`), and an array of tables may gain a
  new element (and new sub-tables) at any later `[[...]]` header.
* Only explicit `[...]`/`[[...]]` headers produce declaration events.
  Tables created implicitly on the way to a declared one do not.
* Nested containers (arrays, inline tables) are delivered as a single
  `:value` event once the container is complete.
* A value is yielded exactly once and never retained by the parser: the
  only state kept is what syntax and duplicate-declaration checks
  require (key names, container kinds, the current table path), so
  processed values can be garbage collected.
* Chunk boundaries may fall anywhere, including inside a UTF-8
  character, an escape sequence or a multi-line string. Errors that can
  only be detected at end of input (unterminated string, truncated
  UTF-8) are raised as `Tomlrb::ParseError` once the IO is exhausted.

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
