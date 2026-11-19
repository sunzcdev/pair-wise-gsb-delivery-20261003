# What is amq-protocol

amq-protocol is an AMQP 0.9.1 serialization library for Ruby. It is not an
AMQP 0-9-1 client such as [Bunny](http://rubybunny.info): amq-protocol only handles serialization and deserialization.

If you want to write your own AMQP 0-9-1 client, this gem will handle all the serialization
needs for you, including RabbitMQ extensions to AMQP 0.9.1.


## Supported Ruby Versions

 * amq-protocol `2.9.0` requires Ruby 3.2 or later
 * amq-protocol `2.8.0` requires Ruby 3.0 or later
 * amq-protocol `2.3.4` has fixes for Ruby 3.5 compatibility
 * amq-protocol `2.3.3` has fixes for Ruby 3.4 compatibility
 * amq-protocol `2.3.0` only supports Ruby 2.2+.
 * amq-protocol `2.0.0` through `2.2.0` and later supports Ruby 2.0+.
 * amq-protocol `1.9.2` was the last version to support Ruby 1.8 and 1.9.


## Installation

    gem install amq-protocol


## Development

Make sure you have Python, pip and the mako templating package installed:

    pip install mako

amq-protocol uses RabbitMQ protocol code generation library that is in Python, so there is some
Python involved in the build.

To regenerate `lib/amq/protocol/client.rb` from the source (`codegen/*` files), run

    ./generate.rb

To make changes, **do not edit client.rb directly**. Instead, edit the `codegen/protocol.rb.pytemplate` and regenerate.

To run tests, use

    bundle install --binstubs
    ./bin/rspec -c spec spec


## Working With Several Protocol Versions

`AMQ::Protocol` keeps the default 0.9.1 namespace this gem has always provided, so
existing code keeps working. Descriptions of a protocol version can also be loaded
as independent namespaces, which lets one process compare versions or speak
different versions on different connections:

``` ruby
v091 = AMQ::Protocol::Registry.load("0.9.1", paths: ["amqp-rabbitmq-0.9.1.json"])
v10  = AMQ::Protocol::Registry.load("1.0.0", paths: ["amqp-1.0.json"])

v091.method(60, 40).argument_names   # => [:ticket, :exchange, :routing_key, :mandatory, :immediate]
v10.method(60, 40).argument_names    # whatever that version declares

v091.namespace::Basic::Publish.decode(payload)
```

Each namespace holds its own generated classes, so class/method ID lookup, argument
metadata and encoding/decoding all belong to the version they were loaded from.
Versions do not redefine `AMQ::Protocol` constants, and therefore do not overwrite
each other.

 * Declared field and property order is preserved: it is wire semantics and is never
   sorted or otherwise rearranged
 * Loading the same description twice reuses one namespace; loading a different
   description under an already-loaded label raises `AMQ::Protocol::Registry::ConflictError`
 * Merging several documents for one version reports conflicting declarations
   (`AMQ::Protocol::Spec::ConflictError`) before anything is generated
 * Descriptions and namespaces hold no IO objects; files are read once while the
   description is built

The `amqp-protocol` bundle ships the upstream rabbitmq-codegen documents in
`lib/amq/protocol/spec/data`, including 0.8 and 0.9.1, so `AMQ::Protocol.descriptions.amqp_0_9_1`
and `AMQ::Protocol.descriptions.amqp_0_8` need no download. Small descriptions can be
built entirely in memory for tests:

``` ruby
description = AMQ::Protocol::Spec::Builtin.demo(version: "1.0.0")
```

Generating a version namespace from a description is deterministic:

    ./generate.rb --versioned                                  # emit a version namespace
    ./generate.rb --describe lib/amq/protocol/spec/data/amqp-rabbitmq-0.9.1.json

    rake verify_descriptions                                    # assert stable manifests


## Maintainer Information

amq-protocol is maintained by [Michael Klishin](https://github.com/michaelklishin).


## Issues

Please report any issues you may find to our [Issue tracker](http://github.com/ruby-amqp/amq-protocol/issues) on GitHub.

## Mailing List

Any questions you may have should be sent to the [Ruby AMQP mailing list](http://groups.google.com/group/ruby-amqp).

## License

MIT (see LICENSE in the repository root).
