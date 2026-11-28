require_relative "test_helper"

class BacktraceCleaningTest < Minitest::Test
  include BacktraceCleanerFixtures

  describe SemanticLogger::BacktraceCleaning do
    let(:backtrace) { [GEM_FRAME, APP_FRAME, GEM_FRAME] }

    describe ".cleaner?" do
      it "accepts an object that responds to #clean" do
        assert SemanticLogger::BacktraceCleaning.cleaner?(AppCleaner.new)
      end

      it "accepts a Proc" do
        assert SemanticLogger::BacktraceCleaning.cleaner?(->(lines) { lines })
      end

      it "rejects other values" do
        [nil, true, "app", :clean].each do |value|
          refute SemanticLogger::BacktraceCleaning.cleaner?(value), value.inspect
        end
      end
    end

    describe ".validate!" do
      it "returns a cleaner" do
        cleaner = AppCleaner.new

        assert_same cleaner, SemanticLogger::BacktraceCleaning.validate!(cleaner)
      end

      it "rejects other values" do
        [nil, true, "app", :clean].each do |value|
          error = assert_raises(ArgumentError) { SemanticLogger::BacktraceCleaning.validate!(value) }

          assert_includes error.message, "Expected a backtrace cleaner that responds to #clean or #call"
        end
      end
    end

    describe ".clean" do
      it "returns the lines from #clean" do
        assert_equal [CLEANED_FRAME], SemanticLogger::BacktraceCleaning.clean(backtrace, AppCleaner.new)
      end

      it "returns the lines from #call" do
        assert_equal [APP_FRAME], SemanticLogger::BacktraceCleaning.clean(backtrace, ->(lines) { lines.grep(/app/) })
      end

      it "prefers #clean when the cleaner also responds to #call" do
        cleaner = AppCleaner.new
        def cleaner.call(_lines) = ["from call"]

        assert_equal [CLEANED_FRAME], SemanticLogger::BacktraceCleaning.clean(backtrace, cleaner)
      end

      it "returns a frozen Array of frozen lines" do
        cleaned = SemanticLogger::BacktraceCleaning.clean(backtrace, AppCleaner.new)

        assert_predicate cleaned, :frozen?
        assert cleaned.all?(&:frozen?)
      end

      it "does not freeze the lines the cleaner returns" do
        line = +"app/a.rb:1"
        SemanticLogger::BacktraceCleaning.clean(backtrace, ->(_lines) { [line] })

        refute_predicate line, :frozen?
      end

      it "does not freeze the supplied lines, even of a String subclass" do
        line_class = Class.new(String)
        lines      = [line_class.new(APP_FRAME), line_class.new(GEM_FRAME)]
        SemanticLogger::BacktraceCleaning.clean(lines, ->(cleaned) { cleaned })
        SemanticLogger::BacktraceCleaning.clean(lines, ->(_lines) {})

        refute lines.any?(&:frozen?)
      end

      it "writes a Thread::Backtrace::Location as a String" do
        cleaned = SemanticLogger::BacktraceCleaning.clean(backtrace, ->(_lines) { caller_locations(0, 1) })

        assert_equal [String], cleaned.map(&:class)
        assert_includes cleaned.first, "backtrace_cleaning_test.rb"
      end

      it "leaves the supplied backtrace unchanged when the cleaner changes the lines it is given" do
        original = backtrace.dup
        cleaner  = ->(lines) { lines.reject! { |line| line == GEM_FRAME } }
        cleaned  = SemanticLogger::BacktraceCleaning.clean(backtrace, cleaner)

        assert_equal [APP_FRAME], cleaned
        assert_equal original, backtrace
      end

      it "returns a frozen copy of the supplied backtrace when the cleaner leaves out every line" do
        gems_only = [+GEM_FRAME, +GEM_FRAME]
        cleaned   = SemanticLogger::BacktraceCleaning.clean(gems_only, AppCleaner.new)

        assert_equal gems_only, cleaned
        assert_predicate cleaned, :frozen?
        assert cleaned.all?(&:frozen?)
        refute_predicate gems_only, :frozen?
        refute gems_only.any?(&:frozen?)
      end

      it "returns the supplied backtrace when the cleaner does not return an Array" do
        assert_equal backtrace, SemanticLogger::BacktraceCleaning.clean(backtrace, ->(_lines) { "app" })
        assert_equal backtrace, SemanticLogger::BacktraceCleaning.clean(backtrace, ->(_lines) {})
      end

      describe "when the cleaner raises" do
        let(:io) { StringIO.new }
        let(:cleaner) { ->(_lines) { raise "broken" } }
        let(:clean_from) { cleaner }

        # The internal logger writes text, as by default, so that it would ask the cleaner to clean
        # the backtrace of any exception it is given.
        def internal_processor
          SemanticLogger::SyncProcessor.new(SemanticLogger::Appenders.new(SemanticLogger::Appender::IO.new(io)))
        end

        def clean_reporting_to_io(backtrace)
          SemanticLogger::Logger.stub(:processor, internal_processor) do
            SemanticLogger::BacktraceCleaning.clean(backtrace, clean_from)
          end
        end

        before { SemanticLogger.backtrace_cleaner = cleaner }
        after { SemanticLogger.backtrace_cleaner = nil }

        it "returns the supplied backtrace" do
          assert_equal backtrace, clean_reporting_to_io(backtrace)
        end

        it "reports the error, and where it was raised, to the internal logger" do
          clean_reporting_to_io(backtrace)

          assert_match(/W .*Backtrace cleaner raised RuntimeError: broken \(.*backtrace_cleaning_test\.rb:\d+/,
                       io.string)
          assert_includes io.string, "so the full backtrace is logged"
        end

        it "names an error class without a name" do
          error_class = Class.new(StandardError)
          cleaner     = ->(_lines) { raise error_class, "broken" }
          SemanticLogger::Logger.stub(:processor, internal_processor) do
            SemanticLogger::BacktraceCleaning.clean(backtrace, cleaner)
          end

          assert_includes io.string, "raised #{error_class.inspect}: broken"
        end

        it "reports the message the error was raised with when its #message raises" do
          cleaner = ->(_lines) { raise ExceptionFixtures::AbstractMessageError, "broken" }
          cleaned = SemanticLogger::Logger.stub(:processor, internal_processor) do
            SemanticLogger::BacktraceCleaning.clean(backtrace, cleaner)
          end

          assert_equal backtrace, cleaned
          assert_includes io.string, "Backtrace cleaner raised ExceptionFixtures::AbstractMessageError: broken"
        end

        describe "for a line that cannot be written as text" do
          let(:clean_from) do
            line = Object.new
            def line.to_s = raise("no text")
            ->(_lines) { [line] }
          end

          it "returns the supplied backtrace, and reports the error" do
            assert_equal backtrace, clean_reporting_to_io(backtrace)
            assert_includes io.string, "Backtrace cleaner raised RuntimeError: no text"
          end
        end

        describe "for an object that is not a cleaner, such as the formatter option true" do
          let(:clean_from) { true }

          it "returns the supplied backtrace, and reports the error" do
            assert_equal backtrace, clean_reporting_to_io(backtrace)
            assert_includes io.string, "Backtrace cleaner raised NoMethodError"
          end
        end
      end

      it "does not call the cleaner without a backtrace" do
        cleaner = ->(_lines) { raise "should not be called" }

        assert_nil SemanticLogger::BacktraceCleaning.clean(nil, cleaner)
        assert_equal [], SemanticLogger::BacktraceCleaning.clean([], cleaner)
      end
    end

    describe "source frames" do
      let(:frame_class) { SemanticLogger::BacktraceCleaning::Frame }
      # Two frames whose text is exactly alike, with the real call line between them, so that only
      # the source of each frame tells them apart (see the shortcuts in the question this answers).
      let(:repeated) { "/app/models/order.rb:12:in 'Order#same'".freeze }
      let(:call_line) { "/app/models/order.rb:42:in 'Order#ship'".freeze }
      let(:backtrace) { [repeated, call_line, repeated] }

      describe SemanticLogger::BacktraceCleaning::Frame do
        it "holds the line and the indices of the original frames" do
          frame = frame_class.new(text: "app.rb:1", indices: [3])

          assert_equal "app.rb:1", frame.text
          assert_equal [3], frame.indices
          assert_equal 3, frame.index
          refute_predicate frame, :collapsed?
          refute_predicate frame, :unknown?
          assert_equal "app.rb:1", frame.to_s
        end

        it "is frozen, with frozen text and indices" do
          source = +"app.rb:1"
          frame  = frame_class.new(text: source, indices: [1, 2, 2, -3, "x"])

          assert_predicate frame, :frozen?
          assert_predicate frame.text, :frozen?
          assert_predicate frame.indices, :frozen?
          refute_predicate source, :frozen?
          assert_equal [1, 2], frame.indices
        end

        it "is equal when both the text and the indices are" do
          assert_equal frame_class.new(text: "a.rb:1", indices: [0]),
                       frame_class.new(text: "a.rb:1", indices: [0])
          refute_equal frame_class.new(text: "a.rb:1", indices: [1]),
                       frame_class.new(text: "a.rb:1", indices: [0])
        end

        it "keeps its text and indices when one is replaced" do
          frame = frame_class.new(text: "a.rb:1", indices: [4]).with(indices: [2, 0])

          assert_equal "a.rb:1", frame.text
          assert_equal [0, 2], frame.indices
          assert_equal [0, 2], frame.with(text: "b.rb:2").indices
          assert_equal "a.rb:1", frame.with(indices: [7]).text
        end

        it "returns a frame that is frozen, as the original is" do
          frame = frame_class.new(text: "a.rb:1", indices: [4]).with(text: "b.rb:2")

          assert_predicate frame, :frozen?
          assert_predicate frame.text, :frozen?
          assert_predicate frame.indices, :frozen?
        end

        it "reports a frame that folds several original frames into one line" do
          frame = frame_class.new(text: "folded", indices: [2, 0])

          assert_predicate frame, :collapsed?
          assert_nil frame.index
          assert_equal [0, 2], frame.source
        end

        it "reports a frame whose original frame cannot be told" do
          frame = frame_class.new(text: "rewritten")

          assert_predicate frame, :unknown?
          assert_nil frame.index
          assert_nil frame.source
          assert_same frame_class::UNKNOWN, frame.indices
        end

        it "accepts its members positionally and by keyword" do
          assert_equal frame_class.new(text: "a.rb:1", indices: [1]), frame_class.new("a.rb:1", [1])
          assert_equal frame_class.new(text: "a.rb:2"), frame_class.new(text: "a.rb:2", indices: [])
        end
      end

      describe ".cleaner?" do
        it "accepts a cleaner that responds to #clean_frames or #call_frames" do
          assert SemanticLogger::BacktraceCleaning.cleaner?(Class.new { def clean_frames(frames) = frames }.new)
          assert SemanticLogger::BacktraceCleaning.cleaner?(Class.new { def call_frames(frames) = frames }.new)
        end
      end

      describe ".frame_cleaner?" do
        it "accepts only a cleaner that keeps the source of each frame" do
          assert SemanticLogger::BacktraceCleaning.frame_cleaner?(Class.new { def clean_frames(frames) = frames }.new)
          assert SemanticLogger::BacktraceCleaning.frame_cleaner?(Class.new { def call_frames(frames) = frames }.new)
          refute SemanticLogger::BacktraceCleaning.frame_cleaner?(AppCleaner.new)
          refute SemanticLogger::BacktraceCleaning.frame_cleaner?(->(lines) { lines })
        end
      end

      describe ".clean_frames" do
        it "gives a frame cleaner each original frame with its own index" do
          cleaner = Class.new do
            attr_reader :seen

            def clean_frames(frames)
              @seen = frames
              frames
            end
          end.new

          SemanticLogger::BacktraceCleaning.clean_frames(backtrace, cleaner)

          assert_equal backtrace, cleaner.seen.map(&:text)
          assert_equal [[0], [1], [2]], cleaner.seen.map(&:indices)
        end

        it "keeps the index of each original frame when the cleaner swaps two that read alike" do
          cleaner = Class.new do
            def clean_frames(frames)
              [frames[2], frames[1], frames[0]]
            end
          end.new

          frames = SemanticLogger::BacktraceCleaning.clean_frames(backtrace, cleaner)

          assert_equal backtrace, frames.map(&:text)
          assert_equal [[2], [1], [0]], frames.map(&:indices)
        end

        it "keeps the index of each original frame when the cleaner rewrites its line" do
          cleaner = Class.new do
            def clean_frames(frames)
              frames.map { |frame| frame.with(text: "rewritten #{frame.index}") }
            end
          end.new

          frames = SemanticLogger::BacktraceCleaning.clean_frames(backtrace, cleaner)

          assert_equal ["rewritten 0", "rewritten 1", "rewritten 2"], frames.map(&:text)
          assert_equal [[0], [1], [2]], frames.map(&:indices)
        end

        it "holds every original index of a line that folds several frames into one" do
          cleaner = Class.new do
            def clean_frames(frames)
              [frames[2].with(text: "collapsed", indices: frames[2].indices + frames[0].indices), frames[1]]
            end
          end.new

          frames = SemanticLogger::BacktraceCleaning.clean_frames(backtrace, cleaner)

          assert_equal ["collapsed", call_line], frames.map(&:text)
          assert_equal [[0, 2], [1]], frames.map(&:indices)
          assert_predicate frames.first, :collapsed?
        end

        it "prefers #clean_frames, then #call_frames" do
          cleaner = Class.new do
            def clean_frames(frames) = frames.first(1)
            def call_frames(_frames) = raise("should not be called")
          end.new

          assert_equal 1, SemanticLogger::BacktraceCleaning.clean_frames(backtrace, cleaner).length

          call_only = Class.new { def call_frames(frames) = frames.last(1) }.new

          assert_equal [2], SemanticLogger::BacktraceCleaning.clean_frames(backtrace, call_only).map(&:index)
        end

        it "hands the cleaner a copy it can change, leaving the original frames as they are" do
          cleaner = Class.new do
            def clean_frames(frames)
              frames.reject! { |frame| frame.index == 1 }
              frames
            end
          end.new
          original = SemanticLogger::BacktraceCleaning.original_frames(backtrace)

          cleaned = SemanticLogger::BacktraceCleaning.clean_frames(backtrace, cleaner)

          assert_equal [0, 2], cleaned.map(&:index)
          assert_equal [0, 1, 2], original.map(&:index)
        end

        it "leaves the supplied backtrace unchanged" do
          original = backtrace.dup
          cleaner  = Class.new { def clean_frames(frames) = frames.reverse }.new

          SemanticLogger::BacktraceCleaning.clean_frames(backtrace, cleaner)

          assert_equal original, backtrace
        end

        describe "for a cleaner that only returns text" do
          it "keeps the index of a line it left as it is, when only one original frame has that text" do
            distinct = ["/app/a.rb:1:in 'A#one'", APP_FRAME, "/app/b.rb:2:in 'B#two'"]
            frames   = SemanticLogger::BacktraceCleaning.clean_frames(distinct, ->(lines) { [lines[2], lines[0]] })

            assert_equal [[2], [0]], frames.map(&:indices)
          end

          it "marks both lines of two original frames that read alike as unknown" do
            frames = SemanticLogger::BacktraceCleaning.clean_frames(backtrace, ->(lines) { [lines[0], lines[2]] })

            assert_equal [repeated, repeated], frames.map(&:text)
            assert_equal [[], []], frames.map(&:indices)
            assert frames.all?(&:unknown?)
          end

          it "marks a rewritten line as unknown rather than blame a frame it does not name" do
            frames = SemanticLogger::BacktraceCleaning.clean_frames(%w[app gem],
                                                                    ->(lines) { lines.grep(/app/).map(&:upcase) })

            assert_equal ["APP"], frames.map(&:text)
            assert_predicate frames.first, :unknown?
          end

          it "keeps every index it can tell, and marks the rest unknown" do
            frames = SemanticLogger::BacktraceCleaning.clean_frames([repeated, APP_FRAME, repeated],
                                                                    ->(lines) { [lines[0], lines[1]] })

            assert_equal [repeated, APP_FRAME], frames.map(&:text)
            assert_equal [[], [1]], frames.map(&:indices)
          end

          it "writes a returned Thread::Backtrace::Location as text, with no source" do
            frames = SemanticLogger::BacktraceCleaning.clean_frames(backtrace,
                                                                    ->(_lines) { caller_locations(0, 1) })

            assert_equal [String], frames.map(&:text).map(&:class)
            assert_predicate frames.first, :unknown?
          end
        end

        it "returns the original frames when the cleaner leaves out every line" do
          frames = SemanticLogger::BacktraceCleaning.clean_frames(backtrace, ->(_lines) { [] })

          assert_equal [[0], [1], [2]], frames.map(&:indices)
          assert_equal backtrace, frames.map(&:text)
          assert_predicate frames, :frozen?
        end

        it "returns the original frames when the cleaner does not return an Array" do
          assert_equal backtrace,
                       SemanticLogger::BacktraceCleaning.clean_frames(backtrace, ->(_lines) { "app" }).map(&:text)
        end

        it "returns nil without a backtrace" do
          assert_nil SemanticLogger::BacktraceCleaning.clean_frames(nil, AppCleaner.new)
          assert_equal [], SemanticLogger::BacktraceCleaning.clean_frames([], AppCleaner.new)
        end

        describe "when the cleaner raises" do
          let(:io) { StringIO.new }

          def frames_reporting_to_io(clean)
            processor = SemanticLogger::SyncProcessor.new(SemanticLogger::Appenders.new(SemanticLogger::Appender::IO.new(io)))
            SemanticLogger::Logger.stub(:processor, processor) do
              SemanticLogger::BacktraceCleaning.clean_frames(backtrace, clean)
            end
          end

          it "returns the original frames, and reports the error" do
            frames = frames_reporting_to_io(->(_lines) { raise "broken" })

            assert_equal backtrace, frames.map(&:text)
            assert_equal [[0], [1], [2]], frames.map(&:indices)
            assert_includes io.string, "Backtrace cleaner raised RuntimeError: broken"
          end
        end
      end

      describe ".clean" do
        it "returns the lines of the frames a frame cleaner keeps" do
          cleaner = Class.new do
            def clean_frames(frames) = frames.reverse.map { |frame| frame.with(text: "<#{frame.text}>") }
          end.new

          assert_equal backtrace.reverse.map { |line| "<#{line}>" },
                       SemanticLogger::BacktraceCleaning.clean(backtrace, cleaner)
        end

        it "returns a frozen Array of frozen lines" do
          cleaner = Class.new { def clean_frames(frames) = frames }.new
          cleaned = SemanticLogger::BacktraceCleaning.clean(backtrace, cleaner)

          assert_predicate cleaned, :frozen?
          assert cleaned.all?(&:frozen?)
        end
      end
    end
  end
end
