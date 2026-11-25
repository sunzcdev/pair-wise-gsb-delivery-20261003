require_relative "../test_helper"

module SemanticLogger
  module Formatters
    class EcsTest < Minitest::Test
      describe Ecs do
        let(:log_time) do
          Time.utc(2017, 1, 14, 8, 32, 5.375276)
        end

        let(:level) do
          :debug
        end

        let(:log) do
          log      = SemanticLogger::Log.new("EcsTest", level)
          log.time = log_time
          log
        end

        # Stand-in for the appender / subscriber that owns host/application/environment.
        let(:appender) do
          Struct.new(:host, :application, :environment).new("test_host", "test_app", "test_env")
        end

        let(:formatter) do
          SemanticLogger::Formatters::Ecs.new
        end

        let(:result) do
          JSON.parse(formatter.call(log, appender), symbolize_names: true)
        end

        describe "core fields" do
          it "emits an ISO-8601 @timestamp" do
            assert_equal "2017-01-14T08:32:05.375276Z", result[:@timestamp]
          end

          it "emits the ecs version" do
            assert_equal SemanticLogger::Formatters::Ecs::ECS_VERSION, result.dig(:ecs, :version)
          end

          it "emits the level and logger name" do
            assert_equal "debug", result.dig(:log, :level)
            assert_equal "EcsTest", result.dig(:log, :logger)
          end

          it "emits the message" do
            log.message = "Hello World"

            assert_equal "Hello World", result[:message]
          end
        end

        describe "process, host, service" do
          it "maps pid and thread name under process" do
            assert_kind_of Integer, result.dig(:process, :pid)
            assert_equal log.thread_name, result.dig(:process, :thread, :name)
          end

          it "maps host to host.hostname" do
            assert_equal "test_host", result.dig(:host, :hostname)
          end

          it "maps application and environment to service" do
            assert_equal "test_app", result.dig(:service, :name)
            assert_equal "test_env", result.dig(:service, :environment)
          end
        end

        describe "origin" do
          it "maps the file, line, and method to log.origin" do
            log.call_site = SemanticLogger::CallSite.new(path: "/app/models/order.rb", line: 42, label: "Order#submit")

            expected = {file: {name: "/app/models/order.rb", line: 42}, function: "Order#submit"}

            assert_equal expected, result.dig(:log, :origin)
          end

          it "omits the function without a call site" do
            log.backtrace = ["/app/models/order.rb:42:in 'Order#submit'"]

            assert_equal({file: {name: "/app/models/order.rb", line: 42}}, result.dig(:log, :origin))
          end
        end

        describe "duration" do
          it "converts milliseconds to event.duration in nanoseconds" do
            log.duration = 1.5

            assert_equal 1_500_000, result.dig(:event, :duration)
          end

          it "omits event.duration when not set" do
            refute result.key?(:event)
          end
        end

        describe "tags and named_tags" do
          it "emits tags as a top-level array" do
            log.tags = %w[first second]

            assert_equal %w[first second], result[:tags]
          end

          it "maps named_tags to labels" do
            log.named_tags = {user_id: 5, account: "acme"}

            assert_equal({user_id: 5, account: "acme"}, result[:labels])
          end

          it "omits labels when empty" do
            refute result.key?(:labels)
          end
        end

        describe "exception" do
          it "maps to error.type / error.message / error.stack_trace" do
            begin
              raise "Oh no"
            rescue StandardError => e
              log.exception = e
            end

            assert_equal "RuntimeError", result.dig(:error, :type)
            assert_equal "Oh no", result.dig(:error, :message)
            assert_kind_of String, result.dig(:error, :stack_trace)
          end

          it "writes a stack trace whose lines are in encodings that cannot be joined" do
            exception = RuntimeError.new("Oh no")
            exception.set_backtrace(["app/café.rb:1:in 'm'", "lib/x.rb:2:in 'n'".encode(Encoding::UTF_16LE)])
            log.exception = exception

            assert_equal "app/café.rb:1:in 'm'\nlib/x.rb:2:in 'n'", result.dig(:error, :stack_trace)
          end

          it "omits error when not set" do
            refute result.key?(:error)
          end
        end

        describe "namespace for non-ECS data" do
          it "nests payload, metric, and metric_amount under the default namespace" do
            log.payload       = {first: 1, second: 2}
            log.metric        = "users/registered"
            log.metric_amount = 3

            assert_equal({first: 1, second: 2}, result.dig(:semantic_logger, :payload))
            assert_equal "users/registered", result.dig(:semantic_logger, :metric)
            assert_equal 3, result.dig(:semantic_logger, :metric_amount)
          end

          it "nests the metric's dimensions under the namespace" do
            log.metric     = "users/registered"
            log.dimensions = {region: "us"}

            assert_equal({region: "us"}, result.dig(:semantic_logger, :dimensions))
          end

          it "uses a custom namespace" do
            formatter = SemanticLogger::Formatters::Ecs.new(namespace: "my_app")
            log.payload = {first: 1}
            parsed = JSON.parse(formatter.call(log, appender), symbolize_names: true)

            assert_equal({first: 1}, parsed.dig(:my_app, :payload))
          end

          it "merges payload into labels when namespace is nil" do
            formatter = SemanticLogger::Formatters::Ecs.new(namespace: nil)
            log.payload    = {first: 1}
            log.named_tags = {user_id: 5}
            parsed = JSON.parse(formatter.call(log, appender), symbolize_names: true)

            assert_equal 1, parsed.dig(:labels, :payload, :first)
            assert_equal 5, parsed.dig(:labels, :user_id)
            refute parsed.key?(:semantic_logger)
          end

          it "omits the namespace when there is no extra data" do
            refute result.key?(:semantic_logger)
          end
        end

        describe "top-level merge conflicts" do
          # Two entries in one batch share a payload that holds an array, the
          # named tags write `service` as a scalar, the payload provides
          # `service.name`, and the appender gives ECS its own valid `service`.
          let(:items) do
            [1, 2, 3]
          end

          let(:payload) do
            {service: {name: "payload_service"}, items: items}
          end

          let(:logs) do
            Array.new(2) do
              entry             = SemanticLogger::Log.new("EcsTest", level)
              entry.time        = log_time
              entry.message     = "Hello World"
              entry.named_tags  = {service: "billing"}
              entry.payload     = payload
              entry
            end
          end

          let(:ecs_first) do
            SemanticLogger::Formatters::Ecs.new(merge: :top_level, on_conflict: :ecs)
          end

          let(:log_first) do
            SemanticLogger::Formatters::Ecs.new(merge: :top_level, on_conflict: :log)
          end

          let(:ecs_batch) do
            JSON.parse(ecs_first.batch(logs, appender), symbolize_names: true)
          end

          let(:log_batch) do
            JSON.parse(log_first.batch(logs, appender), symbolize_names: true)
          end

          it "keeps the ECS service with :ecs priority and relocates the losers to their homes" do
            ecs_batch.each do |entry|
              # The ECS mapping's own service wins the field.
              assert_equal "test_app", entry.dig(:service, :name)
              assert_equal "test_env", entry.dig(:service, :environment)

              # The payload's service.name moved to the payload's namespaced home.
              assert_equal "payload_service", entry.dig(:semantic_logger, :payload, :service, :name)

              # The named tag's scalar, which cannot displace the ECS service
              # object, moved to the named tags' home.
              assert_equal "billing", entry.dig(:labels, :service)

              # Non-conflicting payload keys merge at the top level.
              assert_equal [1, 2, 3], entry[:items]
            end
          end

          it "keeps the payload's service.name with :log priority, deep-merged with the ECS service" do
            log_batch.each do |entry|
              # The payload won service.name; the ECS mapping kept service.environment.
              assert_equal "payload_service", entry.dig(:service, :name)
              assert_equal "test_env", entry.dig(:service, :environment)

              # The named tag's scalar still cannot displace the service object.
              assert_equal "billing", entry.dig(:labels, :service)

              # The payload won, so nothing of it was relocated.
              refute entry.key?(:semantic_logger)

              assert_equal [1, 2, 3], entry[:items]
            end
          end

          it "formats both entries of the batch identically even though they share one payload" do
            assert_equal ecs_batch.first, ecs_batch.last
            assert_equal log_batch.first, log_batch.last
          end

          it "leaves the shared payload, its array, and the named tags untouched" do
            ecs_batch
            log_batch

            assert_equal({service: {name: "payload_service"}, items: [1, 2, 3]}, payload)
            assert_same items, payload[:items]
            logs.each do |entry|
              assert_same payload, entry.payload
              assert_equal({service: "billing"}, entry.named_tags)
            end
          end

          it "still reads the original log by its own rules with the plain JSON formatter" do
            ecs_batch
            log_batch

            json = SemanticLogger::Formatters::Json.new
            logs.each do |entry|
              parsed = JSON.parse(json.call(entry, appender), symbolize_names: true)

              assert_equal "payload_service", parsed.dig(:payload, :service, :name)
              assert_equal [1, 2, 3], parsed.dig(:payload, :items)
              assert_equal "billing", parsed.dig(:named_tags, :service)
            end
          end

          it "always relocates protected ECS fields, whichever source is preferred" do
            logs.each { |entry| entry.payload = payload.merge("@timestamp": "2099-01-01T00:00:00Z", ecs: {version: "0.0"}) }

            [ecs_first, log_first].each do |formatter|
              parsed = JSON.parse(formatter.call(logs.first, appender), symbolize_names: true)

              # The protected fields keep the ECS mapping's values.
              assert_equal "2017-01-14T08:32:05.375276Z", parsed[:@timestamp]
              assert_equal SemanticLogger::Formatters::Ecs::ECS_VERSION, parsed.dig(:ecs, :version)

              # The colliding values moved to the payload's home.
              assert_equal "2099-01-01T00:00:00Z", parsed.dig(:semantic_logger, :payload, :@timestamp)
              assert_equal({version: "0.0"}, parsed.dig(:semantic_logger, :payload, :ecs))
            end
          end

          it "keeps the metric fields under the namespace" do
            logs.each do |entry|
              entry.metric        = "users/registered"
              entry.metric_amount = 3
            end

            assert_equal "users/registered", ecs_batch.first.dig(:semantic_logger, :metric)
            assert_equal 3, ecs_batch.first.dig(:semantic_logger, :metric_amount)
          end

          it "relocates the payload to labels when the namespace is nil" do
            formatter = SemanticLogger::Formatters::Ecs.new(merge: :top_level, namespace: nil)
            parsed    = JSON.parse(formatter.call(logs.first, appender), symbolize_names: true)

            assert_equal "test_app", parsed.dig(:service, :name)
            assert_equal "payload_service", parsed.dig(:labels, :service, :name)
          end

          it "rejects an unknown merge or on_conflict option" do
            assert_raises(ArgumentError) { SemanticLogger::Formatters::Ecs.new(merge: :sideways) }
            assert_raises(ArgumentError) { SemanticLogger::Formatters::Ecs.new(on_conflict: :nobody) }
          end
        end

        describe "JSON encoding" do
          it "returns a single line of valid JSON" do
            log.message    = "Hello World"
            log.payload    = {first: 1}
            log.named_tags = {user_id: 5}

            json = formatter.call(log, appender)

            assert_kind_of String, json
            refute_includes json, "\n"

            parsed = JSON.parse(json)

            assert_equal "Hello World", parsed["message"]
            assert_equal SemanticLogger::Formatters::Ecs::ECS_VERSION, parsed.dig("ecs", "version")
            assert_equal 1, parsed.dig("semantic_logger", "payload", "first")
            assert_equal 5, parsed.dig("labels", "user_id")
          end

          it "batches events into a JSON array" do
            json = formatter.batch([log, log], appender)
            parsed = JSON.parse(json)

            assert_equal 2, parsed.size
            assert_equal "debug", parsed.first.dig("log", "level")
          end
        end
      end
    end
  end
end
