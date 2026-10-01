# frozen_string_literal: true

class CSV
  # CSV::DialectSniffer inspects a bounded sample of bytes from the
  # beginning of a CSV-like input and reports which dialects
  # (column separator, quote character and row separator) are
  # consistent with that sample.
  #
  # It exists for the common situation where a provider says "the
  # file is CSV" without saying which delimiter, quote character or
  # line ending it uses.
  #
  # Properties:
  #
  # * The number of bytes read from the input is strictly bounded by
  #   +sample_bytes+.
  # * The candidate separators are supplied by the caller.
  # * Quoted content (including quoted line breaks and quoted
  #   separators) is honored while evaluating the sample, and a
  #   sample that happens to end in the middle of a field or record
  #   is treated as truncated, never as malformed.
  # * Every candidate is reported together with the reasons for its
  #   evaluation.  When several candidates fit the sample equally
  #   well the result is explicitly ambiguous; the sniffer never
  #   silently picks the first one.
  # * The input does not need to be rewindable: the sampled bytes
  #   are buffered and replayed, so once a dialect is chosen the
  #   data can be parsed from the very first record.
  #
  # Example:
  #   result = CSV.sniff_dialect(io, sample_bytes: 4096,
  #                             col_seps: [",", ";", "\t"],
  #                             quote_chars: ["\"", "'"],
  #                             row_seps: ["\r\n", "\n", "\r"])
  #   result.ambiguous?          # => false
  #   result.dialect             # => {col_sep: ";", row_sep: "\n"}
  #   result.candidates          # => all candidates, best first
  #   csv = result.csv           # parses from the first record
  class DialectSniffer
    # Base class for errors raised by CSV::DialectSniffer.
    class Error < StandardError
    end

    # Raised when the sample cannot be produced by any of the
    # caller-supplied candidate dialects.
    class NoViableDialectError < Error
    end

    # Raised when two or more candidate dialects fit the sample
    # equally well; the sniffer refuses to pick one arbitrarily.
    class AmbiguousDialectError < Error
    end

    DEFAULT_SAMPLE_BYTES = 4096
    DEFAULT_COL_SEPS = [",", ";", "\t", "|"].freeze
    DEFAULT_QUOTE_CHARS = ['"', "'"].freeze
    DEFAULT_ROW_SEPS = ["\r\n", "\n", "\r"].freeze

    # The evaluation of a single candidate dialect against the
    # sample.  Candidates are created by CSV::DialectSniffer#sniff.
    class Candidate
      # The column separator of this candidate.
      attr_reader :col_sep
      # The quote character of this candidate; +nil+ when quoting is
      # disabled or when no quote character was observable in the
      # sample (see #quote_char_determined?).
      attr_reader :quote_char
      # The row separator of this candidate.
      attr_reader :row_sep
      # The column counts of the complete records found in the
      # sample, in order of appearance.
      attr_reader :column_counts
      # Human readable explanations of how this candidate was
      # judged.
      attr_reader :reasons

      def initialize(col_sep:, quote_char:, row_sep:, column_counts:,
                     blank_rows:, stray_line_breaks:, truncated:,
                     malformed:, quote_char_determined:, reasons:)
        @col_sep = col_sep
        @quote_char = quote_char
        @row_sep = row_sep
        @column_counts = column_counts
        @blank_rows = blank_rows
        @stray_line_breaks = stray_line_breaks
        @truncated = truncated
        @malformed = malformed
        @quote_char_determined = quote_char_determined
        @reasons = reasons
      end

      # Whether the sample can be produced by this dialect.  A
      # candidate that is not viable contradicts the sample (see
      # #malformed_reason).
      def viable?
        @malformed.nil?
      end

      # Why the sample contradicts this dialect; +nil+ when viable.
      def malformed_reason
        @malformed
      end

      # Whether the sample ended in the middle of a record.  A
      # truncated sample is expected (the sample is bounded) and is
      # never treated as malformed.
      def truncated?
        @truncated
      end

      # The number of complete records observed in the sample.
      def rows
        @column_counts.size
      end

      # Whether every complete record has the same number of
      # columns.
      def consistent?
        not @column_counts.empty? and @column_counts.uniq.size == 1
      end

      # The number of complete records with more than one column.
      def multi_column_rows
        @column_counts.count {|count| count > 1}
      end

      # Whether the quote character of this candidate was actually
      # determined by the sample.  When no quote character appears
      # in the sample and the caller offered several candidates, the
      # quote character cannot be told apart and this is +false+.
      def quote_char_determined?
        @quote_char_determined
      end

      # A comparable score; higher is better.  +nil+ when the
      # candidate is not viable.  The score prefers, in order:
      # records with several columns, consistent column counts, more
      # complete records, fewer stray line breaks inside fields, and
      # wider records.
      def score
        return nil unless viable?
        [multi_column_rows,
         (consistent? ? 1 : 0),
         rows,
         -@stray_line_breaks,
         @column_counts.max || 0]
      end

      # The dialect as CSV parsing options.  The quote character is
      # only included when it was determined by the sample.
      def to_h
        dialect = {col_sep: @col_sep, row_sep: @row_sep}
        dialect[:quote_char] = @quote_char if @quote_char_determined
        dialect
      end

      def inspect
        state = viable? ? "score=#{score.inspect}" : "malformed"
        "#<#{self.class} #{to_h.inspect} #{state}>"
      end
    end

    # The outcome of CSV::DialectSniffer#sniff.
    class Result
      # All evaluated candidates, best first.  Non-viable candidates
      # come last and carry their #malformed_reason.
      attr_reader :candidates
      # The sampled bytes that were evaluated.
      attr_reader :sample

      def initialize(candidates, sample, &input_factory)
        @candidates = candidates
        @sample = sample
        @input_factory = input_factory
        @consumed = false
        viable = candidates.select(&:viable?)
        best_score = viable.map(&:score).max
        if best_score
          @best_candidates = viable.select {|c| c.score == best_score}
        else
          @best_candidates = []
        end
      end

      # Whether exactly one candidate fits the sample best.
      def detected?
        @best_candidates.size == 1
      end

      # Whether several candidates fit the sample equally well.
      def ambiguous?
        @best_candidates.size > 1
      end

      # The single best Candidate, or +nil+ when the result is
      # ambiguous or no candidate is viable.
      def best
        @best_candidates.first if detected?
      end

      # The candidates that tie for the best score.
      def best_candidates
        @best_candidates.dup
      end

      # The detected dialect as CSV parsing options, or +nil+ when
      # the result is ambiguous or no candidate is viable.
      def dialect
        best&.to_h
      end

      # Builds a CSV object that parses the input with the detected
      # dialect, starting from the very first record: the sampled
      # bytes are replayed before the remainder of the input, so the
      # input does not need to be rewindable.
      #
      # Extra +options+ are merged over the detected dialect and
      # passed to CSV.new.  When the detection is ambiguous (or no
      # candidate is viable), AmbiguousDialectError (or
      # NoViableDialectError) is raised unless +options+ explicitly
      # provide a dialect to use instead.
      #
      # Note: for a non-rewindable input this method can be called
      # only once.
      def csv(**options)
        if @consumed
          raise Error, "the input was already consumed by a previous #csv call"
        end
        dialect = self.dialect
        if dialect.nil?
          if options.empty?
            if ambiguous?
              descriptions = @best_candidates.map {|c| c.to_h.inspect}
              raise AmbiguousDialectError,
                    "multiple dialects fit the sample equally well: " \
                    "#{descriptions.join(', ')}; " \
                    "inspect #candidates and pass explicit options"
            else
              raise NoViableDialectError,
                    "no candidate dialect fits the sample; " \
                    "inspect #candidates for the reasons"
            end
          end
          dialect = {}
        end
        @consumed = true
        CSV.new(@input_factory.call, **dialect.merge(options))
      end
    end

    # An IO-like wrapper that replays the sampled bytes and then
    # delegates to the remainder of the original (possibly
    # non-rewindable) input.  It implements just the subset of the
    # IO protocol that CSV::Parser relies on.
    class SampledInput
      def initialize(sample, input)
        @buffer = sample
        @input = input
      end

      def gets(separator, limit = nil)
        return @input.gets(separator, limit) if @buffer.empty?

        if separator.nil?
          take = limit ? [limit, @buffer.bytesize].min : @buffer.bytesize
        else
          index = @buffer.index(separator)
          if index
            take = @buffer.byteslice(0, index).bytesize + separator.bytesize
            take = limit if limit && limit < take
          elsif limit && limit < @buffer.bytesize
            take = limit
          else
            take = @buffer.bytesize
          end
        end
        chunk = @buffer.byteslice(0, take)
        @buffer = @buffer.byteslice(take, @buffer.bytesize - take) || +""
        chunk
      end

      def eof?
        @buffer.empty? and
          (not @input.respond_to?(:eof?) or @input.eof?)
      end
      alias_method :eof, :eof?

      def close
        @input.close if @input.respond_to?(:close)
      end
    end

    class << self
      def sniff(input, **options)
        new(input, **options).sniff
      end
    end

    def initialize(input,
                   sample_bytes: DEFAULT_SAMPLE_BYTES,
                   col_seps: DEFAULT_COL_SEPS,
                   quote_chars: DEFAULT_QUOTE_CHARS,
                   row_seps: DEFAULT_ROW_SEPS)
      unless sample_bytes.is_a?(Integer) and sample_bytes > 0
        raise ArgumentError, "sample_bytes must be a positive Integer"
      end
      if col_seps.nil? or col_seps.empty?
        raise ArgumentError, "col_seps must not be empty"
      end
      if row_seps.nil? or row_seps.empty?
        raise ArgumentError, "row_seps must not be empty"
      end
      if quote_chars.nil? or quote_chars.empty?
        raise ArgumentError, "quote_chars must not be empty"
      end
      @input = input
      @sample_bytes = sample_bytes
      @col_seps = col_seps
      @quote_chars = quote_chars
      @row_seps = row_seps
    end

    def sniff
      sample, eof_reached, rest = take_sample
      @eof_reached = eof_reached
      encoding = sample.encoding
      col_seps = @col_seps.map {|sep| encode_for(sep, encoding)}
      row_seps = @row_seps.map {|sep| encode_for(sep, encoding)}
      quote_chars = @quote_chars.map {|char| char && encode_for(char, encoding)}

      # A quote character that never appears in the sample cannot be
      # told apart from any other unobserved quote character, so
      # only quote characters that are actually observable are
      # evaluated.  When none is observable a single +nil+
      # placeholder is evaluated and the quote character is reported
      # as undetermined instead of guessing.
      observed = quote_chars.compact.select {|char| sample.include?(char)}
      evaluated = observed.dup
      evaluated << nil if quote_chars.include?(nil)
      if evaluated.empty?
        evaluated = [quote_chars.first] if @quote_chars.size == 1
        evaluated = [nil] if evaluated.empty?
      end
      determined = (not observed.empty?) or @quote_chars.size == 1

      candidates = []
      col_seps.each do |col_sep|
        row_seps.each do |row_sep|
          evaluated.each do |quote_char|
            candidates << evaluate(sample, col_sep, quote_char, row_sep,
                                   quote_char_determined: determined)
          end
        end
      end
      if candidates.none?(&:viable?) and
         not evaluated.include?(nil) and
         @quote_chars.size > 1
        # Every offered quote character contradicts the sample (a
        # lone apostrophe in a name, say); maybe the data simply
        # does not use quoting.  This fallback is only tried when
        # nothing else works, so a quote-aware interpretation always
        # wins when it is viable.
        col_seps.each do |col_sep|
          row_seps.each do |row_sep|
            candidates << evaluate(sample, col_sep, nil, row_sep,
                                   quote_char_determined: false,
                                   unquoted_fallback: true)
          end
        end
      end
      Result.new(sort_candidates(candidates), sample) do
        SampledInput.new(sample, rest)
      end
    end

    private

    def encode_for(string, encoding)
      string.encode(encoding)
    rescue EncodingError
      string
    end

    def take_sample
      if @input.is_a?(String)
        sample = @input.byteslice(0, @sample_bytes) || +""
        rest = @input.byteslice(sample.bytesize..-1) || +""
        [sample, rest.empty?, StringIO.new(rest)]
      else
        chunks = []
        read = 0
        eof = false
        while read < @sample_bytes
          begin
            if @input.respond_to?(:readpartial)
              chunk = @input.readpartial(@sample_bytes - read)
            elsif @input.respond_to?(:read)
              chunk = @input.read(@sample_bytes - read)
            else
              raise ArgumentError,
                    "input must be a String or respond to readpartial/read"
            end
          rescue EOFError
            eof = true
            break
          end
          if chunk.nil? or chunk.empty?
            eof = true
            break
          end
          chunks << chunk
          read += chunk.bytesize
        end
        sample = chunks.empty? ? +"" : chunks.join
        if @input.respond_to?(:external_encoding) &&
           (external_encoding = @input.external_encoding)
          sample = sample.dup.force_encoding(external_encoding)
        end
        [sample, eof, @input]
      end
    end

    # Scans the sample with one candidate dialect.  The scanner
    # honors quoted content (escaped quotes, quoted separators and
    # quoted line breaks) and never treats a sample that ends in the
    # middle of a record as malformed.
    def evaluate(sample, col_sep, quote_char, row_sep,
                 quote_char_determined:, unquoted_fallback: false)
      column_counts = []
      blank_rows = 0
      stray_line_breaks = 0
      truncated = false
      malformed = nil
      pending_fields = 0
      in_field = false
      in_quotes = false
      after_quote = false

      i = 0
      n = sample.length
      col_sep_size = col_sep.length
      row_sep_size = row_sep.length
      while i < n
        char = sample[i]
        if in_quotes
          if char == quote_char
            if sample[i + 1] == quote_char
              i += 2 # escaped quote inside quoted field
            else
              in_quotes = false
              after_quote = true
              i += 1
            end
          else
            i += 1 # quoted content: separators stay ordinary data
          end
        elsif after_quote
          if sample[i, col_sep_size] == col_sep
            pending_fields += 1
            after_quote = false
            in_field = false
            i += col_sep_size
          elsif sample[i, row_sep_size] == row_sep
            column_counts << pending_fields + 1
            pending_fields = 0
            after_quote = false
            in_field = false
            i += row_sep_size
          else
            malformed = "unexpected #{char.inspect} after a closing quote"
            break
          end
        elsif quote_char and char == quote_char
          if in_field
            malformed = "quote character #{quote_char.inspect} appears " \
                        "inside an unquoted field"
            break
          end
          in_quotes = true
          in_field = true
          i += 1
        elsif sample[i, col_sep_size] == col_sep
          pending_fields += 1
          in_field = false
          i += col_sep_size
        elsif sample[i, row_sep_size] == row_sep
          if pending_fields.zero? and not in_field
            blank_rows += 1
          else
            column_counts << pending_fields + 1
            pending_fields = 0
            in_field = false
          end
          i += row_sep_size
        else
          stray_line_breaks += 1 if char == "\r" or char == "\n"
          in_field = true
          i += 1
        end
      end

      if malformed.nil?
        if in_quotes
          if @eof_reached
            malformed = "unclosed quoted field at the end of the input"
          else
            # The sample simply stopped inside a quoted field.
            truncated = true
          end
        elsif pending_fields > 0 or in_field or after_quote
          if @eof_reached
            column_counts << pending_fields + 1
          else
            # The sample simply stopped in the middle of a record.
            truncated = true
          end
        end
      end

      reasons = []
      if malformed
        reasons << "rejected: #{malformed}"
      else
        if unquoted_fallback
          reasons << "every offered quote character contradicts the " \
                     "sample; evaluated without quoting"
        end
        if column_counts.empty?
          reasons << "no complete record found in the sample"
        else
          counts = column_counts.uniq
          if counts.size == 1
            reasons << "#{column_counts.size} complete record(s) with " \
                       "#{counts.first} column(s) each"
          else
            reasons << "#{column_counts.size} complete record(s) with " \
                       "varying column counts #{column_counts.inspect}"
          end
          reasons << "#{multi_column_rows_of(column_counts)} record(s) " \
                     "have more than one column"
        end
        if truncated
          reasons << "the sample ended in the middle of a record; " \
                     "the partial record was ignored"
        end
        reasons << "#{blank_rows} blank line(s) ignored" if blank_rows > 0
        if stray_line_breaks > 0
          reasons << "#{stray_line_breaks} line-break character(s) appear " \
                     "inside fields; the row separator may be wrong"
        end
        unless quote_char_determined or unquoted_fallback
          reasons << "no quote character observed in the sample; " \
                     "quote_char is undetermined"
        end
      end

      Candidate.new(col_sep: col_sep,
                    quote_char: quote_char,
                    row_sep: row_sep,
                    column_counts: column_counts,
                    blank_rows: blank_rows,
                    stray_line_breaks: stray_line_breaks,
                    truncated: truncated,
                    malformed: malformed,
                    quote_char_determined: quote_char_determined,
                    reasons: reasons)
    end

    def multi_column_rows_of(column_counts)
      column_counts.count {|count| count > 1}
    end

    def sort_candidates(candidates)
      candidates.sort_by do |candidate|
        score = candidate.score
        if score
          [0, *score.map {|value| -value}]
        else
          [1, 0, 0, 0, 0, 0]
        end
      end
    end
  end

  # :call-seq:
  #   CSV.sniff_dialect(input) -> CSV::DialectSniffer::Result
  #   CSV.sniff_dialect(input, sample_bytes:, col_seps:, quote_chars:, row_seps:) -> CSV::DialectSniffer::Result
  #
  # Detects the dialect (column separator, quote character and row
  # separator) of +input+ by evaluating caller-supplied candidates
  # against a sample of at most +sample_bytes+ bytes.
  #
  # +input+ may be a String or any object responding to
  # +readpartial+ or +read+; it does not need to be rewindable.
  #
  # Returns a CSV::DialectSniffer::Result that reports every
  # candidate together with the reasons for its evaluation, flags
  # ambiguity explicitly instead of picking a default, and can build
  # a CSV object that parses from the first record:
  #
  #   result = CSV.sniff_dialect(io)
  #   if result.detected?
  #     result.csv.each {|row| p row }
  #   end
  #
  # See CSV::DialectSniffer for details.
  def self.sniff_dialect(input, **options)
    DialectSniffer.sniff(input, **options)
  end
end
