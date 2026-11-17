module Dnsruby

# This class decodes a binary string containing the raw bytes of the message
# as in coming over the wire from a nameserver, and parses it into a
# Dnsruby::Message.
class MessageDecoder #:nodoc: all

  # Keeps a running @index containing the current position (like a cursor)
  # into the binary string.  In general 'get_' methods will position @index
  # to follow the data they have read.

  attr_reader :data, :index

  # Creates an instance of the decoder, optionally with code block
  # to be executed with the instance as its parameter.
  def initialize(data)
    @data = data
    @index = 0
    @limit = data.length
    # Compression addresses have 14 bits. Cache only validated suffixes at
    # addressable offsets, so repeated names do not repeat pointer traversal.
    @name_suffixes = {}
    yield self if block_given?
  end

  # Has bytes remaining in the binary string to be parsed?
  def has_remaining?
    @limit > @index
  end

  # Asserts that the specified position is a valid position in the buffer.
  # If not, raises a DecodeError.  If so, does nothing.
  def assert_buffer_position_valid(end_position)
    unless (0..@limit).include?(end_position)
      raise DecodeError.new("requested position of #{end_position} must be between 0 and buffer size (#{@limit}).")
    end
  end

  # Gets the byte value at the specified position
  def get_byte_at(position)
    assert_buffer_position_valid(position)
    # Wire strings have byte offsets. Avoid allocating a one-character
    # String for every label octet, retaining historical character indexing
    # for direct callers that supply a multibyte text encoding.
    if @data.encoding == Encoding::BINARY || @data.ascii_only?
      @data.getbyte(position)
    else
      @data[position]&.getbyte(0)
    end
  end

  # Gets a 16-bit length field from the binary string and yields to the block.
  # This will be the length of the next item to parse in the binary string.
  # Returns the object returned from that block.
  #
  # When this method returns, @index will point just after the parsed item.
  def get_length16
    save_limit = @limit
    len, = self.get_unpack('n')
    # A nested length may narrow its enclosing buffer, never extend it. In
    # particular, truncated UDP RDATA must raise DecodeError before a typed
    # decoder reads beyond the physical packet and raises an unrelated error.
    assert_buffer_position_valid(@index + len)
    @limit = @index + len
    parsed_data = yield(len)
    if @index < @limit
      message = "Junk exists; limit = #{@limit}, index = #{@index}"
      raise DecodeError.new(message)
    end
    assert_buffer_position_valid(@index)
    parsed_data
  ensure
    @limit = save_limit
  end

  # Returns the specified number of bytes from the binary string.
  # Length defaults to the remaining (not yet processed) size of the string.
  def get_bytes(len = @limit - @index)
    bytes = @data[@index, len]
    @index += len
    bytes
  end

  # Calls String.unpack to get numbers as specified in the template string.
  def get_unpack(template)
    len = 0

    template.bytes.each do |byte|
      case byte.chr
        when 'c', 'C', 'h', 'H'
          len += 1
        when 'n'
          len += 2
        when 'N'
          len += 4
        when '*'
          len = @limit - @index
        else
          raise StandardError.new("unsupported template: '#{byte.chr}' in '#{template}'")
      end
    end

    assert_buffer_position_valid(@index + len)
    number_array = @data.unpack("@#{@index}#{template}")
    @index += len
    number_array
  end

  # Gets a string whose 1-byte length is at @index, and the string starting at @index + 1.
  def get_string
    len = get_byte_at(@index) || 0
    assert_buffer_position_valid(@index + 1 + len)
    data_item = @data[@index + 1, len]
    @index += 1 + len
    data_item
  end

  # Gets all strings from @index to the end of the binary string.
  def get_string_list
    strings = []
    strings << get_string while has_remaining?
    strings
  end

  # Gets a Name from the current @index position.
  def get_name
    Name.new(get_labels)
  end

  # Returns labels starting at @index.
  def get_labels(limit = nil)
    limit = @index if limit.nil? || (@index < limit)
    labels = []
    return_index = nil
    expanded_length = 1 # The terminating root label.
    path = []
    suffix_end = 0

    loop do
      raise DecodeError, 'Truncated DNS name' unless has_remaining?
      if return_index && (suffix = @name_suffixes[@index])
        # Previously decoded suffixes must also fit the current RDATA bound.
        # The pointer's own two octets were checked before this lookup.
        assert_buffer_position_valid(suffix[2])
        expanded_length += suffix[1] - 1
        raise DecodeError, "DNS name exceeds #{Name::MaxNameLength} octets" if expanded_length > Name::MaxNameLength
        labels.concat(suffix[0])
        suffix_end = suffix[2]
        break
      end
      offset = @index
      label_index = labels.length
      temp = get_byte_at(@index)
      case temp
        when 0
          @index += 1
          path << [offset, label_index, @index]
          break
        when 192..255
          idx = get_unpack('n')[0] & 0x3fff
          if limit <= idx
            raise DecodeError.new('non-backward name pointer')
          end
          return_index ||= @index
          path << [offset, label_index, @index]
          @index = idx
          # Each target is below the preceding target (or original name).
          # The 14-bit pointer address therefore bounds traversal without
          # recursion, an arbitrary hop limit, or a growing visited set.
          limit = idx
        when 1..63
          expanded_length += 1 + temp
          if expanded_length > Name::MaxNameLength
            raise DecodeError, "DNS name exceeds #{Name::MaxNameLength} octets"
          end
          labels << get_label.string.dup.freeze
          path << [offset, label_index, @index]
        else
          raise DecodeError, 'Reserved or invalid DNS label type'
      end
    end
    # Pointer-only aliases share one immutable array. Label suffix arrays are
    # bounded by the 255-octet expanded name limit, not by packet record count.
    previous_index = previous_suffix = nil
    path.reverse_each do |offset, label_index, wire_end|
      suffix_end = [suffix_end, wire_end].max
      next if offset >= 0x4000
      value = label_index == previous_index ? previous_suffix : labels[label_index..-1].freeze
      @name_suffixes[offset] ||= [value, 1 + value.sum { |label| label.bytesize + 1 }, suffix_end].freeze
      previous_index, previous_suffix = label_index, value
    end
    @index = return_index if return_index
    # Name and Label expose mutable strings; cached data must never be shared
    # with callers or between separately decoded names.
    labels.map { |label| Name::Label.new(label.dup) }
  end

  # Gets a single label.
  def get_label
    begin
      Name::Label.new(get_string)
    rescue ResolvError => e
      raise DecodeError.new(e) # Turn it into something more suitable
    end
  end

  # Gets a question record.
  def get_question
    name = self.get_name
    type, klass = self.get_unpack('nn')
    klass = Classes.new(klass)
    Question.new(name, type, klass)
  end

  # Gets a resource record.
  def get_rr(update_section: nil)
    name = get_name
    type, klass, ttl = get_unpack('nnN')
    if type == Types::TSIG && (klass != Classes::ANY || ttl != 0)
      raise DecodeError.new('TSIG requires class ANY and TTL zero (RFC 8945 section 4.2)')
    end
    klass = Classes.new(klass)
    typeclass = RR.get_class(type, klass)
    #  @TODO@ Trap decode errors here, and somehow mark the record as bad.
    #  Need some way to represent raw data only
    record = get_length16 do |length|
      marker_context = ttl == 0 && (update_section.nil? ||
        (update_section == :prerequisite ||
          (update_section == :update && klass == Classes::ANY)))
      if length == 0 && marker_context && RR.empty_update_type?(type, klass)
        RR.empty_update_record(name, type, klass)
      else
        typeclass.decode_rdata(self)
      end
    end
    record.name = name
    # OPT overloads this field with EDNS flags and the extended RCODE.
    record.ttl = type != Types::OPT && ttl >= 0x80000000 ? 0 : ttl
    record.type = type
    record.klass = klass
    record.validate_decoded_rdata
    record
  end
end
end
