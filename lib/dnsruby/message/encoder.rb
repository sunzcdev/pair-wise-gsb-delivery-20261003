module Dnsruby
class MessageEncoder #:nodoc: all
  def initialize
    @data = ''.b
    @names = {}
    yield self if block_given?
  end

  def to_s
    @data
  end

  def put_bytes(d)
    @data << d.b
  end

  def put_pack(template, *d)
    begin
      @data << d.pack(template)
    rescue Encoding::CompatibilityError
      raise Dnsruby::EncodeError.new("IDN support currently requires punycode string")
    end
  end

  def put_length16
    length_index = @data.length
    @data << "\0\0"
    data_start = @data.bytesize
    yield
    data_end = @data.bytesize
    length = data_end - data_start
    if length > 0xffff
      raise EncodeError.new("Length-prefixed data exceeds 65535 octets: #{length}")
    end
    @data[length_index, 2] = [length].pack("n")
  end

  def put_string(d)
    raise EncodeError, "Character-string exceeds 255 octets: #{d.bytesize}" if d.bytesize > 255
    self.put_pack("C", d.bytesize)
    @data << d.b
  end

  def put_string_list(ds)
    ds.each { |d| self.put_string(d) }
  end

  def put_rr(rr, canonical=false)
    #  RFC4034 Section 6.2
    put_name(rr.name, canonical)
    put_pack('nnN', rr.type.code, rr.klass.code, rr.ttl)
    put_length16 { rr.encode_rdata(self, canonical) }
  end

  def put_name(d, canonical = false, downcase = canonical)
    #  DNSSEC requires some records (e.g. NSEC, RRSIG) to be canonicalised, but
    #  not downcased. YUK!
    d = d.downcase if downcase
    put_labels(d.to_a, canonical)
  end

  def put_labels(d, do_canonical)
    if do_canonical
      d.each { |label| put_label(label) }
      @data << "\0"
      return
    end
    # Snapshot each label once. Suffix keys share these immutable strings,
    # while retaining label boundaries and case for lossless compression.
    labels = d.map { |label| label.string.b.freeze }
    d.each_index do |i|
      domain = labels[i..-1].freeze
      if (idx = @names[domain]) && idx < 0x4000
        self.put_pack('n', 0xc000 | idx)
        return
      else
        # Compression pointers have only 14 bits for the target offset.
        # Later names can still use an earlier eligible suffix.
        @names[domain] ||= @data.bytesize if @data.bytesize < 0x4000
        self.put_label(d[i])
      end
    end
    @data << "\0"
  end


  def put_label(d)
    #       s, = Name.encode(d)
    s = d
    raise EncodeError, "Label exceeds 63 octets: #{s.string.bytesize}" if s.string.bytesize > 63
    self.put_string(s.string)
  end
end
end
