# frozen_string_literal: true

module Mail
  # = Encoding Plan
  #
  # A read-only preflight check that compares a message against the set of
  # SMTP extension capabilities offered by a target server (for example the
  # +8BITMIME+, +BINARYMIME+ and +SMTPUTF8+ keywords from an EHLO reply)
  # and works out, before anything is delivered:
  #
  # * which capabilities the message needs to be delivered as-is,
  # * which parts can be losslessly re-encoded to fit the offered
  #   capabilities (headers as RFC 2047 encoded-words, bodies as
  #   quoted-printable or base64),
  # * and which parts block delivery entirely because they cannot be
  #   downgraded (for example a non-ASCII envelope address: an
  #   internationalized local part must never be rewritten into some
  #   other address, so it hard-requires +SMTPUTF8+).
  #
  # The plan never modifies the message it was built from.  Applying the
  # plan returns a new message with the planned re-encodings carried out;
  # encoding that result is consistent with the preflight conclusion.
  #
  #   plan = Mail::EncodingPlan.new(mail, capabilities: %w[8BITMIME])
  #   plan.deliverable?          # => true or false
  #   plan.blockers              # => items that prevent delivery
  #   plan.adjustments           # => items that will be losslessly re-encoded
  #   plan.required_capabilities # => capabilities needed to send as-is
  #   plan.apply                 # => new Mail::Message with adjustments applied
  class EncodingPlan
    # SMTP extension capabilities the plan understands.
    KNOWN_CAPABILITIES = %w[8BITMIME BINARYMIME SMTPUTF8].freeze

    # RFC 5321 4.5.3.1.6 maximum line length, excluding the CRLF.
    MAX_LINE_LENGTH = 998

    # Raised by #apply when the plan found blockers.
    class UnsatisfiableError < StandardError
      attr_reader :blockers

      def initialize(blockers)
        @blockers = blockers
        super "message cannot be delivered with the offered capabilities: " +
              blockers.map(&:detail).join('; ')
      end
    end

    # A single finding of the plan.
    #
    # [scope]      one of :envelope, :header or :body
    # [status]     one of :ok (deliverable as-is, capability permitting),
    #              :reencode (will be losslessly re-encoded) or
    #              :blocked (cannot be delivered with the offered capabilities)
    # [capability] the SMTP capability needed to deliver this item unmodified,
    #              or nil when no capability could help
    # [subject]    what the item is about: an address, a field name or a
    #              description of a MIME part
    # [detail]     human readable explanation
    # [encoding]   for :reencode body items, the transfer encoding that will
    #              be applied
    # [part]       for body items, the Mail::Message/Part the item refers to
    Item = Struct.new(:scope, :status, :capability, :subject, :detail, :encoding, :part, keyword_init: true) do
      def ok?
        status == :ok
      end

      def reencode?
        status == :reencode
      end

      def blocked?
        status == :blocked
      end
    end

    attr_reader :message, :capabilities, :items

    def initialize(message, capabilities: [])
      @message = message
      @capabilities = Array(capabilities).map { |c| c.to_s.upcase }.uniq.freeze
      @items = []
      analyze_envelope
      analyze_headers
      analyze_bodies
      @items.freeze
    end

    # True if the server advertised the given capability.
    def offers?(capability)
      capabilities.include?(capability.to_s.upcase)
    end

    # True when nothing blocks delivery: every item either fits the offered
    # capabilities as-is or can be losslessly re-encoded.
    def deliverable?
      blockers.empty?
    end

    # Items that cannot be delivered with the offered capabilities and
    # cannot be losslessly re-encoded either.
    def blockers
      items.select(&:blocked?)
    end

    # Items that will be losslessly re-encoded when the plan is applied.
    def adjustments
      items.select(&:reencode?)
    end

    # Capabilities needed to deliver the message exactly as-is.
    def required_capabilities
      items.map(&:capability).compact.uniq
    end

    # Required capabilities the server did not offer.  Missing capabilities
    # are not necessarily blockers: re-encoding may compensate for them.
    def missing_capabilities
      required_capabilities - capabilities
    end

    # Returns a new Mail::Message with all planned re-encodings applied.
    # The original message is left unchanged.  Raises UnsatisfiableError
    # when the plan has blockers.
    def apply
      raise UnsatisfiableError, blockers unless deliverable?

      copy = deep_copy_message(message)
      each_leaf(copy) do |leaf|
        item = body_item_for(leaf)
        reencode_part!(leaf, item.encoding) if item && item.reencode? && item.encoding
      end
      copy
    end

    private

    def analyze_envelope
      envelope_addresses.each do |address|
        next if address.to_s.ascii_only?

        if offers?('SMTPUTF8')
          add_item :envelope, :ok, 'SMTPUTF8', address,
                   'non-ASCII envelope address requires the SMTPUTF8 capability'
        else
          add_item :envelope, :blocked, 'SMTPUTF8', address,
                   'non-ASCII envelope address requires the SMTPUTF8 capability; ' \
                   'an internationalized address cannot be rewritten to another address'
        end
      end
    end

    def envelope_addresses
      addresses = []
      from = message.smtp_envelope_from
      addresses << from unless Utilities.blank?(from)
      addresses.concat Array(message.smtp_envelope_to)
      addresses.compact.map(&:to_s).reject { |addr| Utilities.blank?(addr) }
    end

    def analyze_headers
      each_message(message) do |part|
        where = part.equal?(message) ? 'message header' : 'MIME part header'
        part.header.fields.each do |field|
          analyze_field(field, where)
        end
      end
    end

    def analyze_field(field, where)
      encoded =
        begin
          field.encoded
        rescue StandardError
          nil
        end

      raw_value =
        begin
          field.value.to_s
        rescue StandardError
          ''
        end

      subject = "#{where} #{field.name}"

      if encoded && encoded.ascii_only?
        unless raw_value.ascii_only?
          if offers?('SMTPUTF8')
            add_item :header, :ok, 'SMTPUTF8', subject,
                     "non-ASCII header field #{field.name} is covered by the " \
                     'SMTPUTF8 capability'
          else
            add_item :header, :reencode, 'SMTPUTF8', subject,
                     "non-ASCII header field #{field.name} will be losslessly " \
                     're-encoded (RFC 2047 encoded-words / RFC 2231 parameters)'
          end
        end
      elsif offers?('SMTPUTF8')
        add_item :header, :ok, 'SMTPUTF8', subject,
                 "header field #{field.name} contains non-ASCII text that " \
                 'cannot be re-encoded and requires the SMTPUTF8 capability'
      else
        add_item :header, :blocked, 'SMTPUTF8', subject,
                 "header field #{field.name} contains non-ASCII text that " \
                 'cannot be losslessly re-encoded and requires the SMTPUTF8 capability'
      end
    end

    def analyze_bodies
      each_leaf(message) do |leaf|
        item = body_item_for(leaf)
        @items << item if item
      end
    end

    def body_item_for(part)
      body = part.body
      return nil if body.nil? || body.multipart?

      raw = body.raw_source.to_s
      return nil if transport_clean?(raw)

      subject = body_subject_for(part)
      encoding = current_transfer_encoding(part, body)

      unless Mail::Encodings.defined?(encoding)
        return Item.new(scope: :body, status: :blocked, subject: subject, part: part,
                        detail: "cannot losslessly re-encode unknown " \
                                "content-transfer-encoding #{encoding.inspect}")
      end

      content =
        begin
          body.decoded
        rescue StandardError
          nil
        end

      if content.nil?
        return Item.new(scope: :body, status: :blocked, subject: subject, part: part,
                        detail: "cannot decode body with content-transfer-encoding " \
                                "#{encoding.inspect} to re-encode it losslessly")
      end

      content = content.b

      if content.include?("\x00".b)
        capability_item(part, subject, 'BINARYMIME', 'base64', 'binary')
      elsif !content.ascii_only?
        capability_item(part, subject, '8BITMIME', default_reencoding_for(part), '8bit')
      elsif long_lines?(content)
        if text_part?(part)
          Item.new(scope: :body, status: :reencode, subject: subject, part: part,
                   encoding: 'quoted-printable',
                   detail: "body lines exceed #{MAX_LINE_LENGTH} octets; will be " \
                           'losslessly re-encoded as quoted-printable')
        else
          capability_item(part, subject, 'BINARYMIME', 'base64', 'binary')
        end
      else
        # The content itself is clean 7bit; only its transport form needs
        # to be normalized (for example unwrapped base64 or bare line feeds).
        Item.new(scope: :body, status: :reencode, subject: subject, part: part,
                 encoding: encoding,
                 detail: "body will be losslessly re-encoded as #{encoding} to " \
                         'normalize its transport form')
      end
    end

    def capability_item(part, subject, capability, target, kind)
      if offers?(capability)
        Item.new(scope: :body, status: :ok, capability: capability, subject: subject,
                 part: part,
                 detail: "#{kind} content requires the #{capability} capability")
      else
        Item.new(scope: :body, status: :reencode, capability: capability,
                 subject: subject, part: part, encoding: target,
                 detail: "#{kind} content requires the #{capability} capability, " \
                         "which is not offered; will be losslessly re-encoded as #{target}")
      end
    end

    def current_transfer_encoding(part, body)
      encoding =
        begin
          part.content_transfer_encoding if part.respond_to?(:content_transfer_encoding)
        rescue StandardError
          nil
        end
      encoding = body.encoding if Utilities.blank?(encoding)
      encoding = encoding.to_s.downcase
      encoding.empty? ? '7bit' : encoding
    end

    def long_lines?(content)
      content.each_line.any? do |line|
        line.sub(/\r?\n\z/, '').bytesize > MAX_LINE_LENGTH
      end
    end

    def transport_clean?(raw)
      raw = raw.b
      raw.ascii_only? && !raw.include?("\x00".b) && !long_lines?(raw)
    end

    def text_part?(part)
      type = part.mime_type
      type.nil? || type.downcase.start_with?('text/')
    end

    def default_reencoding_for(part)
      text_part?(part) ? 'quoted-printable' : 'base64'
    end

    def body_subject_for(part)
      return 'message body' if part.equal?(message)

      type = part.mime_type || 'application/octet-stream'
      name =
        begin
          part.filename if part.respond_to?(:filename)
        rescue StandardError
          nil
        end
      name ? "MIME part #{type} (#{name})" : "MIME part #{type}"
    end

    def add_item(scope, status, capability, subject, detail)
      @items << Item.new(scope: scope, status: status, capability: capability,
                         subject: subject, detail: detail)
    end

    # Yields the message and, recursively, every part that has its own header.
    def each_message(part, &block)
      yield part
      part.parts.each { |child| each_message(child, &block) } if part.multipart?
    end

    # Yields the message and, recursively, every non-multipart part whose
    # body is actually transferred over the wire.
    def each_leaf(part, &block)
      if part.multipart?
        part.parts.each { |child| each_leaf(child, &block) }
      else
        yield part
      end
    end

    # Copies a message deeply enough that applying re-encodings to the copy
    # cannot leak into the original: Message#dup already deep-copies the
    # header fields, so only the body chain needs to be duplicated.
    def deep_copy_message(part)
      part.body # force lazy parsing so @body_raw is settled before copying
      copy = part.dup
      copy.instance_variable_set(:@body_raw, nil)
      copy.instance_variable_set(:@body, deep_copy_body(part.body))
      copy
    end

    def deep_copy_body(body)
      copy = body.dup
      copy.instance_variable_set(:@parts, Mail::PartsList.new(body.parts.map { |p| deep_copy_message(p) }))
      copy
    end

    def reencode_part!(part, target)
      body = part.body
      encoder = Mail::Encodings.get_encoding(target)
      new_body = body.dup
      new_body.instance_variable_set(:@raw_source, encoder.encode(body.decoded))
      new_body.encoding = target
      part.instance_variable_set(:@body, new_body)
      part.content_transfer_encoding = target
    end
  end
end
