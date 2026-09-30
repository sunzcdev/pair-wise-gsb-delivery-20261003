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

## Exact fractional seconds

By default, fractional seconds are parsed through Ruby floats, which can drift
for sub-nanosecond precision (for example `23:59:59.999999999999`). Pass
`exact_fractional_seconds: true` (or `exact: true`) to keep the fraction exact
using Integer/Rational arithmetic. The value is never carried over to the next
second, and trailing zeroes are normalized away.

```ruby
Tomlrb.parse('a = 23:59:59.999999999999', exact_fractional_seconds: true)
# Offset Date-Time values become Tomlrb::OffsetDateTime. Local Date-Time and
# Local Time keep their own classes. All of #==, #to_s and #to_time express the
# same exact value:
v = Tomlrb.parse('a = 1979-05-27T07:32:00.999999999999-07:00', exact: true)['a']
v.to_s      #=> "1979-05-27T07:32:00.999999999999-07:00"
v.to_time   #=> 1979-05-27 07:32:00 999999999999/1000000000000 -0700 (Time)
```

Local types still never attach a date or the machine time zone; call
`#to_time` explicitly to build a `Time`. The default return conventions are
unchanged when the option is omitted.

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
