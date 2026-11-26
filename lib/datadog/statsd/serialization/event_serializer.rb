# frozen_string_literal: true

module Datadog
  class Statsd
    module Serialization
      class EventSerializer
        EVENT_BASIC_OPTIONS = {
          date_happened:    'd:',
          hostname:         'h:',
          aggregation_key:  'k:',
          priority:         'p:',
          source_type_name: 's:',
          alert_type:       't:',
        }.freeze

        def initialize(container_id, external_data, global_tags: [], max_payload_size: MAX_EVENT_SIZE)
          @tag_serializer = TagSerializer.new(global_tags)
          @field_serializer = FieldSerializer.new(container_id, external_data)
          # the serialized event must fit both the protocol event limit and
          # the outbound packet limit, the tightest of the two is the budget
          @max_payload_size = [max_payload_size, MAX_EVENT_SIZE].min
        end

        def format(title, text, options = EMPTY_OPTIONS)
          if options[:shortenable_text_range]
            return format_with_shortenable_range(title, text, options)
          end

          title = escape(title)
          text = escape(text)

          String.new.tap do |event|
            build_event(event, title, text, options)

            if event.bytesize > MAX_EVENT_SIZE
              if options[:truncate_if_too_long]
                event.slice!(MAX_EVENT_SIZE..event.length)
              else
                raise "Event #{title} payload is too big (more that 8KB), event discarded"
              end
            end
          end
        end

        protected

        attr_reader :tag_serializer
        attr_reader :field_serializer

        def escape(text)
          text.delete('|').tap do |t|
            t.gsub!("\n", '\n')
          end
        end

        private

        # Serializes an event whose caller-designated body segment
        # (options[:shortenable_text_range], expressed in characters on the
        # raw, unescaped text) may be shortened to make the whole packet fit.
        #
        # The segment is reduced to its longest complete UTF-8 prefix while
        # everything outside of it (the surrounding body text, the title, the
        # tags and the option fields) is always serialized in full. When even
        # the fixed parts do not fit in the budget, the event is rejected.
        def format_with_shortenable_range(title, text, options)
          shortenable_range = normalized_range(text, options[:shortenable_text_range])

          escaped_title = escape(title)
          range_start, range_length = shortenable_range
          before_segment = escape(text[0, range_start])
          after_segment = escape(text[range_start + range_length, text.length - range_start - range_length])

          segment = text[range_start, range_length]

          # size, in escaped bytes, of the prefix made of the first n raw
          # characters of the shortenable segment. Built in a single linear
          # pass so that the truncation point is never found by building the
          # whole overlong event packet over and over again.
          escaped_prefix_sizes = escaped_prefix_size_table(segment)

          tail_options_size = event_tail_options_size(options)
          fixed_size = event_header_size(escaped_title.bytesize, 0) +
                       escaped_title.bytesize + 1 + # "|" separator between title and text
                       before_segment.bytesize + after_segment.bytesize +
                       tail_options_size

          # even an empty shortenable segment does not fit: the fixed parts
          # alone are too big, the event has to be rejected.
          unless fixed_size <= @max_payload_size
            raise "Event #{escaped_title} payload is too big (more that #{@max_payload_size} bytes), event discarded"
          end

          # binary search (O(log n)) for the longest complete-character prefix
          # of the segment that keeps the whole packet within the budget.
          max_segment_chars = segment.length
          full_text_size = before_segment.bytesize +
                           escaped_prefix_sizes[max_segment_chars] +
                           after_segment.bytesize

          # The header embeds the decimal text size, whose number of digits can
          # itself change while shortening (e.g. 1000 -> 999). Sizing the header
          # with the largest possible digit count makes the feasibility test
          # monotonic in the segment length: a candidate that fits this upper
          # bound header can only fit the real, smaller header.
          upper_bound_header_size = event_header_size(escaped_title.bytesize, full_text_size)
          fixed_without_text = upper_bound_header_size + escaped_title.bytesize + 1 + tail_options_size
          text_budget = @max_payload_size - fixed_without_text

          first_too_long = (0..max_segment_chars).bsearch do |char_count|
            candidate_text_size = before_segment.bytesize +
                                  escaped_prefix_sizes[char_count] +
                                  after_segment.bytesize
            candidate_text_size > text_budget
          end

          # bsearch returns the first index for which the block is true; the
          # longest length that fits is therefore the preceding index. When no
          # index matches (every length fits, including the full segment) the
          # whole segment is kept.
          fitting_chars = first_too_long ? first_too_long - 1 : max_segment_chars

          # zero-cost characters (deleted "|" characters) create plateaus in
          # the size table: extend the retained prefix over every trailing
          # character that does not grow the escaped packet.
          while fitting_chars < max_segment_chars &&
                escaped_prefix_sizes[fitting_chars + 1] == escaped_prefix_sizes[fitting_chars]
            fitting_chars += 1
          end

          # The search above reserves room for the largest possible header.
          # Once the actual (possibly shorter) decimal text size is known, a
          # few more characters may fit: walk forward while the real packet
          # stays within the budget. This loop only ever advances, so it stays
          # linear in the number of reclaimed characters without rebuilding
          # any overlong packet.
          while fitting_chars < max_segment_chars
            next_text_size = before_segment.bytesize +
                             escaped_prefix_sizes[fitting_chars + 1] +
                             after_segment.bytesize
            next_total_size = event_header_size(escaped_title.bytesize, next_text_size) +
                              escaped_title.bytesize + 1 +
                              next_text_size +
                              tail_options_size
            break if next_total_size > @max_payload_size

            fitting_chars += 1
          end

          escaped_segment = escape(segment[0, fitting_chars])
          escaped_text = String.new
          escaped_text << before_segment
          escaped_text << escaped_segment
          escaped_text << after_segment

          String.new.tap do |event|
            build_event(event, escaped_title, escaped_text, options)
          end
        end

        # Appends the shared event body (header, title, text and every option,
        # tag and field) to +event+. +title+ and +text+ are already escaped.
        def build_event(event, title, text, options)
          event << '_e{'
          event << title.bytesize.to_s
          event << ','
          event << text.bytesize.to_s
          event << '}:'
          event << title
          event << '|'
          event << text

          # we are serializing the generic service check options
          # before serializing specialized options that need edge-cases
          EVENT_BASIC_OPTIONS.each do |option_key, shortcut|
            if value = options[option_key]
              event << '|'
              event << shortcut
              event << value.to_s.delete('|')
            end
          end

          # also returns the global tags from serializer
          if tags = tag_serializer.format(options[:tags])
            event << '|#'
            event << tags
          end

          if fields = field_serializer.format(options[:cardinality])
            event << fields
          end
        end

        # Bytesize of everything that follows the event text (basic options,
        # tags and origin/cardinality fields), including their separators.
        def event_tail_options_size(options)
          size = 0

          EVENT_BASIC_OPTIONS.each do |option_key, shortcut|
            if value = options[option_key]
              size += 1 + shortcut.bytesize + value.to_s.delete('|').bytesize
            end
          end

          if tags = tag_serializer.format(options[:tags])
            size += 2 + tags.bytesize # "|#"
          end

          if fields = field_serializer.format(options[:cardinality])
            size += fields.bytesize
          end

          size
        end

        # Bytesize of the "_e{title_size,text_size}:" header for the given
        # field sizes. The decimal length of each size is part of the header.
        def event_header_size(title_size, text_size)
          # "_e{" (3) + title size digits + "," (1) + text size digits + "}:" (2)
          3 + title_size.to_s.bytesize + 1 + text_size.to_s.bytesize + 2
        end

        # Returns an array of length segment.length + 1 where the element at
        # index n is the escaped bytesize of the first n raw characters of the
        # segment.
        def escaped_prefix_size_table(segment)
          sizes = [0]
          total = 0
          segment.each_char do |char|
            total += escaped_char_size(char)
            sizes << total
          end
          sizes
        end

        def escaped_char_size(char)
          if char == '|'
            0 # "|" characters are deleted by the event escaping
          elsif char == "\n"
            2 # a newline is escaped as the two characters "\n"
          else
            char.bytesize
          end
        end

        # Normalizes the caller provided range (character indexes on the raw
        # text) into a [start, length] pair of non-negative character offsets.
        def normalized_range(text, range)
          unless range.is_a?(Range) && range.begin && range.end
            raise ArgumentError, 'shortenable_text_range must be a Range of character indexes'
          end

          start = range.begin
          stop = range.end

          start += text.length if start < 0
          stop += text.length if stop < 0

          # inclusive length of the covered character span (0 for an empty
          # range such as 0...0 or length...length), computed before adjusting
          # the exclusive end
          length = range.exclude_end? ? stop - start : stop - start + 1

          # an empty range is only valid as an insertion point at the very end
          # of the text
          if length == 0 && start != text.length
            raise ArgumentError, 'shortenable_text_range is out of the event text bounds'
          end

          if start < 0 || start > text.length || length < 0 || start + length > text.length
            raise ArgumentError, 'shortenable_text_range is out of the event text bounds'
          end

          [start, length]
        end
      end
    end
  end
end
