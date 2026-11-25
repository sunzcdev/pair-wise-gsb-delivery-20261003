# Shoryuken

Shoryuken _sho-ryu-ken_ is a super-efficient [Amazon SQS](https://aws.amazon.com/sqs/) thread-based message processor.

[![Build Status](https://github.com/ruby-shoryuken/shoryuken/workflows/Specs/badge.svg)](https://github.com/ruby-shoryuken/shoryuken/actions)
[![Join the chat at https://slack.shoryuken.io](https://raw.githubusercontent.com/karafka/misc/master/slack.svg)](https://slack.shoryuken.io)

## Key features

- [Rails Active Job](https://github.com/ruby-shoryuken/shoryuken/wiki/Rails-Integration-Active-Job)
- [Queue Load balancing](https://github.com/ruby-shoryuken/shoryuken/wiki/Shoryuken-options#load-balancing)
- [Concurrency per queue](https://github.com/ruby-shoryuken/shoryuken/wiki/Processing-Groups)
- [Long Polling](https://github.com/ruby-shoryuken/shoryuken/wiki/Long-Polling)
- [Batch processing](https://github.com/ruby-shoryuken/shoryuken/wiki/Worker-options#batch)
- [Auto extend visibility timeout](https://github.com/ruby-shoryuken/shoryuken/wiki/Worker-options#auto_visibility_timeout)
- [Exponential backoff](https://github.com/ruby-shoryuken/shoryuken/wiki/Worker-options#retry_intervals)
- [Middleware support](https://github.com/ruby-shoryuken/shoryuken/wiki/Middleware)
- Amazon SQS CLI. See `shoryuken help sqs`
- Configuration check without processing. See `shoryuken help check`

## Requirements

Ruby 3.3 or greater.

## Installation

Add this line to your application's Gemfile:

```ruby
gem 'shoryuken'
```

And then execute:

```shell
$ bundle
```

## Usage

Check the [Getting Started](https://github.com/ruby-shoryuken/shoryuken/wiki/Getting-Started) page.

## Failure handoff for non-retryable exceptions

Messages matched by `non_retryable_exceptions` are deleted immediately by default.
For workers that should first transfer those permanent failures to another system,
configure an optional failure-handoff adapter:

```ruby
class PaymentsWorker
  include Shoryuken::Worker

  shoryuken_options queue: 'payments',
                    non_retryable_exceptions: [ArgumentError],
                    failure_handoff: Payments::FailureDestination.new

  def perform(sqs_msg, body)
    # ...
  end
end
```

The destination adapter provides idempotent admission and result lookup keyed by
the **source queue and source message id** (never the SQS receipt handle, which
changes on redelivery):

```ruby
class Payments::FailureDestination
  # Idempotent; called with a Shoryuken::FailureHandoff::Identity and Record.
  # Return :accepted/:rejected (or a Result/boolean/status hash).
  def accept_failure(identity, record)
    # ...
    :accepted
  end

  # Return :accepted/:rejected, nil/:not_found, or raise when unavailable.
  def failure_result(identity)
    # ...
  end
end
```

Shoryuken deletes the current source receipt only when admission is confirmed.
When the admission response is lost or the result cannot be resolved, and when
the destination explicitly rejects the failure, the source message is left in
the queue. On redelivery (a new receipt, same message id) the same stable
identity is reused to look up the original admission, so the destination keeps a
single record and only the currently valid receipt is deleted. Workers without
`failure_handoff` keep the original immediate-delete behavior. A global adapter
can be set with `Shoryuken.failure_handoff = adapter`.

## More Information

For more information check the [wiki page](https://github.com/ruby-shoryuken/shoryuken/wiki).

## Credits

[Mike Perham](https://github.com/mperham), creator of [Sidekiq](https://github.com/mperham/sidekiq), and [everybody who contributed to it](https://github.com/mperham/sidekiq/graphs/contributors). Shoryuken wouldn't exist as it is without those contributions.

## License

Copyright (c) Pablo Cantero\
Copyright (c) Maciej Mensfeld\
Copyright (c) [Shoryuken contributors](https://github.com/ruby-shoryuken/shoryuken/graphs/contributors)

Shoryuken is an Open Source project licensed under the terms of the LGPLv3 license. See [LICENSE](LICENSE) for the license text.

## Contributing

1. Fork it ( https://github.com/ruby-shoryuken/shoryuken/fork )
2. Create your feature branch (`git checkout -b my-new-feature`)
3. Commit your changes (`git commit -am 'Add some feature'`)
4. Push to the branch (`git push origin my-new-feature`)
5. Create a new Pull Request

### Testing

To run all unit specs against the latest dependency versions, execute

```sh
bundle exec rake spec
```

To run integration specs (including Rails tests), start ElasticMQ and run:

```sh
docker compose up -d
bundle exec rake spec:integration
```

### To release a new version

Compare latest tag with HEAD:

```sh
git log $(git describe --tags --abbrev=0)..HEAD --oneline
```

then update CHANGELOG.md.

Update version in `lib/shoryuken/version.rb` with the appropriate version number [SEMVER](https://semver.org/).

then run:

```sh
bundle exec rake release
```
