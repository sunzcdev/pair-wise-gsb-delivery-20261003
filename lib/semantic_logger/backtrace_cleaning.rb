module SemanticLogger
  # Asks an application's backtrace cleaner, such as `Rails.backtrace_cleaner`, to clean a backtrace.
  #
  # A backtrace cleaner is any object that responds to `#clean`, as ActiveSupport::BacktraceCleaner
  # does, or to `#call`, such as a Proc. It is given the backtrace as an Array of Strings, and
  # returns the lines to log in its place: it can leave lines out, and shorten or rewrite them.
  #
  # A cleaner that also wants to say which original frames each line it returns came from responds
  # to `#clean_frames`, or to `#call_frames`, instead. It is given the original frames (see Frame)
  # and returns the frames to log, so that a frame it moves, rewrites, or folds together with
  # another keeps the index of the original frame it came from, even when two frames are written
  # exactly alike.
  #
  # Internal: Log caches the result for each of its exceptions; see Log#backtrace_of and
  # Log#backtrace_frames_of.
  module BacktraceCleaning
    # A line of a cleaned backtrace, with the indices of the original frames it came from.
    #
    # A cleaner that responds to `#clean_frames` or `#call_frames` is given the original frames,
    # each with the index of its line in the exception's own backtrace: 0 for the frame that raised
    # the exception. A frame it keeps keeps that index however it rewrites the text, or wherever it
    # moves the frame, and one line that it folds several original frames into has each of their
    # indices. A frame whose indices are empty is one whose original frame cannot be told, such as a
    # line that a cleaner which only returns text did not leave as it was (see .clean_frames).
    #
    # A Frame is frozen, and so are its text and indices, so that every appender can share it. It
    # holds no reference to the exception, whose backtrace is never changed.
    Frame = Data.define(:text, :indices)

    # The behavior of a Frame (see above), held apart from the Data definition so that it can also
    # define #with, which Data itself only gains on Ruby 3.4.
    class Frame
      # The indices of a frame that cannot be traced to an original frame.
      UNKNOWN = [].freeze

      # Parameters
      #   text: [String]
      #     The line as the cleaner writes it.
      #   indices: [Array<Integer>]
      #     The indices, in the exception's own backtrace, of the original frames this frame stands
      #     for: one for a frame that was kept or rewritten, several for frames folded into one
      #     line, and none when they cannot be told.
      #
      # Called with the two members as keywords, as Data passes them, or with the two positional
      # arguments, as a cleaner may build one. The text is copied into a plain frozen String, and
      # the indices are normalized (see .normalize), so that every frame is safe to share.
      def initialize(*args, **members)
        text    = members.key?(:text) ? members[:text] : args[0]
        indices = members.key?(:indices) ? members[:indices] : (args[1] || UNKNOWN)
        super(text: String.new(text.to_s).freeze, indices: Frame.normalize(indices))
      end

      # Returns [Frame] a copy with the given members replaced, which a cleaner uses to rewrite a
      # line, to fold several original frames into one line (see #indices), or to move a frame:
      #
      #   frames[2].with(text: "repeated", indices: frames[2].indices + frames[0].indices)
      def with(text: self.text, indices: self.indices)
        Frame.new(text: text, indices: indices)
      end

      # Returns [Array<Frame>] the frames of the supplied backtrace, each with its own index, which
      # is what a cleaner that responds to #clean_frames is given.
      def self.original(lines)
        lines.each_with_index.map { |line, index| new(text: line, indices: [index]) }
      end

      # Returns [Array<Integer>] the indices as a frozen Array of the Integers of 0 or more, without
      # duplicates, in ascending order. Anything else, such as a nil, or an index a cleaner made up
      # as a String, is left out rather than making the entry fail to be written.
      def self.normalize(indices)
        return UNKNOWN if indices.nil?

        indices = [indices] unless indices.is_a?(Array)
        indices = indices.select { |index| index.is_a?(Integer) && !index.negative? }.uniq.sort
        indices.empty? ? UNKNOWN : indices.freeze
      end

      # Returns [Integer|nil] the index of the original frame, or nil when there is none, or when
      # this frame folds several original frames into one line (see #collapsed?).
      def index
        indices.first if indices.length == 1
      end

      # Returns [Integer|Array<Integer>|nil] the original frames this frame stands for, as a
      # structured formatter writes them: the index of the one original frame, the indices of the
      # several it folds into its line, or nil when they cannot be told.
      def source
        return indices.first if indices.length == 1

        indices if collapsed?
      end

      # Returns [true|false] whether the frame folds more than one original frame into its line.
      def collapsed? = indices.length > 1

      # Returns [true|false] whether the original frame cannot be told, such as a line that a
      # cleaner which only returns text rewrote.
      def unknown? = indices.empty?

      # Returns [String] the line, as the text formatters write it.
      def to_s = text
    end

    # Returns [true|false] whether the object can clean a backtrace.
    def self.cleaner?(object)
      object.respond_to?(:clean) || object.respond_to?(:call) || frame_cleaner?(object)
    end

    # Returns [true|false] whether the object can clean a backtrace while keeping the index of each
    # original frame (see Frame).
    def self.frame_cleaner?(object)
      object.respond_to?(:clean_frames) || object.respond_to?(:call_frames)
    end

    # Returns the cleaner, for a setting that holds one.
    # Raises ArgumentError when it cannot clean a backtrace (see .cleaner?), naming what the
    # setting expects, since it can accept other values too.
    def self.validate!(cleaner, expected: "a backtrace cleaner")
      return cleaner if cleaner?(cleaner)

      raise(ArgumentError,
            "Expected #{expected} that responds to #clean or #call, or to #clean_frames or #call_frames, " \
            "not #{cleaner.inspect}")
    end

    # Returns [Array<String>] the backtrace as the cleaner cleans it, or nil when there is none.
    # The Array and its lines are frozen copies, so that every appender can share them.
    #
    # Returns a copy of the supplied backtrace instead when the cleaner leaves out every line, so
    # that the only clue to where an exception came from is never lost, for example an exception
    # raised entirely within gems that a cleaner silences. Also when the cleaner does not return an
    # Array of lines, or raises a StandardError, such as for an object that is not a cleaner, so
    # that a cleaner cannot stop a log entry being written. The error is reported to the internal
    # logger, since nothing else would show that the cleaner is broken.
    #
    # The cleaner is given a copy of the backtrace, which it may change, since the supplied one
    # belongs to the exception, and other appenders, such as error services, also read it.
    def self.clean(backtrace, cleaner)
      return if backtrace.nil?

      clean_frames(backtrace, cleaner).map(&:text).freeze
    end

    # Returns [Array<Frame>] the backtrace as the cleaner cleans it, each frame with the indices of
    # the original frames it came from, or nil when there is none. Falls back to the supplied
    # backtrace, each frame with its own index, as .clean falls back to its lines.
    #
    # A cleaner that responds to `#clean_frames` or `#call_frames` (see .frame_cleaner?) is given
    # the original frames and returns the frames to log, so that it can say exactly which original
    # frames a line it writes kept or folded together (see Frame).
    #
    # Any other cleaner, such as `Rails.backtrace_cleaner`, is given the lines and returns text, as
    # before, so a line it returns keeps the index of an original frame only when that line is the
    # text of exactly one original frame, such as one it left as it was. A line it rewrote, or the
    # text of two or more original frames, which cannot be told apart by its text, is marked as
    # unknown (see Frame#unknown?) rather than blamed on the wrong frame.
    #
    # Without a cleaner, the original frames are returned, each with its own index.
    #
    # Only the returned frames hold the indices: the exception's own backtrace is never changed.
    def self.clean_frames(backtrace, cleaner)
      return if backtrace.nil?

      frames = original_frames(backtrace)
      # Compared by identity, since a cleaner, such as a test double, need not define #nil?.
      return frames if frames.empty? || nil.equal?(cleaner)

      cleaned_frames(backtrace, frames, cleaner) || frames
    end

    # Returns [Array<Frame>] the supplied backtrace as frames, each with the index of its line and
    # none of the lines changed.
    def self.original_frames(backtrace)
      Frame.original(backtrace).freeze
    end

    # Returns [Array<Frame>] a frozen copy of the frames the cleaner keeps, or nil when it keeps
    # none, does not return an Array of frames, or raises.
    def self.cleaned_frames(backtrace, frames, cleaner)
      cleaned = frame_cleaner?(cleaner) ? call_frames(cleaner, frames) : call_clean(cleaner, backtrace)
      return unless cleaned.is_a?(Array) && !cleaned.empty?

      # Only the keys of the lines that no other line repeats, since a line that is the text of two
      # or more original frames does not say which of them it came from.
      positions = unique_positions(backtrace)
      cleaned.map { |frame| frame_for(frame, positions) }.freeze
    rescue StandardError => e
      report(e)
      nil
    end
    private_class_method :cleaned_frames

    # Returns the value the cleaner made, given a copy of the frames, which it may change, so that a
    # frame is never written by two cleaners at once. A frame cleaner prefers #clean_frames, as a
    # text cleaner prefers #clean.
    def self.call_frames(cleaner, frames)
      cleaner.respond_to?(:clean_frames) ? cleaner.clean_frames(frames.dup) : cleaner.call_frames(frames.dup)
    end
    private_class_method :call_frames

    # Returns the value the cleaner made, given a copy of the backtrace's lines, which it may change.
    def self.call_clean(cleaner, backtrace)
      lines = backtrace.dup
      cleaner.respond_to?(:clean) ? cleaner.clean(lines) : cleaner.call(lines)
    end
    private_class_method :call_clean

    # Returns [Frame] the cleaned frame, with the indices it gives, or those of the one original
    # frame whose text it is. A cleaner can return Thread::Backtrace::Location objects, or Strings
    # of its own, which say nothing of where they came from.
    def self.frame_for(frame, positions)
      return Frame.new(text: frame.text, indices: frame.indices) if Frame === frame # rubocop:disable Style/CaseEquality

      text = frame.to_s
      Frame.new(text: text, indices: positions.fetch(text, Frame::UNKNOWN))
    end
    private_class_method :frame_for

    # Returns [Hash{String => Integer}] the index of each line of the backtrace that no other line
    # repeats, by its text.
    def self.unique_positions(backtrace)
      positions = {}
      repeated  = {}
      backtrace.each_with_index do |line, index|
        text = line.to_s
        positions.key?(text) ? repeated[text] = true : positions[text] = index
      end
      repeated.empty? ? positions : positions.reject { |text, _index| repeated.key?(text) }
    end
    private_class_method :unique_positions

    # Reports an error raised by a cleaner to the internal logger, as text: given the exception
    # itself, the internal logger's formatter would ask the same cleaner to clean its backtrace.
    def self.report(exception)
      where = exception.backtrace&.first
      Logger.processor.logger.warn(
        "Backtrace cleaner raised #{Utils.exception_name(exception)}: #{Utils.exception_message(exception)}" \
        "#{" (#{where})" if where}, so the full backtrace is logged"
      )
    rescue StandardError
      nil
    end
    private_class_method :report
  end
end
