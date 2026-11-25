require_relative "../test_helper"

module CoreExt
  class RubyWarningsTest < Minitest::Test
    # The two categories Ruby itself warns with, in the text Ruby writes before calling
    # Warning.warn, with the file and line that raised the warning.
    DEPRECATED_WARNING   = "app/order.rb:42: warning: BufferedIO is deprecated, use IO#read\n".freeze
    EXPERIMENTAL_WARNING = "app/pattern.rb:7: warning: Find pattern is experimental\n".freeze

    # A warning Ruby writes without a category, such as a redefined method.
    PLAIN_WARNING = "app/other.rb:3: warning: method redefined; discarding old submit\n".freeze

    # A warning built without the "path:line: warning: " prefix, such as one handed to Warning.warn
    # by hand.
    HAND_BUILT_WARNING = "something went wrong\n".freeze

    # Collects every entry it is given, unlike InMemoryAppender which keeps only the last one. Its
    # level and filter are set on the instance itself, since add_appender(appender:) cannot apply
    # them to an instance that already carries its own (see Appender.subscriber_instance).
    class Collector < SemanticLogger::Subscriber
      attr_reader :logs

      def initialize(**options)
        @logs = []
        super
      end

      def log(log)
        @logs << log
      end
    end

    # Stand-in for `Warning` for the pass-through tests, so that they neither prepend onto the real
    # `Warning` nor depend on Ruby's C-level category filtering (Ruby drops a categorized warning
    # before Warning.warn when its category switch is off). The module prepended to it resolves
    # `super` to the `warn` defined here.
    class WarningStub
      attr_reader :warnings

      def initialize
        @warnings = []
      end

      def warn(message, category: nil)
        @warnings << [message, category]
        nil
      end

      prepend SemanticLogger::CoreExt::RubyWarnings
    end

    # Stand-in for an application's own `Warning.warn` that takes only the message.
    class MessageOnlyWarningStub
      attr_reader :warnings

      def initialize
        @warnings = []
      end

      def warn(message)
        @warnings << message
        nil
      end

      prepend SemanticLogger::CoreExt::RubyWarnings
    end

    describe SemanticLogger::CoreExt::RubyWarnings do
      let(:extension) { SemanticLogger::CoreExt::RubyWarnings }

      # Log inline on the calling thread, so an entry is in the appender before the assertion runs
      # and appenders added here never race the background processor across tests.
      before do
        SemanticLogger.sync!
        SemanticLogger.capture_ruby_warnings = false
        SemanticLogger.default_level         = :trace
        @appenders = []
      end

      after do
        @appenders.each { |appender| SemanticLogger.remove_appender(appender) }
        SemanticLogger.capture_ruby_warnings = false
      end

      def add_collector(**options)
        SemanticLogger.add_appender(appender: Collector.new(**options)).tap do |appender|
          @appenders << appender
        end
      end

      describe "enabling and disabling" do
        it "is off until enabled" do
          refute SemanticLogger.capture_ruby_warnings?
        end

        it "prepends itself to Warning once when enabled" do
          SemanticLogger.capture_ruby_warnings = true

          assert SemanticLogger.capture_ruby_warnings?
          assert_includes Warning.singleton_class.ancestors, extension
          owner = Warning.singleton_class.instance_method(:warn).owner

          SemanticLogger.capture_ruby_warnings = true
          assert_same owner, Warning.singleton_class.instance_method(:warn).owner
        end

        it "stops capturing when disabled while staying prepended" do
          SemanticLogger.capture_ruby_warnings = true
          SemanticLogger.capture_ruby_warnings = false

          refute SemanticLogger.capture_ruby_warnings?
          assert_includes Warning.singleton_class.ancestors, extension
        end

        it "does not change Ruby's own warning category settings" do
          deprecated   = Warning[:deprecated]
          experimental = Warning[:experimental]

          SemanticLogger.capture_ruby_warnings = true
          SemanticLogger.capture_ruby_warnings = false

          assert_equal deprecated, Warning[:deprecated]
          assert_equal experimental, Warning[:experimental]
        end
      end

      describe "capturing categorized warnings" do
        # Eagerly added in before, so it exists before any warning is raised (a lazy `let` would be
        # built only when first referenced, after the entries it is meant to collect).
        let(:collector) { @collector }

        before do
          @collector = add_collector
          SemanticLogger.capture_ruby_warnings = true
        end

        it "logs a deprecated warning as an ordinary :warn entry" do
          Warning.warn(DEPRECATED_WARNING, category: :deprecated)

          log = collector.logs.first
          assert log
          assert_equal :warn, log.level
          assert_equal "Ruby Warning", log.name
          assert_equal :deprecated, log.warning_category
        end

        it "logs an experimental warning with its category" do
          Warning.warn(EXPERIMENTAL_WARNING, category: :experimental)

          assert_equal :experimental, collector.logs.first.warning_category
        end

        it "carries the location Ruby named in the warning as the call site" do
          Warning.warn(DEPRECATED_WARNING, category: :deprecated)

          call_site = collector.logs.first.call_site
          assert_equal "app/order.rb", call_site.path
          assert_equal 42, call_site.line
        end

        it "uses the warning text without its location prefix and line break as the message" do
          Warning.warn(DEPRECATED_WARNING, category: :deprecated)

          assert_equal "BufferedIO is deprecated, use IO#read", collector.logs.first.message
        end

        it "parses a Windows path that holds a colon in its drive letter" do
          Warning.warn("C:/app/order.rb:42: warning: old API\n", category: :deprecated)

          call_site = collector.logs.first.call_site
          assert_equal "C:/app/order.rb", call_site.path
          assert_equal 42, call_site.line
          assert_equal "old API", collector.logs.first.message
        end

        it "keeps a hand-built warning's text and gives it no call site" do
          Warning.warn(HAND_BUILT_WARNING, category: :deprecated)

          log = collector.logs.first
          assert_nil log.call_site
          assert_equal "something went wrong", log.message
        end

        it "writes the category and the location through the JSON formatter" do
          Warning.warn(DEPRECATED_WARNING, category: :deprecated)

          parsed = JSON.parse(SemanticLogger::Formatters::Json.new.call(collector.logs.first, collector))

          assert_equal "deprecated", parsed["warning_category"]
          assert_equal "app/order.rb", parsed["file"]
          assert_equal 42, parsed["line"]
          assert_equal "warn", parsed["level"]
          assert_equal "BufferedIO is deprecated, use IO#read", parsed["message"]
        end

        it "leaves an application's own warn without a warning_category" do
          SemanticLogger["Billing"].warn("payment retry scheduled")

          log = collector.logs.first
          assert_equal "Billing", log.name
          assert_nil log.warning_category
          assert_equal "payment retry scheduled", log.message
        end

        it "tells a native warning and an application warn apart at the same severity" do
          SemanticLogger["Billing"].warn("payment retry scheduled")
          Warning.warn(DEPRECATED_WARNING, category: :deprecated)

          business, native = collector.logs
          assert_equal :warn, business.level
          assert_equal :warn, native.level
          assert_nil business.warning_category
          assert_equal :deprecated, native.warning_category
        end
      end

      describe "existing level filtering" do
        let(:collector) { @collector }

        before do
          @collector = add_collector
          SemanticLogger.capture_ruby_warnings = true
        end

        it "does not log a captured warning below the global default level" do
          SemanticLogger.default_level = :error

          Warning.warn(DEPRECATED_WARNING, category: :deprecated)

          assert_empty collector.logs
        end

        it "logs a captured warning when the default level allows :warn" do
          SemanticLogger.default_level = :warn

          Warning.warn(DEPRECATED_WARNING, category: :deprecated)

          assert_equal 1, collector.logs.size
        end

        it "lets an appender's own level filter a captured warning" do
          strict = add_collector(level: :error)

          Warning.warn(DEPRECATED_WARNING, category: :deprecated)

          assert_empty strict.logs
          assert_equal 1, collector.logs.size
        end
      end

      describe "an appender filter on the warning category" do
        before { SemanticLogger.capture_ruby_warnings = true }

        it "lets one appender drop experimental while another keeps every warning" do
          everything      = add_collector
          no_experimental = add_collector(filter: ->(log) { log.warning_category != :experimental })

          SemanticLogger["Billing"].warn("a business warning")
          Warning.warn(DEPRECATED_WARNING, category: :deprecated)
          Warning.warn(EXPERIMENTAL_WARNING, category: :experimental)

          assert_equal [nil, :deprecated, :experimental], everything.logs.map(&:warning_category)
          assert_equal [nil, :deprecated], no_experimental.logs.map(&:warning_category)
        end
      end

      describe "warnings that must pass through unchanged" do
        let(:collector) { @collector }

        before do
          @collector = add_collector
          SemanticLogger.capture_ruby_warnings = true
        end

        it "passes a warning without a category on to the next Warning.warn" do
          stub = WarningStub.new

          stub.warn(PLAIN_WARNING)

          assert_equal [[PLAIN_WARNING, nil]], stub.warnings
          assert_empty collector.logs
        end

        it "passes a categorized warning on unchanged while capturing is disabled" do
          SemanticLogger.capture_ruby_warnings = false
          stub = WarningStub.new

          stub.warn(DEPRECATED_WARNING, category: :deprecated)

          assert_equal [[DEPRECATED_WARNING, :deprecated]], stub.warnings
          assert_empty collector.logs
        end

        it "does not hand the category to a Warning.warn that takes only the message" do
          # Capturing is switched off so this exercises the pass-through branch: a #warn that takes
          # only the message must get the message alone, without the category keyword it rejects.
          SemanticLogger.capture_ruby_warnings = false
          stub = MessageOnlyWarningStub.new

          stub.warn(DEPRECATED_WARNING, category: :deprecated)

          assert_equal [DEPRECATED_WARNING], stub.warnings
          assert_empty collector.logs
        end
      end
    end
  end
end
