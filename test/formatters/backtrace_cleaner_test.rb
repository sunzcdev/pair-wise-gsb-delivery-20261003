require_relative "../test_helper"

# The NewRelicLogs formatter requires newrelic_rpm.
add_mocks_to_load_path

require "opentelemetry-logs-sdk"
require "semantic_logger/formatters/open_telemetry"

module SemanticLogger
  module Formatters
    class BacktraceCleanerTest < Minitest::Test
      include BacktraceCleanerFixtures

      MOCK_LOGGER = Struct.new(:host, :application, :environment, :name)

      # Only in a backtrace line: the file name and line number fields, taken from the exception's
      # first frame when the entry has no backtrace of its own, do not include the method.
      GEM_METHOD = "Rack::Runtime#call".freeze

      # A pattern that writes the backtrace, which the default pattern leaves out.
      class BacktracePattern < Pattern
        def initialize(**args)
          # rubocop:disable Style/FormatStringToken -- the Pattern formatter's syntax, not a format string
          super(pattern: "%{message} %{backtrace}", **args)
          # rubocop:enable Style/FormatStringToken
        end
      end

      # Formatters that clean backtraces with the application's cleaner unless told otherwise.
      CLEANING = [Default, Color, BacktracePattern, Syslog, Json, Fluentd].freeze

      # Formatters that write backtraces in full unless told otherwise.
      NOT_CLEANING = [Raw, Ecs, Logfmt, NewRelicLogs, SyslogCee, Loki, OpenTelemetry].freeze

      describe "backtrace_cleaner" do
        # Loki is built without structured_metadata:, which keeps pid and thread as labels.
        allow_deprecation(/The :loki formatter writes pid and thread as stream labels by default/)

        let(:logger) { MOCK_LOGGER.new("host", "app", "test", "BacktraceCleanerTest") }

        let(:exception) do
          exception = RuntimeError.new("Oh no")
          exception.set_backtrace([GEM_FRAME, APP_FRAME, GEM_FRAME])
          exception
        end

        let(:log) do
          log           = SemanticLogger::Log.new("BacktraceCleanerTest", :error)
          log.message   = "Failed"
          log.exception = exception
          log
        end

        # The formatted output, as text that holds every backtrace line.
        def output(formatter)
          formatter.call(log, logger).to_s
        end

        def assert_cleaned(formatter)
          text = output(formatter)

          assert_includes text, CLEANED_FRAME
          refute_includes text, GEM_METHOD
          refute_includes text, APP_FRAME
        end

        def assert_full(formatter)
          text = output(formatter)

          assert_includes text, APP_FRAME
          assert_includes text, GEM_METHOD
        end

        before { SemanticLogger.backtrace_cleaner = AppCleaner.new }
        after { SemanticLogger.backtrace_cleaner = nil }

        describe "by default" do
          CLEANING.each do |formatter_class|
            it "cleans backtraces with the application's cleaner in #{formatter_class.name}" do
              assert_cleaned formatter_class.new
            end
          end

          NOT_CLEANING.each do |formatter_class|
            it "writes backtraces in full in #{formatter_class.name}" do
              assert_full formatter_class.new
            end
          end

          it "writes backtraces in full when the application has no cleaner" do
            SemanticLogger.backtrace_cleaner = nil

            assert_full Default.new
            assert_full Json.new
          end

          it "uses the application's cleaner set after the formatter was built" do
            SemanticLogger.backtrace_cleaner = nil
            formatter                        = Json.new
            SemanticLogger.backtrace_cleaner = AppCleaner.new

            assert_cleaned formatter
          end
        end

        describe "when false" do
          it "writes backtraces in full in a formatter that cleans them by default" do
            assert_full Default.new(backtrace_cleaner: false)
            assert_full Formatters.factory(json: {backtrace_cleaner: false})
          end
        end

        describe "when true" do
          NOT_CLEANING.each do |formatter_class|
            it "cleans backtraces with the application's cleaner in #{formatter_class.name}" do
              assert_cleaned formatter_class.new(backtrace_cleaner: true)
            end
          end
        end

        describe "with a cleaner of the formatter's own" do
          it "uses it in place of the application's cleaner" do
            SemanticLogger.backtrace_cleaner = nil
            formatter                        = Formatters.factory(raw: {backtrace_cleaner: AppCleaner.new})

            assert_equal [CLEANED_FRAME], formatter.call(log, logger)[:exception][:stack_trace]
          end

          it "accepts a Proc" do
            formatter = Default.new(backtrace_cleaner: ->(lines) { lines.grep(/rack/) })

            refute_includes output(formatter), APP_FRAME
          end

          it "can be assigned after the formatter was built" do
            formatter                   = Raw.new
            formatter.backtrace_cleaner = AppCleaner.new

            assert_cleaned formatter
          end
        end

        it "accepts a cleaner that does not define #== or #nil?, such as a mock" do
          cleaner = Minitest::Mock.new.expect(:clean, [CLEANED_FRAME], [Array])

          assert_includes output(Default.new(backtrace_cleaner: cleaner)), CLEANED_FRAME
          cleaner.verify
        end

        it "gives each formatter a copy of the cleaned backtrace that it can change" do
          stack_trace = Raw.new(backtrace_cleaner: true).call(log, logger)[:exception][:stack_trace]
          stack_trace.first << " edited"
          stack_trace << "added"

          assert_equal [CLEANED_FRAME], Raw.new(backtrace_cleaner: true).call(log, logger)[:exception][:stack_trace]
        end

        it "rejects a value that cannot clean a backtrace" do
          error = assert_raises(ArgumentError) { Default.new(backtrace_cleaner: "false") }

          assert_includes error.message, "Expected true, false, or a backtrace cleaner"
          assert_raises(ArgumentError) { Json.new.backtrace_cleaner = :clean }
        end

        it "cleans the backtrace of each exception that caused the logged one" do
          begin
            begin
              raise ArgumentError, "inner"
            rescue ArgumentError => e
              e.set_backtrace([GEM_FRAME, "/app/models/line_item.rb:3:in 'LineItem#save'"])
              raise "outer"
            end
          rescue RuntimeError => e
            e.set_backtrace([GEM_FRAME, APP_FRAME])
            log.exception = e
          end

          assert_equal "-- Exception: RuntimeError: outer\n#{CLEANED_FRAME}\n" \
                       "Cause: ArgumentError: inner\nmodels/line_item.rb:3:in 'LineItem#save'",
                       Default.new.tap { |formatter| formatter.call(log, logger) }.exception

          cause = Raw.new(backtrace_cleaner: true).call(log, logger)[:exception][:cause]

          assert_equal ["models/line_item.rb:3:in 'LineItem#save'"], cause[:stack_trace]
        end

        it "writes the backtrace in full when the cleaner leaves out every line" do
          exception.set_backtrace([GEM_FRAME])

          assert_includes output(Default.new), GEM_FRAME
        end

        it "escapes control characters in the cleaned lines" do
          formatter = Default.new(escape_control_chars: true, backtrace_cleaner: ->(_lines) { ["app/a.rb:1\n forged"] })

          assert_includes output(formatter), "app/a.rb:1\\n forged"
        end

        it "leaves the exception's backtrace unchanged" do
          output(Default.new)

          assert_equal [GEM_FRAME, APP_FRAME, GEM_FRAME], exception.backtrace
        end

        describe "backtrace_frames" do
          # Two frames whose text is exactly alike, with the real call line between them.
          let(:repeated) { "/app/models/order.rb:12:in 'Order#same'".freeze }
          let(:call_line) { "/app/models/order.rb:42:in 'Order#ship'".freeze }
          let(:cleaned_frames) { [repeated, call_line, repeated] }

          # Swaps the two repeated frames, rewrites their text, and in a second pass folds them into
          # one, keeping every original index. The text alone cannot tell which frame is which.
          let(:frame_cleaner) do
            Class.new do
              def clean_frames(frames)
                [
                  frames[2].with(text: "moved", indices: [2]),
                  frames[1],
                  frames[0].with(text: "moved", indices: [0])
                ]
              end
            end.new
          end

          let(:folding_cleaner) do
            Class.new do
              def clean_frames(frames)
                [frames[2].with(text: "folded", indices: frames[2].indices + frames[0].indices), frames[1]]
              end
            end.new
          end

          before do
            exception.set_backtrace(cleaned_frames)
            SemanticLogger.backtrace_cleaner = nil
          end

          # The cleaned frames as the structured formatter writes them, before JSON serialization.
          def raw_frames(formatter)
            formatter.call(log, logger)[:exception][:stack_trace_frames]
          end

          it "writes the index of each original frame a cleaner moved" do
            frames = raw_frames(Raw.new(backtrace_cleaner: frame_cleaner, backtrace_frames: true))

            assert_equal [{line: "moved", index: 2}, {line: call_line, index: 1}, {line: "moved", index: 0}], frames
          end

          it "writes every original index a cleaner folded into one line" do
            formatter = Raw.new(backtrace_cleaner: folding_cleaner, backtrace_frames: true)
            frames    = raw_frames(formatter)

            assert_equal [{line: "folded", indices: [0, 2]}, {line: call_line, index: 1}], frames
            assert_equal ["folded", call_line],
                         Raw.new(backtrace_cleaner: folding_cleaner).call(log, logger)[:exception][:stack_trace]
          end

          it "writes the frames as JSON too, keeping the source of each line" do
            parsed = JSON.parse(Json.new(backtrace_cleaner: frame_cleaner, backtrace_frames: true).call(log, logger))

            assert_equal [{"line" => "moved", "index" => 2}, {"line" => call_line, "index" => 1},
                          {"line" => "moved", "index" => 0}],
                         parsed["exception"]["stack_trace_frames"]
            assert_equal ["moved", call_line, "moved"], parsed["exception"]["stack_trace"]
          end

          it "keeps the same source on every format of the same entry, and the original backtrace" do
            formatter = Raw.new(backtrace_cleaner: frame_cleaner, backtrace_frames: true)

            assert_equal raw_frames(formatter), raw_frames(formatter)
            assert_equal cleaned_frames, exception.backtrace
          end

          it "keeps the call site of the entry, which is the original call line" do
            log.call_site = SemanticLogger::CallSite.parse(call_line)

            formatter = Raw.new(backtrace_cleaner: frame_cleaner, backtrace_frames: true)
            formatter.call(log, logger)

            assert_equal call_line, log.call_site.to_s
            assert_equal ["/app/models/order.rb", 42], log.call_site.file_name_and_line
          end

          it "marks the frames a cleaner that only returns text rewrote as having no source" do
            frames = raw_frames(Raw.new(backtrace_cleaner: ->(lines) { [lines[0], lines[1]] }, backtrace_frames: true))

            assert_equal [{line: repeated}, {line: call_line, index: 1}], frames
          end

          it "writes no frames by default" do
            refute Raw.new(backtrace_cleaner: frame_cleaner).call(log, logger)[:exception].key?(:stack_trace_frames)
            refute JSON.parse(Json.new(backtrace_cleaner: frame_cleaner).call(log, logger))["exception"].
              key?("stack_trace_frames")
          end

          it "accepts true and false" do
            assert Raw.new(backtrace_frames: true).call(log, logger)[:exception].key?(:stack_trace_frames)
            refute Raw.new(backtrace_frames: false).call(log, logger)[:exception].key?(:stack_trace_frames)
          end

          it "writes the frames of each exception that caused the logged one" do
            begin
              begin
                raise ArgumentError, "inner"
              rescue ArgumentError => e
                e.set_backtrace([GEM_FRAME, "/app/models/line_item.rb:3:in 'LineItem#save'"])
                raise "outer"
              end
            rescue RuntimeError => e
              e.set_backtrace([GEM_FRAME, APP_FRAME])
              log.exception = e
            end

            cause = Raw.new(backtrace_cleaner: true, backtrace_frames: true).call(log, logger)[:exception][:cause]

            assert_equal [{line: GEM_FRAME, index: 0},
                          {line: "/app/models/line_item.rb:3:in 'LineItem#save'", index: 1}],
                         cause[:stack_trace_frames]
          end

          it "writes the frames of the original backtrace when there is no cleaner" do
            frames = raw_frames(Raw.new(backtrace_frames: true))

            assert_equal [{line: repeated, index: 0}, {line: call_line, index: 1}, {line: repeated, index: 2}], frames
          end

          it "gives each formatter a copy of the frames that it can change" do
            formatter = Raw.new(backtrace_cleaner: frame_cleaner, backtrace_frames: true)
            frames    = raw_frames(formatter)
            frames.first[:line] << " edited"
            frames << {line: "added"}

            assert_equal 3, raw_frames(formatter).length
            assert_equal "moved", raw_frames(formatter).first[:line]
          end

          it "writes the text formatters' output without the frames" do
            assert_includes output(Default.new(backtrace_cleaner: frame_cleaner, backtrace_frames: true)), "moved"
          end
        end
      end
    end
  end
end
