# frozen_string_literal: true

require "stringio"

class CSV
  # Raised by CSV.parse_detected when the inspected sample is equally
  # compatible with more than one dialect, so no dialect can be chosen
  # without additional guidance from the caller.
  class AmbiguousDialectError < MalformedCSVError
    # The CSV::DialectDetection result that describes the ambiguity.
    attr_reader :detection

    def initialize(detection)
      @detection = detection
      message = +"Can't choose a CSV dialect automatically"
      dimensions = detection.ambiguous_dimensions
      unless dimensions.empty?
        message << ": #{dimensions.inspect} are ambiguous"
      end
      super(message, 0)
    end
  end

  #
  # A candidate CSV dialect: the three settings that decide how data is
  # split into rows and fields.
  #
  # +col_sep+::    Column separator (+String+).
  # +row_sep+::    Row separator (+String+), or +nil+ when the inspected
  #                sample does not contain any line break (in that case
  #                +row_sep: :auto+ is used when parsing).
  # +quote_char+:: Quote character (+String+) or +nil+ for unquoted data.
  #
  Dialect = Struct.new(:col_sep, :row_sep, :quote_char) do
    # Returns keyword options usable with CSV.new and friends.
    def to_csv_options
      options = {
        col_sep: col_sep,
        quote_char: quote_char,
      }
      options[:row_sep] = row_sep.nil? ? :auto : row_sep
      options
    end
  end

  #
  # Result of a dialect detection run.
  #
  # +sample+::               The characters actually inspected (+String+).
  # +sample_bytes+::         Number of bytes requested for the sample.
  # +truncated+::            +true+ when more data follows the sample.
  # +candidates+::           All dialects consistent with the sample, best
  #                          first. Each element is a +Hash+ with
  #                          +:dialect+, +:score+ and +:reasons+ keys.
  # +best+::                 The unique best CSV::Dialect, or +nil+ when
  #                          the result is ambiguous (or no structural
  #                          evidence was found).
  # +ambiguous+::            +true+ when several dialects explain the
  #                          sample equally well.
  # +ambiguous_dimensions+:: Array of dimension names (+"col_sep"+,
  #                          +"row_sep"+ or +"quote_char"+) tied at the
  #                          top.
  # +rejected+::             Candidate settings that did not survive, each
  #                          as a +Hash+ with +:settings+ and +:reason+
  #                          keys.
  #
  DialectDetection = Struct.new(:sample,
                                :sample_bytes,
                                :truncated,
                                :candidates,
                                :best,
                                :ambiguous,
                                :ambiguous_dimensions,
                                :rejected) do
    alias_method :ambiguous?, :ambiguous
  end

  class << self
    # :call-seq:
    #   CSV.detect_dialect(string_or_io, **options) -> CSV::DialectDetection
    #
    # Inspects at most +sample_bytes+ bytes of +string_or_io+ and ranks the
    # supplied candidate dialects. Quoted fields (including fields spanning
    # several physical lines) are respected: separators and line breaks
    # inside quotes never count as structure.
    #
    # A sample that ends in the middle of a field, or inside a quoted
    # field, is treated as a truncated but valid file, not as corrupt
    # data.
    #
    # Options:
    # +col_seps+::     Candidate column separators. Defaults to
    #                  <tt>[",", ";", "\t"]</tt>.
    # +quote_chars+::  Candidate quote characters. Defaults to
    #                  <tt>['"', nil]</tt> (+nil+ means unquoted data).
    # +row_seps+::     Candidate row separator rules. Defaults to
    #                  <tt>[:auto]</tt>; concrete strings such as
    #                  <tt>"\n"</tt>, <tt>"\r\n"</tt> or <tt>"\r"</tt> may
    #                  be given as well.
    # +sample_bytes+:: Maximum number of bytes to inspect. Defaults to
    #                  65536. The stream is read sequentially and never
    #                  rewound, so this works with non-rewindable IO.
    #
    # The returned CSV::DialectDetection always lists every surviving
    # candidate with the evidence behind its score; when two dialects are
    # equally justified, +best+ is +nil+ and +ambiguous?+ is +true+ instead
    # of silently picking the first one.
    def detect_dialect(string_or_io,
                       col_seps: [",", ";", "\t"],
                       quote_chars: ['"', nil],
                       row_seps: [:auto],
                       sample_bytes: 64 * 1024)
      detector = DialectDetector.new(string_or_io,
                                     col_seps: col_seps,
                                     quote_chars: quote_chars,
                                     row_seps: row_seps,
                                     sample_bytes: sample_bytes)
      detector.detect
    end

    # :call-seq:
    #   CSV.parse_detected(string_or_io, **options) {|csv| ... }
    #
    # Runs CSV.detect_dialect and then parses the whole input from the
    # first record using the winning dialect. Bytes consumed while probing
    # are replayed before the rest of the stream is read, so
    # +string_or_io+ does not need to support +seek+/+rewind+.
    #
    # +options+ are the same as CSV.detect_dialect plus ordinary parsing
    # options (e.g. +headers: true+).
    #
    # Raises CSV::AmbiguousDialectError when detection is ambiguous;
    # rescue it and read the CSV::DialectDetection carried by the error to
    # decide how to proceed.
    def parse_detected(string_or_io, **options, &block)
      detection_keys = [:col_seps, :quote_chars, :row_seps, :sample_bytes]
      detection_options = options.slice(*detection_keys)
      parsing_options = options.except(*detection_keys)

      detector = DialectDetector.new(string_or_io, **detection_options)
      detection = detector.detect
      if detection.ambiguous?
        raise AmbiguousDialectError.new(detection)
      end

      csv = CSV.new(detector.replay_io,
                    **detection.best.to_csv_options,
                    **parsing_options)
      if block_given?
        begin
          yield csv
        ensure
          close_source(csv)
        end
      else
        begin
          csv.read
        ensure
          close_source(csv)
        end
      end
    end

    private
    def close_source(csv)
      io = csv.to_io
      io.close if io.respond_to?(:close) && !io.closed?
    rescue IOError
      # Already closed; nothing to do.
    end
  end

  # Internal: drives sample collection, dialect evaluation and IO replay.
  # Note: Don't use this class directly.
  class DialectDetector # :nodoc:
    def initialize(string_or_io,
                   col_seps: [",", ";", "\t"],
                   quote_chars: ['"', nil],
                   row_seps: [:auto],
                   sample_bytes: nil)
      @source = string_or_io
      @col_seps = Array(col_seps)
      @quote_chars = Array(quote_chars)
      @row_sep_rules = Array(row_seps)
      @sample_bytes = sample_bytes.nil? ? 64 * 1024 : sample_bytes

      validate_candidates
    end

    def detect
      sample, truncated, replay_io = collect_sample
      result = DialectProbe.evaluate(sample,
                                     truncated: truncated,
                                     col_seps: @col_seps,
                                     quote_chars: @quote_chars,
                                     row_sep_rules: @row_sep_rules)
      @replay_io = replay_io
      DialectDetection.new(sample,
                           @sample_bytes,
                           truncated,
                           result[:candidates],
                           result[:best],
                           result[:ambiguous],
                           result[:ambiguous_dimensions],
                           result[:rejected])
    end

    # Returns an IO that yields the consumed sample followed by the
    # (still unread) remainder of the source.
    def replay_io
      @replay_io
    end

    private
    def validate_candidates
      [[:col_seps, @col_seps, false],
       [:quote_chars, @quote_chars, true]].each do |name, values, allow_nil|
        if values.empty?
          raise ArgumentError, "#{name} must contain at least one candidate"
        end
        values.each do |value|
          next if allow_nil && value.nil?
          unless value.is_a?(String) && !value.empty?
            message = "each #{name} candidate must be a non empty String"
            message << " or nil" if allow_nil
            raise ArgumentError, message
          end
        end
      end
      if @row_sep_rules.empty?
        raise ArgumentError, "row_seps must contain at least one candidate"
      end
      @row_sep_rules.each do |value|
        unless value == :auto || (value.is_a?(String) && !value.empty?)
          raise ArgumentError,
                "each row_seps candidate must be :auto or a non empty String"
        end
      end
      unless @sample_bytes.is_a?(Integer) && @sample_bytes >= 0
        raise ArgumentError, "sample_bytes must be a non negative Integer"
      end
    end

    def collect_sample
      if @source.is_a?(String)
        string = @source
        sample = string.byteslice(0, @sample_bytes) || +""
        [sample, sample.bytesize < string.bytesize,
         StringIO.new(string)]
      else
        read_stream_sample
      end
    end

    def read_stream_sample
      chunks = []
      total = 0
      eof = false
      while total < @sample_bytes
        wanted = [8192, @sample_bytes - total].min
        chunk = read_sample_chunk(wanted)
        if chunk.nil? || chunk.empty?
          eof = true
          break
        end
        chunks << chunk
        total += chunk.bytesize
      end
      sample = chunks.join
      replay_prefix = sample

      # Drop a partial multibyte character cut by the byte limit.
      unless sample.valid_encoding?
        cut = sample.bytesize
        cut -= 1 until cut.zero? ||
                       sample.byteslice(0, cut).valid_encoding?
        leftover = sample.byteslice(cut, sample.bytesize - cut)
        sample = sample.byteslice(0, cut) || +""
        replay_prefix = sample + (leftover || "")
      end

      truncated = !eof || replay_prefix.bytesize > sample.bytesize
      [sample, truncated, ReplayIO.new(replay_prefix, @source)]
    end

    def read_sample_chunk(wanted)
      chunk = @source.read(wanted)
      if chunk && chunk.bytesize > wanted
        chunk.byteslice(0, wanted)
      else
        chunk
      end
    rescue ArgumentError
      # The stream may reject a length argument; read unbound and bound the
      # returned chunk ourselves.
      chunk = @source.read
      return nil if chunk.nil?
      chunk.byteslice(0, wanted)
    end
  end

  # Internal: an IO-like object that replays buffered bytes followed by
  # the unread remainder of the original stream. Only the small surface
  # used by CSV::Parser is implemented. Chunks handed out never split a
  # multibyte character, which keeps the parser's per chunk encoding
  # validation happy even when the sample ended in the middle of one.
  class ReplayIO # :nodoc:
    def initialize(prefix, source)
      @encoding = source.respond_to?(:external_encoding) ?
                    source.external_encoding : nil
      @buffer = String.new(encoding: prefix.encoding)
      @buffer.concat(prefix)
      @source = source
      @eof = false
    end

    # Returns the next record. When +limit+ is given the returned chunk is
    # at most that many bytes, but it may be shorter to avoid returning a
    # chunk that ends in the middle of a multibyte character.
    def gets(separator = $/, limit = nil)
      loop do
        index = separator && @buffer.index(separator)
        if index
          length = index + separator.length
          length = limit if limit && length > limit
          return consume(length)
        end
        if limit && @buffer.bytesize >= limit
          return consume(limit)
        end
        break unless pull_more
      end
      return nil if @buffer.empty?
      consume(limit)
    end

    def read(length = nil)
      until @eof
        break if length && @buffer.bytesize >= length
        break unless pull_more(length)
      end
      consume(length)
    end

    def eof?
      return false unless @buffer.empty?
      @eof || (@source.respond_to?(:eof?) && @source.eof?)
    end

    def external_encoding
      @encoding
    end

    def internal_encoding
      nil
    end

    def close
      @source.close if @source.respond_to?(:close) && !@source.closed?
    rescue IOError
      # Already closed; nothing to do.
    end

    def closed?
      @source.respond_to?(:closed?) && @source.closed?
    end

    def respond_to_missing?(name, include_private = false)
      @source.respond_to?(name, include_private) || super
    end

    def method_missing(name, *args, &block)
      if @source.respond_to?(name)
        @source.public_send(name, *args, &block)
      else
        super
      end
    end

    private
    # Pulls up to +max_bytes+ more bytes from the source. Streams that only
    # accept a mandatory read length are supported.
    def pull_more(max_bytes = nil)
      return false if @eof
      chunk = read_from_source(max_bytes || 8192)
      if chunk.nil? || chunk.empty?
        @eof = true
        return false
      end
      @buffer << chunk
      true
    end

    def read_from_source(length)
      if @source.respond_to?(:read)
        begin
          return @source.read(length)
        rescue ArgumentError
          # Some streams reject a length argument; retry without one and
          # bound the result ourselves.
          chunk = @source.read
          return chunk if chunk.nil?
          return chunk.byteslice(0, length) || chunk
        end
      end
      @source.public_send(:gets)
    end

    # Takes up to +length+ bytes, extending past it when needed so the
    # returned string never ends in the middle of a multibyte character.
    def consume(length)
      return nil if @buffer.empty? && @eof
      length = @buffer.bytesize if length.nil? || length > @buffer.bytesize
      while length < @buffer.bytesize &&
            !@buffer.byteslice(0, length).valid_encoding?
        length += 1
      end
      line = @buffer.byteslice(0, length) || +""
      @buffer = @buffer.byteslice(length..-1) || +""
      line
    end
  end

  # Internal: quote aware evaluation of candidate dialects against a sample.
  class DialectProbe # :nodoc:
    Result = Struct.new(:complete_rows,
                        :complete_column_counts,
                        :dangling_columns,
                        :quoted_fields,
                        :dangling_quote,
                        :stray_line_chars,
                        :malformed,
                        :reason,
                        keyword_init: true) do
      def viable?
        !malformed
      end
    end

    def self.evaluate(sample,
                      truncated:,
                      col_seps:,
                      quote_chars:,
                      row_sep_rules:)
      new(sample,
          truncated,
          col_seps,
          quote_chars,
          row_sep_rules).evaluate
    end

    def initialize(sample, truncated, col_seps, quote_chars, row_sep_rules)
      @sample = sample
      @truncated = truncated
      @col_seps = col_seps
      @quote_chars = quote_chars
      @row_sep_rules = row_sep_rules
    end

    def evaluate
      rejected = []
      evaluations = []
      preference = 0

      @col_seps.each do |col_sep|
        @quote_chars.each do |quote_char|
          row_seps_for(quote_char).each do |row_sep|
            result = tokenize(col_sep, row_sep, quote_char)
            settings = {
              col_sep: col_sep,
              row_sep: row_sep,
              quote_char: quote_char,
            }
            if result.viable?
              evaluations << {
                settings: settings,
                result: result,
                preference: preference,
              }
            else
              rejected << {
                settings: settings,
                reason: result.reason,
              }
            end
            preference += 1
          end
        end
      end

      build_ranking(evaluations, rejected)
    end

    private
    # Expands the configured row separator rules for a quote character.
    # :auto is resolved using only line breaks that appear outside fields
    # quoted by +quote_char+. Returns the list of concrete row separators
    # (nil when undetermined) with duplicates removed.
    def row_seps_for(quote_char)
      resolved = []
      seen = {}
      @row_sep_rules.each do |rule|
        row_sep = rule == :auto ? detect_row_separator(quote_char) : rule
        key = row_sep.nil? ? :undetermined : row_sep
        unless seen.key?(key)
          seen[key] = true
          resolved << row_sep
        end
      end
      resolved
    end

    # Mirrors CSV's :auto detection: the first line break outside quotes
    # decides. Returns nil when the sample has no usable line break.
    def detect_row_separator(quote_char)
      index = 0
      in_quote = false
      while index < @sample.length
        char = @sample[index]
        if in_quote
          if char == quote_char
            if @sample[index + 1] == quote_char
              index += 2
              next
            end
            in_quote = false
          end
          index += 1
          next
        end
        case char
        when quote_char
          in_quote = true
        when "\r"
          if @sample[index + 1] == "\n"
            return "\r\n"
          end
          # A CR sitting exactly on a truncated edge could be the start of
          # CRLF, so defer the decision instead of committing to CR.
          return nil if @truncated && index == @sample.length - 1
          return "\r"
        when "\n"
          return "\n"
        end
        index += 1
      end
      nil
    end

    # Strict (non liberal) scan of the sample for one dialect. A dangling
    # field at the end is valid when the sample was truncated.
    def tokenize(col_sep, row_sep, quote_char)
      complete_counts = []
      quoted_fields = 0
      stray = 0
      fields_in_row = 1
      saw_data = false

      index = 0
      state = :field_start
      size = @sample.length

      while index < size
        char = @sample[index]
        case state
        when :field_start
          if quote_char && char == quote_char
            state = :in_quote
            saw_data = true
            index += 1
          elsif match_at?(index, row_sep)
            if saw_data
              complete_counts << fields_in_row
            end
            fields_in_row = 1
            saw_data = false
            index += row_sep.length
          elsif match_at?(index, col_sep)
            saw_data = true
            fields_in_row += 1
            index += col_sep.length
          elsif truncated_separator_prefix?(index, row_sep)
            # The sample may cut a multi character row separator in half.
            break
          else
            stray += 1 if char == "\r" || char == "\n"
            saw_data = true
            state = :in_unquoted
            index += 1
          end
        when :in_unquoted
          if match_at?(index, row_sep)
            complete_counts << fields_in_row
            fields_in_row = 1
            saw_data = false
            index += row_sep.length
            state = :field_start
          elsif match_at?(index, col_sep)
            fields_in_row += 1
            index += col_sep.length
            state = :field_start
          elsif truncated_separator_prefix?(index, row_sep)
            break
          else
            return malformed("Illegal quoting") if quote_char &&
                                                   char == quote_char
            stray += 1 if char == "\r" || char == "\n"
            index += 1
          end
        when :in_quote
          if char == quote_char
            if @sample[index + 1] == quote_char
              index += 2
            else
              state = :after_quote
              quoted_fields += 1
              index += 1
            end
          else
            index += 1
          end
        when :after_quote
          if match_at?(index, col_sep)
            fields_in_row += 1
            index += col_sep.length
            state = :field_start
          elsif match_at?(index, row_sep)
            complete_counts << fields_in_row
            fields_in_row = 1
            saw_data = false
            index += row_sep.length
            state = :field_start
          else
            return malformed("Any value after quoted field isn't allowed")
          end
        end
      end

      if state == :in_quote && !@truncated
        return malformed("Unclosed quoted field")
      end

      # The last physical line may be incomplete (sample cut in the middle
      # of a record). It is kept apart from the complete rows so scoring
      # can give it less weight.
      dangling_quote = state == :in_quote
      dangling_columns = nil
      if saw_data || dangling_quote
        dangling_columns = fields_in_row
      end

      Result.new(complete_rows: complete_counts.size,
                 complete_column_counts: complete_counts,
                 dangling_columns: dangling_columns,
                 quoted_fields: quoted_fields,
                 dangling_quote: dangling_quote,
                 stray_line_chars: stray,
                 malformed: false,
                 reason: nil)
    end

    def match_at?(index, separator)
      !separator.nil? &&
        separator.length > 0 &&
        @sample[index, separator.length] == separator
    end

    # True when the tail of the sample is a proper prefix of +row_sep+,
    # which is only benign if the sample is truncated.
    def truncated_separator_prefix?(index, row_sep)
      @truncated &&
        !row_sep.nil? &&
        row_sep.length > 1 &&
        (1...row_sep.length).any? do |length|
          index + length == @sample.length &&
            @sample[index, length] == row_sep[0, length]
        end
    end

    def malformed(reason)
      Result.new(complete_rows: 0,
                 complete_column_counts: [],
                 dangling_columns: nil,
                 quoted_fields: 0,
                 dangling_quote: false,
                 stray_line_chars: 0,
                 malformed: true,
                 reason: reason)
    end

    def build_ranking(evaluations, rejected)
      if evaluations.empty?
        return {
          candidates: [],
          best: nil,
          ambiguous: true,
          ambiguous_dimensions: [],
          rejected: rejected,
        }
      end

      scored = evaluations.map do |evaluation|
        score, reasons = score(evaluation)
        {
          dialect: Dialect.new(evaluation[:settings][:col_sep],
                               evaluation[:settings][:row_sep],
                               evaluation[:settings][:quote_char]),
          score: score,
          reasons: reasons,
          evaluation: evaluation,
        }
      end

      scored.sort_by! {|entry| entry[:score]}
      scored.reverse!

      # The preference component only breaks display order: dialects tied
      # on everything observable are equally valid.
      evidence_score = proc {|entry| entry[:score][0..-2]}
      top_evidence = evidence_score.call(scored.first)
      tied = scored.select {|entry| evidence_score.call(entry) == top_evidence}
      dimensions = ambiguous_dimensions(tied)

      {
        candidates: scored.map do |entry|
          {
            dialect: entry[:dialect],
            score: entry[:score],
            reasons: entry[:reasons],
          }
        end,
        best: dimensions.empty? ? scored.first[:dialect] : nil,
        ambiguous: !dimensions.empty?,
        ambiguous_dimensions: dimensions,
        rejected: rejected,
      }
    end

    def score(evaluation)
      result = evaluation[:result]
      counts = result.complete_column_counts
      complete_rows = result.complete_rows

      modal = 0
      rows_match = 0
      if complete_rows > 0
        frequencies = counts.tally
        # Prefer the most frequent width; wider widths break frequency
        # ties (a separator that produces more structure wins) and column
        # count breaks the final tie, keeping the result independent of
        # candidate iteration order.
        modal, rows_match =
          frequencies.each_pair.max_by do |count, frequency|
            [frequency, count]
          end
      else
        # No completed record visible: the dangling record is the only
        # structural evidence, so use its width as a weak signal.
        modal = result.dangling_columns || 0
      end

      # Quoting is committed syntax: a dialect that parses quote characters
      # as part of well formed fields outranks one that treats the same
      # bytes as ordinary text.
      quote_rank =
        if result.quoted_fields > 0
          2
        elsif result.dangling_quote
          1
        else
          0
        end
      row_sep_observed = evaluation[:settings][:row_sep] ? 1 : 0

      reasons = []
      reasons << if complete_rows > 0
                   "#{complete_rows} complete row(s) tokenized"
                 else
                   "0 complete row(s) tokenized"
                 end
      if complete_rows > 0
        reasons << "#{rows_match}/#{complete_rows} complete row(s) " \
                   "have #{modal} column(s)"
      elsif result.dangling_columns
        reasons << "dangling row shows #{result.dangling_columns} column(s)"
      else
        reasons << "no row structure visible in the sample"
      end
      reasons << "stray line-ending characters outside quotes: " \
                 "#{result.stray_line_chars}"
      if result.quoted_fields > 0
        reasons << "#{result.quoted_fields} quoted field(s) honored"
      end
      if result.dangling_quote
        reasons << "sample ends inside a quoted field " \
                   "(only possible because the sample is truncated)"
      end
      unless evaluation[:settings][:row_sep]
        reasons << "no row separator visible in the sample"
      end

      # Higher is better; compared lexicographically:
      # [quote syntax rank, columns on the modal complete row,
      #  complete rows sharing that width, visible row separator,
      #  fewer stray line chars, earlier candidate in the caller's order]
      score = [
        quote_rank,
        modal,
        rows_match,
        row_sep_observed,
        -result.stray_line_chars,
        -evaluation[:preference],
      ]
      [score, reasons]
    end

    # Two top-scoring dialects are only in conflict when the dimension on
    # which they differ is actually observable in the sample.
    def ambiguous_dimensions(tied)
      return [] if tied.size < 2

      dimensions = []
      col_seps = tied.map {|entry| entry[:dialect].col_sep}.uniq
      row_seps = tied.map {|entry| entry[:dialect].row_sep}.uniq
      quote_chars = tied.map {|entry| entry[:dialect].quote_char}.uniq

      dimensions << "col_sep" if col_seps.size > 1
      dimensions << "row_sep" if row_seps.size > 1
      if quote_chars.size > 1
        observed = tied.any? do |entry|
          result = entry[:evaluation][:result]
          result.quoted_fields > 0 || result.dangling_quote
        end
        dimensions << "quote_char" if observed
      end
      dimensions
    end
  end
end
