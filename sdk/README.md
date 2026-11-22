# opentelemetry-sdk

The `opentelemetry-sdk` gem provides the reference implementation of the OpenTelemetry Ruby API. Using `opentelemetry-sdk`, an application can collect, analyze, and export telemetry data such as distributed traces and metrics.

## What is OpenTelemetry?

[OpenTelemetry][opentelemetry-home] is an open source observability framework, providing a general-purpose API, SDK, and related tools required for the instrumentation of cloud-native software, frameworks, and libraries.

OpenTelemetry provides a single set of APIs, libraries, agents, and collector services to capture distributed traces and metrics from your application. You can analyze them using Prometheus, Jaeger, and other observability tools.

## How does this gem fit in?

The `opentelemetry-sdk` gem provides the reference implementation of the OpenTelemetry Ruby interfaces defined in the `opentelemetry-api` gem. That is, it includes the *functionality* needed to collect, analyze, and export telemetry data produced using the API.

Generally, Ruby *applications* should install `opentelemetry-sdk` (or
other concrete implementation of the OpenTelemetry API). Using the SDK,
an application can configure how it wants telemetry data to be handled,
including which data should be persisted, how it should be formatted,
and where it should be recorded or exported. However, *libraries* that
produce telemetry data should generally depend only on
`opentelemetry-api`, deferring the choice of concrete implementation to the application developer.

## How do I get started?

Install the gem using:

```sh
gem install opentelemetry-sdk
```

Or, if you use [bundler][bundler-home], include `opentelemetry-sdk` in your `Gemfile`.

Then, configure the SDK according to your desired handling of telemetry data, and use the OpenTelemetry interfaces to produces traces and other information. Following is a basic example.

```ruby
require 'opentelemetry/sdk'

# Configure the sdk with default export and context propagation formats.
OpenTelemetry::SDK.configure

# Many configuration options may be set via the environment. To use them,
# set the appropriate variable before calling configure. For example:
#
# ENV['OTEL_TRACES_EXPORTER'] = 'console'
# ENV['OTEL_PROPAGATORS'] = 'ottrace'
# OpenTelemetry::SDK.configure
#
# You may also configure the SDK programmatically, for advanced usage or to
# enable auto-instrumentation. For example:
#
# OpenTelemetry::SDK.configure do |c|
#   c.service_name = something_calculated_dynamically
#   c.add_span_processor(
#     OpenTelemetry::SDK::Trace::Export::SimpleSpanProcessor.new(
#       OpenTelemetry::SDK::Trace::Export::ConsoleSpanExporter.new
#     )
#   )
#
#   c.use 'OpenTelemetry::Instrumentation::Net::HTTP'
# end
#
# Note that the SimpleSpanProcessor is not recommended for use in production.


# To start a trace you need to get a Tracer from the TracerProvider
tracer = OpenTelemetry.tracer_provider.tracer('my_app_or_gem', '0.1.0')

# create a span
tracer.in_span('foo') do |span|
  # set an attribute
  span.set_attribute('platform', 'osx')
  # add an event
  span.add_event('event in bar')
  # create bar as child of foo
  tracer.in_span('bar') do |child_span|
    # inspect the span
    pp child_span
  end
end
```

For additional examples, see the [examples on github][examples-github].

## Retaining important events and links

A span silently drops the oldest events (and the newest links) once it reaches
its `event_count_limit` (or `link_count_limit`). Long-running tasks can fill
that capacity with low-value entries before the diagnostic information — an
exception event, the link to the originating job — is recorded.

`SpanLimits` accepts an opt-in `retention_policy` so callers can mark entries as
preferred and still respect the same count limits:

```ruby
require 'opentelemetry/sdk'

SpanLimits = OpenTelemetry::SDK::Trace::SpanLimits

# Keep preferred entries regardless of age, filling any spare capacity with
# the historical per-type defaults (newest events, oldest links).
span_limits = SpanLimits.new(
  event_count_limit: 4,
  link_count_limit: 2,
  retention_policy: :keep_preferred
)

# `OpenTelemetry::SDK.configure` does not expose span limits, so build the
# provider directly when a custom policy is required.
OpenTelemetry.tracer_provider = OpenTelemetry::SDK::Trace::TracerProvider.new(
  span_limits: span_limits
).tap do |provider|
  provider.add_span_processor(
    OpenTelemetry::SDK::Trace::Export::SimpleSpanProcessor.new(
      OpenTelemetry::SDK::Trace::Export::ConsoleSpanExporter.new
    )
  )
end

tracer = OpenTelemetry.tracer_provider.tracer('my_app_or_gem', '0.1.0')

tracer.in_span('long-running-task') do |span|
  span.add_event('progress', attributes: { 'step' => 1 })
  span.add_event('progress', attributes: { 'step' => 2 })
  # Mark the entries that matter. Preferred entries survive trimming.
  span.add_event('validation-failed', attributes: { 'reason' => 'timeout' }, preferred: true)
  # record_exception marks its event as preferred by default.
  span.record_exception(StandardError.new('boom'))
  # Links can be marked when created...
  span.add_link(OpenTelemetry::Trace::Link.new(OpenTelemetry::Trace.current_span.context, preferred: true))
  # ...or when added.
  span.add_link(OpenTelemetry::Trace::Link.new(OpenTelemetry::Trace.current_span.context), preferred: true)
end
```

Every policy still honours the configured count limits, and the retained entries
keep their original order. When the preferred entries alone exceed a limit, the
newest preferred entries are kept. The dropped totals are still reported on the
exported `SpanData` through `total_recorded_events` and `total_recorded_links`.

The accepted policies are:

| Policy            | Behaviour when the limit is reached                                              |
| ----------------- | -------------------------------------------------------------------------------- |
| `nil` (default)   | Historical behaviour: keep the newest events, keep the oldest links; markers ignored. |
| `:keep_newest`    | Keep the newest entries for both events and links.                               |
| `:keep_oldest`    | Keep the oldest entries for both events and links.                               |
| `:keep_preferred` | Keep preferred entries first, then fill spare capacity with the per-type default; newest preferred wins when over capacity. |

## How can I get involved?

The `opentelemetry-sdk` gem source is [on github][repo-github], along with related gems including `opentelemetry-api`.

The OpenTelemetry Ruby gems are maintained by the OpenTelemetry-Ruby special interest group (SIG). You can get involved by joining us in [GitHub Discussions][discussions-url] or attending our weekly meeting. See the [meeting calendar][community-meetings] for dates and times. For more information on this and other language SIGs, see the OpenTelemetry [community page][ruby-sig].

## License

The `opentelemetry-sdk` gem is distributed under the Apache 2.0 license. See [LICENSE][license-github] for more information.

[opentelemetry-home]: https://opentelemetry.io
[bundler-home]: https://bundler.io
[repo-github]: https://github.com/open-telemetry/opentelemetry-ruby
[license-github]: https://github.com/open-telemetry/opentelemetry-ruby/blob/main/LICENSE
[examples-github]: https://github.com/open-telemetry/opentelemetry-ruby/tree/main/examples
[ruby-sig]: https://github.com/open-telemetry/community#ruby-sig
[community-meetings]: https://github.com/open-telemetry/community/blob/main/README.md#calendar
[discussions-url]: https://github.com/open-telemetry/opentelemetry-ruby/discussions
