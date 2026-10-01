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

## Editing documents

`Tomlrb::Document` edits TOML in place while preserving the original text:
comments, whitespace, quoting style and number formats of untouched regions
are kept byte-for-byte.

```ruby
doc = Tomlrb::Document.load_file('config.toml')

doc.set('database.pool', 10)                 # replace a value in place
doc.set('servers[1].weight', 2500)           # 2nd [[servers]] instance
doc.rename('database.url', 'dsn')            # rename a key
doc.move('server.timeout', 'database')       # move a key across tables
doc.move('server.host', 'database', as: 'listen') # move and rename

File.write('config.toml', doc.to_s)
```

* Paths are dot-separated strings (`'servers[1].host'`, quoting supported) or
  arrays (`['servers', 1, 'host']`); integer segments address specific
  `[[array-of-tables]]` instances.
* `set` accepts strings, numbers, booleans, arrays, hashes (serialized as
  inline tables) and date/time objects. Wrap a string in
  `Tomlrb::Document::Raw` to insert TOML verbatim, e.g.
  `doc.set('server.port', Tomlrb::Document::Raw.new('0x1F91'))`.
* `move` inserts the pair right after the target table's header, so the
  table-header scope and dotted-key interpretation of neighbouring keys are
  never disturbed. Missing target tables are appended as new `[table]`
  sections.
* Every edit is validated against a full re-parse. Edits that would cause
  duplicate declarations or type conflicts raise
  `Tomlrb::Document::EditError` and leave the document unchanged.

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
