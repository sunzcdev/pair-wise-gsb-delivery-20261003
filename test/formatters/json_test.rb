require_relative "../test_helper"

module SemanticLogger
  module Formatters
    class JsonTest < Minitest::Test
      describe Json do
        let(:log_time) do
          Time.utc(2017, 1, 14, 8, 32, 5.375276)
        end

        let(:level) do
          :debug
        end

        let(:log) do
          log      = SemanticLogger::Log.new("JsonTest", level)
          log.time = log_time
          log
        end

        let(:appender) do
          Struct.new(:host, :application, :environment).new("test_host", "test_app", "test_env")
        end

        let(:formatter) do
          SemanticLogger::Formatters::Json.new
        end

        let(:parsed) do
          JSON.parse(formatter.call(log, appender))
        end

        it "returns a JSON string" do
          result = formatter.call(log, appender)

          assert_kind_of String, result
          assert JSON.parse(result)
        end

        it "uses :timestamp as the default time key" do
          assert parsed.key?("timestamp")
          refute parsed.key?("time")
        end

        it "formats the time as iso_8601 by default" do
          assert_equal "2017-01-14T08:32:05.375276Z", parsed["timestamp"]
        end

        it "honours a custom time_key" do
          formatter = SemanticLogger::Formatters::Json.new(time_key: :ts)
          parsed    = JSON.parse(formatter.call(log, appender))

          assert parsed.key?("ts")
          refute parsed.key?("timestamp")
        end

        it "honours a custom time_format" do
          formatter = SemanticLogger::Formatters::Json.new(time_format: "%Y-%m-%d")
          parsed    = JSON.parse(formatter.call(log, appender))

          assert_equal "2017-01-14", parsed["timestamp"]
        end

        it "serializes the standard fields" do
          log.message    = "Hello World"
          log.payload    = {first: 1, second: 2}
          log.tags       = %w[first second]
          log.named_tags = {user: "joe"}

          assert_equal "debug",          parsed["level"]
          assert_equal log.level_index,  parsed["level_index"]
          assert_equal "JsonTest",       parsed["name"]
          assert_equal "Hello World",    parsed["message"]
          assert_equal $$,               parsed["pid"]
          assert_equal({"first" => 1, "second" => 2}, parsed["payload"])
          assert_equal %w[first second], parsed["tags"]
          assert_equal({"user" => "joe"}, parsed["named_tags"])
        end

        it "serializes an exception" do
          begin
            raise "Oh no"
          rescue Exception => e
            log.exception = e
          end

          assert_equal "RuntimeError", parsed["exception"]["name"]
          assert_equal "Oh no",        parsed["exception"]["message"]
        end

        it "serializes an exception whose #message raises, with a cause of an anonymous class" do
          cause_class = Class.new(StandardError)
          begin
            begin
              raise cause_class, "disk full"
            rescue cause_class
              raise ExceptionFixtures::AbstractMessageError, "Order 7"
            end
          rescue ExceptionFixtures::AbstractMessageError => e
            log.exception = e
          end

          assert_equal "ExceptionFixtures::AbstractMessageError", parsed["exception"]["name"]
          assert_equal "Order 7", parsed["exception"]["message"]
          assert_equal cause_class.to_s, parsed["exception"]["cause"]["name"]
          assert_equal "disk full", parsed["exception"]["cause"]["message"]
        end

        # Issue #180: previously `.to_json` raised on non UTF-8 data (the exact class
        # varies by json gem version) and the log entry was dropped. Every field that
        # can carry caller-supplied strings must now serialize without raising.
        describe "non-UTF-8 data (issue #180)" do
          # A binary string holding a byte (0xE2) that is not valid UTF-8.
          let(:bad) { "Bad: \xE2".b }

          it "serializes host, application, and environment" do
            appender = Struct.new(:host, :application, :environment).new("host\xE2".b, "app\xE2".b, "env\xE2".b)
            parsed   = JSON.parse(formatter.call(log, appender))

            assert_equal "host", parsed["host"]
            assert_equal "app",  parsed["application"]
            assert_equal "env",  parsed["environment"]
          end

          it "serializes the name" do
            log.name = bad

            assert_equal "Bad: ", parsed["name"]
          end

          it "serializes the thread name" do
            log.thread_name = bad

            assert_equal "Bad: ", parsed["thread"]
          end

          it "serializes the message, stripped as any message is" do
            log.message = bad

            assert_equal "Bad:", parsed["message"]
          end

          it "serializes the metric name" do
            log.metric        = bad
            log.metric_amount = 3

            assert_equal "Bad: ", parsed["metric"]
            assert_equal 3, parsed["metric_amount"]
          end

          it "serializes the metric's dimensions" do
            log.metric     = "user/login"
            log.dimensions = {region: bad}

            assert_equal({"region" => "Bad: "}, parsed["dimensions"])
          end

          it "serializes tags" do
            log.tags = ["good", bad]

            assert_equal ["good", "Bad: "], parsed["tags"]
          end

          it "serializes named_tag keys and values" do
            log.named_tags = {"key\xE2".b => bad}

            assert_equal({"key" => "Bad: "}, parsed["named_tags"])
          end

          it "serializes a nested payload, including keys and arrays" do
            log.payload = {"outer\xE2".b => {inner: bad, list: [bad]}}

            assert_equal({"outer" => {"inner" => "Bad: ", "list" => ["Bad: "]}}, parsed["payload"])
          end

          it "serializes the exception message and backtrace" do
            exception = RuntimeError.new(bad)
            exception.set_backtrace(["good_file.rb:1", "bad_file\xE2.rb:2".b])
            log.exception = exception

            assert_equal "Bad: ", parsed["exception"]["message"]
            assert_equal ["good_file.rb:1", "bad_file.rb:2"], parsed["exception"]["stack_trace"]
          end

          it "serializes a nested cause exception message" do
            begin
              begin
                raise bad
              rescue StandardError
                raise "the effect"
              end
            rescue Exception => e
              log.exception = e
            end

            assert_equal "Bad: ", parsed["exception"]["cause"]["message"]
          end

          it "serializes everything at once without raising" do
            log.message    = bad
            log.tags       = [bad]
            log.named_tags = {"key\xE2".b => bad}
            log.payload    = {"outer\xE2".b => {inner: bad}}

            assert JSON.parse(formatter.call(log, appender))
          end

          it "does not mangle valid multibyte UTF-8 in the same document" do
            log.message = "€ café"
            log.payload = {"naïve" => "Bad: \xE2".b}

            assert_equal "€ café", parsed["message"]
            assert_equal({"naïve" => "Bad: "}, parsed["payload"])
          end
        end

        # Binary data usually holds UTF-8, such as a response body or Base64-decoded text. Its
        # characters are kept, and only its invalid bytes dropped.
        describe "binary data that holds UTF-8" do
          # Valid UTF-8 and one invalid byte (0xFF), so that no json gem version writes it as it is.
          let(:binary) { "Café \xFF☕".b }

          it "keeps the characters of the message" do
            log.message = "Café ☕".b

            assert_equal "Café ☕", parsed["message"]
          end

          it "keeps the characters of the message, dropping its invalid bytes" do
            log.message = binary

            assert_equal "Café ☕", parsed["message"]
          end

          it "keeps the characters of tags and named tags" do
            log.tags       = [binary]
            log.named_tags = {"clé".b => binary}

            assert_equal ["Café ☕"], parsed["tags"]
            assert_equal({"clé" => "Café ☕"}, parsed["named_tags"])
          end

          it "keeps the characters of payload keys and values" do
            log.payload = {"clé".b => {inner: binary, list: [binary]}}

            assert_equal({"clé" => {"inner" => "Café ☕", "list" => ["Café ☕"]}}, parsed["payload"])
          end

          # Such as the keys of a request's binary text turned into Symbols.
          it "keeps the characters of Symbol keys made from binary text" do
            log.named_tags = {binary.to_sym => "v"}
            log.payload    = {binary.to_sym => 1}

            assert_equal({"Café ☕" => "v"}, parsed["named_tags"])
            assert_equal({"Café ☕" => 1}, parsed["payload"])
          end

          it "keeps the characters of the exception message" do
            log.exception = RuntimeError.new(binary)

            assert_equal "Café ☕", parsed["exception"]["message"]
          end
        end

        # An appender that sends entries out can mask diagnostic sub-keys that an in-memory
        # appender keeps, with sensitive keys of its formatter's own, on top of the global
        # SemanticLogger.sensitive_keys.
        describe "with its own sensitive keys" do
          let(:mask) { SemanticLogger::Redactor::MASK }

          # Anonymous test values: a Struct of an anonymous class in the payload, and an
          # exception of an anonymous class, whose details the formatter writes.
          let(:diagnostics) { Struct.new(:worker, :memory_bytes, :api_key).new("w1", 1024, "abc123") }
          let(:exception_class) { Class.new(StandardError) }

          let(:log) do
            log         = SemanticLogger::Log.new("JsonTest", level)
            log.time    = log_time
            log.message = "Collected"
            log.payload = {diagnostics: diagnostics}
            begin
              raise exception_class, "disk full"
            rescue exception_class => e
              log.exception = e
            end
            log
          end

          let(:masking) { SemanticLogger::Formatters::Json.new(sensitive_keys: %i[api_key memory_bytes]) }
          let(:keeping) { SemanticLogger::Formatters::Json.new }

          it "writes the entry as its own keys say, whichever formatter runs first, without changing it" do
            # The masking formatter runs first, as an appender added before another does, and second.
            forward = [masking, keeping].map { |formatter| JSON.parse(formatter.call(log, appender)) }
            reverse = [keeping, masking].map { |formatter| JSON.parse(formatter.call(log, appender)) }

            # Each formatter writes the same fields in either order.
            assert_equal forward, reverse.reverse
            masked, kept = forward

            assert_equal mask, masked.dig("payload", "diagnostics", "api_key")
            assert_equal mask, masked.dig("payload", "diagnostics", "memory_bytes")
            assert_equal "w1", masked.dig("payload", "diagnostics", "worker")

            assert_equal "abc123", kept.dig("payload", "diagnostics", "api_key")
            assert_equal 1024, kept.dig("payload", "diagnostics", "memory_bytes")

            # The exception's details, which are not redacted, are the same in both.
            [masked, kept].each do |parsed|
              assert_equal exception_class.to_s, parsed.dig("exception", "name")
              assert_equal "disk full", parsed.dig("exception", "message")
            end

            # The log entry is never changed.
            assert_same diagnostics, log.payload[:diagnostics]
            assert_equal "abc123", diagnostics.api_key
            assert_equal 1024, diagnostics.memory_bytes
          end

          it "keeps the entry itself when nothing in it is sensitive to it" do
            log.payload = {worker: "w1"}
            masking.call(log, appender)

            assert_same log, masking.log
          end

          describe "with the global sensitive keys as its base" do
            before { SemanticLogger.sensitive_keys = [:password] }
            after { SemanticLogger.sensitive_keys = nil }

            it "masks the global keys and its own" do
              log.payload = {password: "secret", diagnostics: diagnostics}
              # Redacted once before any appender writes the entry, as Appenders#log does.
              log.redact(SemanticLogger.redactor)

              masked = JSON.parse(masking.call(log, appender))
              kept   = JSON.parse(keeping.call(log, appender))

              assert_equal mask, masked.dig("payload", "password")
              assert_equal mask, masked.dig("payload", "diagnostics", "api_key")
              assert_equal mask, kept.dig("payload", "password")
              assert_equal "abc123", kept.dig("payload", "diagnostics", "api_key")
            end
          end
        end

        describe "payload values that JSON cannot represent" do
          it "serializes a float that is not finite, instead of dropping the event" do
            log.message = "Transfer"
            log.payload = {amount: "1e400".to_f}

            assert_equal "Transfer", parsed["message"]
            assert_equal({"amount" => "Infinity"}, parsed["payload"])
          end

          it "serializes request params nested as deeply as JSON.parse accepts" do
            log.message = "Completed #create"
            log.payload = {params: JSON.parse("#{'{"a":' * 100}1#{'}' * 100}")}

            assert_equal "Completed #create", parsed["message"]
          end

          it "serializes a Struct as an object of its members, not its #inspect" do
            client = Object.new
            client.instance_variable_set(:@password, "secret")
            log.message = "Connected"
            log.payload = {connection: Struct.new(:host, :client).new("db1", client)}

            assert_equal({"host" => "db1", "client" => client.to_s}, parsed["payload"]["connection"])
            refute_includes formatter.call(log, appender), "secret"
          end
        end
      end
    end
  end
end
