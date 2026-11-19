# frozen_string_literal: true

require "amq/protocol/spec"
require "amq/protocol/spec/loader"

module AMQ
  module Protocol
    module Spec
      # Protocol descriptions shipped with the gem.
      #
      # The documents are the unmodified rabbitmq-codegen JSON files, so the
      # bundled 0.9.1 description and the generated legacy namespace have one
      # common source of truth. Loading them yields IO-free Description
      # objects.
      module Builtin
        DATA_DIR = File.expand_path("data", __dir__)

        module_function

        # Absolute path of a bundled description document.
        def path(name)
          File.join(DATA_DIR, "#{name}.json")
        end

        # The RabbitMQ-flavoured AMQP 0.9.1 description this gem generates.
        def amqp_0_9_1
          @amqp_0_9_1 ||= Loader.load_file(path("amqp-rabbitmq-0.9.1"), label: "0.9.1")
        end

        # The historic AMQP 0.8 description, useful for side-by-side
        # comparisons with 0.9.1.
        def amqp_0_8
          @amqp_0_8 ||= Loader.load_file(path("amqp-rabbitmq-0.8"), label: "0.8.0")
        end

        # A small in-memory description with no file behind it, for tests that
        # do not need a full protocol.
        def demo(version: "1.0.0", port: 5679, tag: "demo")
          major, minor, revision = version.split(".").map { |part| Integer(part) }
          document = {
            "name" => "DemoAMQP",
            "major-version" => major,
            "minor-version" => minor,
            "revision" => revision,
            "port" => port,
            "domains" => [["label", "shortstr"]],
            "constants" => [
              { "name" => "FRAME-METHOD", "value" => 1 },
              { "name" => "FRAME-HEADER", "value" => 2 },
              { "name" => "FRAME-BODY", "value" => 3 },
              { "name" => "FRAME-HEARTBEAT", "value" => 8 },
              { "name" => "FRAME-END", "value" => 206 },
              { "name" => "DEMO-TAG", "value" => tag }
            ],
            "classes" => [
              {
                "name" => "demo",
                "id" => 100,
                "methods" => [
                  {
                    "name" => "ping",
                    "id" => 10,
                    "arguments" => [
                      { "domain" => "label", "name" => "label" },
                      { "type" => "bit", "name" => "loud", "default-value" => false },
                      { "type" => "bit", "name" => "repeat", "default-value" => false }
                    ]
                  },
                  {
                    "name" => "pong",
                    "id" => 11,
                    "arguments" => [
                      { "type" => "shortstr", "name" => "tag" },
                      { "type" => "longlong", "name" => "counter" }
                    ]
                  }
                ],
                "properties" => [
                  { "type" => "shortstr", "name" => "content-type" },
                  { "type" => "octet", "name" => "priority" }
                ]
              }
            ]
          }

          Loader.from_document(document, label: "demo-#{version}")
        end
      end
    end
  end
end
