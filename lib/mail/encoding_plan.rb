# encoding: utf-8
# frozen_string_literal: true

module Mail
  # A read-only preflight check that analyzes whether a message can be
  # delivered to a server advertising a given set of SMTP extensions
  # (capabilities), and which lossless re-encodings would make it fit.
  #
  # The plan inspects four areas:
  #
  # * Envelope addresses (smtp_envelope_from / smtp_envelope_to)
  # * Header fields (unstructured text and address fields)
  # * Body content-transfer-encoding
  # * Line lengths against the SMTP 998-octet limit
  #
  # Internationalized local parts are never rewritten into a different
  # address: they require SMTPUTF8 and are reported as blockers when the
  # server does not offer it. Internationalized domains in envelope
  # addresses can be losslessly re-encoded with IDNA/punycode. Non-ASCII
  # header text is losslessly re-encoded as RFC 2047 encoded-words, and
  # 8bit/binary bodies as quoted-printable or base64.
  #
  # Example:
  #
  #  plan = Mail::EncodingPlan.new(mail, capabilities: %i[8bitmime])
  #  plan.compatible?            #=> true if deliverable (after re-encodings)
  #  plan.required_capabilities  #=> capabilities the server must still offer
  #  plan.reencodings            #=> lossless re-encodings that will be applied
  #  plan.blockers               #=> items that cannot be degraded
  #  plan.apply                  #=> a new, re-encoded Mail::Message
  #
  # No network probing is performed; the capability set is supplied by
  # the caller.
  class EncodingPlan
    # Raised by EncodingPlan#apply when the message cannot be degraded to
    # fit the server's capabilities.
    class IncompatibleError < StandardError
      attr_reader :blockers

      def initialize(blockers)
        @blockers = blockers
        super("Message cannot be delivered with the available capabilities: " \
              "#{blockers.map(&:detail).join('; ')}")
      end
    end

    # An item that prevents delivery with the given capabilities and
    # cannot be losslessly re-encoded away.
    Blocker = Struct.new(:source, :reason, :detail)

    # A lossless re-encoding the plan will perform so the message fits
    # the given capabilities.
    Reencoding = Struct.new(:source, :from, :to, :detail)

    # RFC 5321 4.5.3.1.6: a line of SMTP content may not exceed 998
    # octets excluding the CRLF.
    SMTP_LINE_LIMIT = 998

    CAPABILITY_ALIASES = {
      'smtputf8'       => :smtputf8,
      '8bitmime'       => :eight_bit_mime,
      'eight_bit_mime' => :eight_bit_mime,
      'binarymime'     => :binarymime
    }.freeze

    KNOWN_CAPABILITIES = CAPABILITY_ALIASES.values.freeze

    attr_reader :message, :capabilities, :required_capabilities, :reencodings, :blockers

    def initialize(message, capabilities: [])
      @message = message
      @capabilities = Array(capabilities).map { |cap| normalize_capability(cap) }.uniq
      @required_capabilities = []
      @reencodings = []
      @blockers = []
      analyze
    end

    # True when the message can be delivered to a server offering the
    # given capabilities, applying the listed lossless re-encodings if
    # needed.
    def compatible?
      blockers.empty? && missing_capabilities.empty?
    end

    # Capabilities the message still needs after all lossless
    # re-encodings, but the server does not offer.
    def missing_capabilities
      required_capabilities - capabilities
    end

    # Returns a new Mail::Message re-encoded to fit the capabilities.
    # The receiver's message is left unchanged. Raises IncompatibleError
    # when the plan has blockers or missing capabilities.
    def apply
      raise IncompatibleError, blockers unless compatible?

      copy = deep_copy_message(message)
      copy.transport_encoding = transport_encoding_name
      rewrite_envelope(copy)
      copy
    end

    private

    def analyze
      analyze_envelope
      analyze_headers
      analyze_part(message, 'body')
    end

    def normalize_capability(cap)
      CAPABILITY_ALIASES.fetch(cap.to_s.downcase) do
        raise ArgumentError, "Unknown SMTP capability: #{cap.inspect}. " \
                             "Known capabilities: #{KNOWN_CAPABILITIES.join(', ')}"
      end
    end

    def require_capability(cap)
      @required_capabilities << cap unless @required_capabilities.include?(cap)
    end

    def smtputf8?
      capabilities.include?(:smtputf8)
    end

    # The best transfer encoding the server can transport directly.
    def transport_encoding_name
      if capabilities.include?(:binarymime)
        'binary'
      elsif capabilities.include?(:eight_bit_mime)
        '8bit'
      else
        '7bit'
      end
    end

    def transport_encoding_class
      Encodings.get_encoding(transport_encoding_name)
    end

    # -- Envelope -----------------------------------------------------

    def analyze_envelope
      envelope_addresses.each do |source, address|
        next if address.nil? || address.ascii_only?

        local, domain = address_parts(address)

        if local && !local.ascii_only?
          require_capability :smtputf8
          unless smtputf8?
            blockers << Blocker.new(source, :internationalized_local_part,
              "#{source} address #{address.inspect} has a non-ASCII local part " \
              "which requires SMTPUTF8; it cannot be rewritten into another address")
          end
        end

        if domain && !domain.ascii_only?
          if smtputf8?
            require_capability :smtputf8
          else
            reencodings << Reencoding.new(source, domain, Punycode.encode_domain(domain),
              "#{source} address #{address.inspect} has a non-ASCII domain which " \
              "will be losslessly re-encoded with IDNA (punycode)")
          end
        end
      end
    end

    def envelope_addresses
      addresses = []
      from = message.smtp_envelope_from
      addresses << [:envelope_from, from.to_s] unless Utilities.blank?(from)
      Array(message.smtp_envelope_to).each do |to|
        addresses << [:envelope_to, to.to_s]
      end
      addresses
    end

    # Splits an address into [local, domain] without modifying it.
    def address_parts(address)
      parsed = Mail::Address.new(address)
      [parsed.local, parsed.domain]
    rescue StandardError
      local, _, domain = address.to_s.rpartition('@')
      local = address.to_s if domain.empty?
      [local, domain.empty? ? nil : domain]
    end

    # -- Headers ------------------------------------------------------

    def analyze_headers
      message.header.fields.each do |field|
        value = safe_field_value(field)
        next if value.nil? || value.ascii_only?

        if field.field.is_a?(Mail::CommonAddressField)
          analyze_address_field(field)
        else
          reencodings << Reencoding.new("header #{field.name}", 'utf-8', 'rfc2047',
            "header #{field.name.inspect} contains non-ASCII text which will be " \
            "losslessly encoded for transport (RFC 2047 encoded-words / RFC 2231 parameters)")
        end
      end
    end

    def analyze_address_field(field)
      source = "header #{field.name}"
      field.addrs.each do |address|
        if address.display_name && !address.display_name.ascii_only?
          reencodings << Reencoding.new(source, 'utf-8', 'rfc2047',
            "#{source} display name #{address.display_name.inspect} will be " \
            "losslessly encoded as RFC 2047 encoded-words")
        end

        if address.local && !address.local.ascii_only?
          require_capability :smtputf8
          unless smtputf8?
            blockers << Blocker.new(source, :internationalized_local_part,
              "#{source} address #{address.address.inspect} has a non-ASCII local part " \
              "which requires SMTPUTF8; it cannot be rewritten into another address " \
              "or expressed as an encoded-word")
          end
        end

        if address.domain && !address.domain.ascii_only?
          # Header fields are message content (and may be covered by
          # signatures), so unlike envelope domains they are not
          # rewritten to their IDNA form.
          require_capability :smtputf8
          unless smtputf8?
            blockers << Blocker.new(source, :internationalized_domain,
              "#{source} address #{address.address.inspect} has a non-ASCII domain " \
              "which requires SMTPUTF8 in message headers")
          end
        end
      end
    rescue StandardError
      blockers << Blocker.new(source, :unparseable_address_field,
        "#{source} contains non-ASCII addresses that could not be parsed")
    end

    def safe_field_value(field)
      field.value.to_s
    rescue StandardError
      nil
    end

    # -- Body ---------------------------------------------------------

    def analyze_part(part, source)
      if part.multipart?
        part.parts.each_with_index do |subpart, index|
          analyze_part(subpart, "#{source} part #{index + 1}")
        end
      else
        analyze_leaf(part, source)
      end
    end

    def analyze_leaf(part, source)
      body = part.body
      source_encoding = Encodings.get_encoding(body.encoding)

      unless source_encoding
        blockers << Blocker.new(source, :unknown_transfer_encoding,
          "#{source} uses unknown content-transfer-encoding #{body.encoding.inspect} " \
          "and cannot be analyzed")
        return
      end

      predicted = Encodings::TransferEncoding.negotiate(
        transport_encoding_class, source_encoding, body.raw_source,
        part.send(:allowed_encodings))

      if predicted.nil?
        blockers << Blocker.new(source, :no_lossless_encoding,
          "#{source} (#{part.mime_type || 'text/plain'}, #{source_encoding}) cannot be " \
          "losslessly re-encoded for a server without " \
          "#{capabilities.include?(:binarymime) ? 'BINARYMIME' : '8BITMIME/BINARYMIME'}")
        return
      end

      case predicted.to_s
      when '8bit'
        require_capability :eight_bit_mime
      when 'binary'
        require_capability :binarymime
      end

      if predicted != source_encoding
        reencodings << Reencoding.new(source, source_encoding.to_s, predicted.to_s,
          "#{source} will be losslessly re-encoded from #{source_encoding} to #{predicted}")
      end
    end

    # -- Apply --------------------------------------------------------

    def deep_copy_message(part)
      copy = part.dup
      copy.instance_variable_set(:@body, deep_copy_body(part.body))
      copy
    end

    def deep_copy_body(body)
      new_body = Mail::Body.new(body.raw_source.to_s.dup)
      new_body.encoding = body.encoding
      if body.multipart?
        if body.boundary && body.raw_source.to_s.include?("--#{body.boundary}")
          # Re-parse the raw text so the copy gets its own part objects.
          new_body.split!(body.boundary)
        else
          body.parts.each { |subpart| new_body << deep_copy_message(subpart) }
          new_body.boundary = body.boundary
        end
      end
      new_body
    end

    def rewrite_envelope(copy)
      from = copy.smtp_envelope_from
      if !Utilities.blank?(from) && (rewritten = rewrite_address(from.to_s)) != from.to_s
        copy.smtp_envelope_from = rewritten
      end

      tos = Array(copy.smtp_envelope_to).map(&:to_s)
      rewritten_tos = tos.map { |to| rewrite_address(to) }
      copy.smtp_envelope_to = rewritten_tos if rewritten_tos != tos
    end

    # Re-encodes a non-ASCII domain with IDNA. Never touches the local
    # part: an internationalized local part is not another address and
    # must not be rewritten into one.
    def rewrite_address(address)
      local, domain = address_parts(address)
      return address unless domain && !domain.ascii_only?
      return address unless local && local.ascii_only?

      spec = "#{local}@#{Punycode.encode_domain(domain)}"
      parsed = begin
        Mail::Address.new(address)
      rescue StandardError
        nil
      end
      if parsed && parsed.display_name
        "#{parsed.display_name} <#{spec}>"
      else
        spec
      end
    end

    # Minimal RFC 3492 punycode encoder used to losslessly re-encode
    # internationalized envelope domains (IDNA ToASCII for labels that
    # contain non-ASCII characters).
    module Punycode #:nodoc:
      BASE         = 36
      TMIN         = 1
      TMAX         = 26
      SKEW         = 38
      DAMP         = 700
      INITIAL_BIAS = 72
      INITIAL_N    = 128
      DELIMITER    = '-'

      module_function

      # Encodes a domain name to its IDNA ASCII form. Labels that are
      # already ASCII are passed through unchanged.
      def encode_domain(domain)
        domain.to_s.split('.', -1).map do |label|
          label.ascii_only? ? label : "xn--#{encode(label)}"
        end.join('.')
      end

      def encode(input)
        codepoints = input.codepoints
        output = +''

        basic = codepoints.select { |cp| cp < 0x80 }
        output << basic.pack('C*')
        output << DELIMITER unless basic.empty?

        n = INITIAL_N
        delta = 0
        bias = INITIAL_BIAS
        handled = basic.length

        while handled < codepoints.length
          min = codepoints.select { |cp| cp >= n }.min
          delta += (min - n) * (handled + 1)
          n = min

          codepoints.each do |cp|
            delta += 1 if cp < n
            next unless cp == n

            q = delta
            k = BASE
            loop do
              t = k <= bias ? TMIN : (k >= bias + TMAX ? TMAX : k - bias)
              break if q < t
              output << encode_digit(t + (q - t) % (BASE - t))
              q = (q - t) / (BASE - t)
              k += BASE
            end
            output << encode_digit(q)
            bias = adapt(delta, handled + 1, handled == basic.length)
            delta = 0
            handled += 1
          end

          delta += 1
          n += 1
        end

        output
      end

      def adapt(delta, num_points, first_time)
        delta = first_time ? delta / DAMP : delta / 2
        delta += delta / num_points

        k = 0
        while delta > (BASE - TMIN) * TMAX / 2
          delta /= BASE - TMIN
          k += BASE
        end
        k + (BASE - TMIN + 1) * delta / (delta + SKEW)
      end

      def encode_digit(digit)
        (digit < 26 ? 97 + digit : 22 + digit).chr
      end
    end
    private_constant :Punycode
  end
end
