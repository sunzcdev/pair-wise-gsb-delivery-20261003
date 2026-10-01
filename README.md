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

## Editing documents while preserving comments

Re-emitting a parsed Hash loses comments, spacing and the original spelling
of numbers and strings. `Tomlrb::Document` edits the source text instead,
touching only the ranges involved in an edit: comments, blank lines,
quoting and the spelling of untouched values are kept byte for byte.

```ruby
document = Tomlrb::Document.new(File.read('config.toml'))

document.set(['server', 'port'], 9090)               # change a value
document.rename(['server', 'port'], 'listen')         # rename a key
document.move(['pool', 'size'], ['server', 'workers']) # move across tables

# Array-of-tables instances are addressed with an integer segment.
document.set(['targets', 0, 'name'], 'alpha-renamed')

document.text # the edited document
```

Paths are arrays of strings (and integers for array-of-tables instances),
or dot-separated strings for bare keys.

Every edit is validated against the document structure and re-parsed
afterwards. Edits that would create a duplicate declaration or a type
conflict raise `Tomlrb::EditError` and leave the document untouched, so a
sequence of edits never produces a half-written file.

```ruby
begin
  document.rename(['server', 'host'], 'port') # 'port' already exists
rescue Tomlrb::EditError => error
  error.message # "server.port is already declared"
end
```

To rewrite a file atomically, use `Document.open`:

```ruby
Tomlrb::Document.open('config.toml') do |document|
  document.set(['server', 'port'], 9090)
end # written only if every edit in the block succeeded
```

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
