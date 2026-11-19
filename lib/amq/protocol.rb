# -*- coding: utf-8 -*-

require "amq/protocol/version"
require "amq/protocol/client"
require "amq/protocol/channel_close"
require "amq/protocol/registry"
require "amq/protocol/spec/builtin"
require "amq/protocol/generator"

module AMQ
  module Protocol
    # Versioned protocol descriptions.
    #
    # The module above (AMQ::Protocol) keeps the default 0.9.1 namespace that
    # predates versioning, so existing callers are unaffected. Use
    # AMQ::Protocol::Registry to load descriptions as separate namespaces:
    #
    #   version = AMQ::Protocol::Registry.load("0.9.1", paths: ["amqp-0.9.1.json"])
    #   version.method(60, 40)          # metadata for basic.publish
    #   version.namespace::Basic::Publish.decode(payload)
    #
    # Loading reads each file once; descriptions and namespaces hold no IO.
    def self.load_version(version, **options)
      Registry.load(version, **options)
    end

    # Returns an already loaded version, raising KeyError if it is missing.
    def self.version(version)
      Registry.fetch(version)
    end

    # Bundled protocol descriptions.
    def self.descriptions
      Spec::Builtin
    end
  end
end
