# frozen_string_literal: true
#--
# accesslog.rb -- Access log handling utilities
#
# Author: IPR -- Internet Programming with Ruby -- writers
# Copyright (c) 2002 keita yamaguchi
# Copyright (c) 2002 Internet Programming with Ruby writers
#
# $IPR: accesslog.rb,v 1.1 2002/10/01 17:16:32 gotoyuzo Exp $

module WEBrick

  ##
  # AccessLog provides logging to various files in various formats.
  #
  # Multiple logs may be written to at the same time:
  #
  #   access_log = [
  #     [$stderr, WEBrick::AccessLog::COMMON_LOG_FORMAT],
  #     [$stderr, WEBrick::AccessLog::REFERER_LOG_FORMAT],
  #   ]
  #
  #   server = WEBrick::HTTPServer.new :AccessLog => access_log
  #
  # Custom log formats may be defined.  WEBrick::AccessLog provides a subset
  # of the formatting from Apache's mod_log_config
  # http://httpd.apache.org/docs/mod/mod_log_config.html#formats.  See
  # AccessLog::setup_params for a list of supported options

  module AccessLog

    ##
    # Raised if a parameter such as %e, %i, %o or %n is used without fetching
    # a specific field.

    class AccessLogError < StandardError; end

    ##
    # A configurable view of the response header fields that were actually
    # written to the transport.
    #
    # Unlike WEBrick::HTTPResponse#header (a Hash that can only hold one value
    # per field name), the view is built from
    # WEBrick::HTTPResponse#sent_header, so a field that appears more than
    # once on the wire (for example two Set-Cookie lines) is reported once per
    # occurrence.  Values of the same field are joined by +separator+, and a
    # value that contains the separator is quoted, so the boundary between two
    # occurrences stays visible in the log.
    #
    # Fields named in +masked+ are replaced by +mask+ when they are formatted.
    # Masking only affects the access log; the bytes written to the socket are
    # never changed.
    #
    # The view may be configured with the :AccessLogResponseHeaders option of
    # WEBrick::Config::HTTP, or per access log entry as the third element of
    # an :AccessLog array:
    #
    #   :AccessLog => [
    #     [$stderr, WEBrick::AccessLog::COMMON_LOG_FORMAT],
    #     [$stderr, "%W", {
    #       :fields    => %w[cache-control x-cache set-cookie],
    #       :masked    => %w[set-cookie],
    #       :mask      => "[FILTERED]",
    #       :separator => ", ",
    #     }],
    #   ]
    #
    # Once configured, the view is available to the format string through
    # <tt>%{header}o</tt> (a single selected field, joined by the separator,
    # with masked fields masked) and <tt>%W</tt> (every selected field as
    # <tt>name=value</tt> pairs).

    class ResponseHeaderView
      ##
      # Joins the repeated values of one field.  A value that itself contains
      # the separator (or a double quote or backslash) is quoted, so the
      # boundary between two occurrences of the same field stays unambiguous
      # in the log.

      DEFAULT_SEPARATOR = ", "

      ##
      # Separates the +name=value+ pairs emitted by <tt>%W</tt>.

      DEFAULT_PAIR_SEPARATOR = " "

      ##
      # Replacement for a masked field value.

      DEFAULT_MASK = "[FILTERED]"

      ##
      # Selected field names, in the order <tt>%W</tt> emits them.  When this
      # is empty every field may be read through <tt>%{header}o</tt>, but
      # <tt>%W</tt> emits nothing.

      attr_reader :fields

      ##
      # Field names whose values must not appear in the log.

      attr_reader :masked

      ##
      # Replacement text for masked values.

      attr_reader :mask

      ##
      # Boundary inserted between repeated values of the same field.

      attr_reader :separator

      ##
      # Separator inserted between the pairs emitted by <tt>%W</tt>.

      attr_reader :pair_separator

      ##
      # Builds a view from +spec+, which may be a ResponseHeaderView, an
      # Array of field names, a Hash of options, or nil (no view).

      def self.build(spec)
        case spec
        when nil
          nil
        when self
          spec
        when Hash
          options = {}
          spec.each { |key, value| options[key.to_s.downcase.to_sym] = value }
          new(**options)
        when Array
          new(:fields => spec)
        else
          new(:fields => Array(spec))
        end
      end

      def initialize(fields: [], masked: [], mask: DEFAULT_MASK,
                     separator: DEFAULT_SEPARATOR,
                     pair_separator: DEFAULT_PAIR_SEPARATOR)
        @fields = normalize_field_names(fields)
        @masked = normalize_field_names(masked)
        @mask = mask.to_s
        @separator = separator.to_s
        @pair_separator = pair_separator.to_s
      end

      ##
      # May +name+ be read through this view?  With no selection every field
      # may be read.

      def field?(name)
        @fields.empty? || @fields.include?(normalize_name(name))
      end

      ##
      # Must the value of +name+ be masked in the log?

      def masked?(name)
        @masked.include?(normalize_name(name))
      end

      ##
      # Every value of +name+ that was written to the transport, in wire
      # order.  An empty Array means the field was not written.
      #
      # When the response was never handed to a transport the header Hash is
      # used instead, which is what handlers that write the response
      # themselves (for example mod_ruby) rely on.

      def values(response, name)
        name = normalize_name(name)
        if pairs = sent_pairs(response)
          pairs.each_with_object([]) { |(field, value), result|
            result << value.to_s if normalize_name(field) == name
          }
        else
          value = header_value(response, name)
          value.nil? ? [] : [value]
        end
      end

      ##
      # Formats every value of +name+ as it should appear in the log, or nil
      # when the field was not written.  A masked field is replaced once per
      # occurrence, so the number of occurrences stays visible.

      def value(response, name)
        values = self.values(response, name)
        return nil if values.empty?
        values = values.map { @mask } if masked?(name)
        join_values(values)
      end

      ##
      # Like #value, but nil for a field this view does not select.  This is
      # what a log format string reads, so the log exposes only the fields the
      # view allows.

      def selected_value(response, name)
        return nil unless field?(name)
        value(response, name)
      end

      ##
      # Formats all selected fields as <tt>name=value</tt> pairs.

      def to_log_string(response)
        return "" if @fields.empty?
        @fields.map { |name|
          value = self.value(response, name)
          "#{name}=#{value.nil? ? "-" : value}"
        }.join(@pair_separator)
      end

      private

      def normalize_name(name)
        name.to_s.downcase
      end

      def normalize_field_names(names)
        Array(names).map { |name| normalize_name(name) }.uniq
      end

      ##
      # Joins repeated values, quoting a value that contains the separator so
      # the boundary cannot be confused with a separator inside a value.  A
      # lone value is quoted too when it contains the separator, so the log
      # never looks as if the field was written twice.

      def join_values(values)
        values.map { |value| quote_value(value) }.join(@separator)
      end

      def quote_value(value)
        value = value.to_s
        if value.include?(@separator) || value.include?('"') || value.include?('\\')
          '"' + value.gsub(/[\\"]/) { |char| "\\#{char}" } + '"'
        else
          value
        end
      end

      ##
      # The [name, value] pairs written to the transport.  Returns nil when
      # the response was never handed to a transport, so callers can fall
      # back to the header Hash.

      def sent_pairs(response)
        return nil unless response.respond_to?(:sent_header)
        if response.respond_to?(:header_write_state) &&
           response.header_write_state == :unsent
          return nil
        end
        response.sent_header
      end

      def header_value(response, name)
        return nil unless response.respond_to?(:header)
        header = response.header
        return nil unless header
        header.each { |key, value|
          return value.to_s if normalize_name(key) == name
        }
        nil
      end
    end

    ##
    # The Common Log Format's time format

    CLF_TIME_FORMAT     = "[%d/%b/%Y:%H:%M:%S %Z]"

    ##
    # Common Log Format

    COMMON_LOG_FORMAT   = "%h %l %u %t \"%r\" %s %b"

    ##
    # Short alias for Common Log Format

    CLF                 = COMMON_LOG_FORMAT

    ##
    # Referer Log Format

    REFERER_LOG_FORMAT  = "%{Referer}i -> %U"

    ##
    # User-Agent Log Format

    AGENT_LOG_FORMAT    = "%{User-Agent}i"

    ##
    # Combined Log Format

    COMBINED_LOG_FORMAT = "#{CLF} \"%{Referer}i\" \"%{User-agent}i\""

    ##
    # The view used when no response header view is configured.  It selects
    # nothing and masks nothing, so <tt>%{header}o</tt> still reads every
    # field from the header snapshot while <tt>%W</tt> emits nothing.

    DEFAULT_HEADER_VIEW = ResponseHeaderView.new.freeze

    module_function

    # This format specification is a subset of mod_log_config of Apache:
    #
    # %a:: Remote IP address
    # %b:: Total response size
    # %e{variable}:: Given variable in ENV
    # %f:: Response filename
    # %h:: Remote host name
    # %{header}i:: Given request header
    # %l:: Remote logname, always "-"
    # %m:: Request method
    # %{attr}n:: Given request attribute from <tt>req.attributes</tt>
    # %{header}o:: Given response header, from the header as written to the
    #              transport (repeated values are kept)
    # %p:: Server's request port
    # %{format}p:: The canonical port of the server serving the request or the
    #              actual port or the client's actual port.  Valid formats are
    #              canonical, local or remote.
    # %q:: Request query string
    # %r:: First line of the request
    # %s:: Request status; "-" when the header write failed before the status
    #     line was confirmed
    # %t:: Time the request was received
    # %T:: Time taken to process the request
    # %u:: Remote user from auth
    # %U:: Unparsed URI
    # %W:: All selected response header fields, as <tt>name=value</tt> pairs
    # %x:: How much of the response header reached the transport:
    #     <tt>unsent</tt>, <tt>partial</tt> or <tt>complete</tt>
    # %{bytes}x:: Number of response header bytes confirmed written
    # %{size}x:: Size of the response header block handed to the transport
    # %{status}x:: Status code carried by a status line that was written, or "-"
    # %%:: Literal %
    #
    # %{header}o reads the response header as it was handed to the transport,
    # so a field written more than once is reported once per occurrence; see
    # ResponseHeaderView.

    def setup_params(config, req, res)
      params = Hash.new("")
      params["a"] = req.peeraddr[3]
      params["b"] = res.sent_size
      params["e"] = ENV
      params["f"] = res.filename || ""
      params["h"] = req.peeraddr[2]
      params["i"] = req
      params["l"] = "-"
      params["m"] = req.request_method
      params["n"] = req.attributes
      params["o"] = res
      params["p"] = req.port
      params["q"] = req.query_string
      params["r"] = req.request_line.sub(/\x0d?\x0a\z/o, '')
      params["s"] = response_status(res)
      params["t"] = req.request_time
      params["T"] = Time.now - req.request_time
      params["u"] = req.user || "-"
      params["U"] = req.unparsed_uri
      params["v"] = config[:ServerName]
      params["W"] = response_header_view(config, res)
      params
    end

    ##
    # Builds the ResponseHeaderView for +res+.
    #
    # The :AccessLogResponseHeaders option of +config+ wins when it is set; the
    # view is also stored on the response so a servlet can inspect it.  When
    # the option is not set the response's own #response_header_view is used,
    # so a servlet may configure the view for just its own responses.  With
    # neither, no view is used and the historical behavior applies.

    def response_header_view(config, res)
      spec = config[:AccessLogResponseHeaders] if config
      if spec.nil?
        return res.response_header_view if res.respond_to?(:response_header_view)
        return nil
      end
      build_response_header_view(spec, res)
    end

    ##
    # Builds a view from +spec+ and stores it on +res+.  Used both for the
    # server-wide option and for a per-log-entry view.

    def build_response_header_view(spec, res)
      view = ResponseHeaderView.build(spec)
      if view && res.respond_to?(:response_header_view=)
        res.response_header_view = view
      end
      view
    end

    ##
    # The status to log.
    #
    # Once a status line was confirmed written this is that status, so the log
    # agrees with the wire even if the response object changed afterwards.
    # When the header write failed before the status line was confirmed, "-"
    # is reported instead of the status the response would have sent, so a
    # failed write cannot look like a delivered response.  When nothing has
    # been sent yet (for example a handler that writes the response itself)
    # this is the status the response would send; pair it with <tt>%x</tt> to
    # tell the two apart.

    def response_status(res)
      if res.respond_to?(:sent_status)
        return res.sent_status if res.sent_status
        if res.respond_to?(:header_write_state) &&
           res.header_write_state == :partial
          return "-"
        end
      end
      res.status
    end

    ##
    # Formats +params+ according to +format_string+ which is described in
    # setup_params.

    def format(format_string, params)
      format_string.gsub(/\%(?:\{(.*?)\})?>?([a-zA-Z%])/){
         param, spec = $1, $2
         case spec[0]
         when ?e, ?i, ?n
           raise AccessLogError,
             "parameter is required for \"#{spec}\"" unless param
           (value = params[spec][param]) ? escape(value) : "-"
         when ?o
           raise AccessLogError,
             "parameter is required for \"#{spec}\"" unless param
           value = format_response_header(params, param)
           value ? escape(value) : "-"
         when ?W
           escape(format_response_headers(params))
         when ?x
           format_header_write(params, param)
         when ?t
           params[spec].strftime(param || CLF_TIME_FORMAT)
         when ?p
           case param
           when 'remote'
             escape(params["i"].peeraddr[1].to_s)
           else
             escape(params["p"].to_s)
           end
         when ?%
           "%"
         else
           escape(params[spec].to_s)
         end
      }
    end

    ##
    # Reads one response header field for <tt>%{header}o</tt>.
    #
    # The field comes from the header as it was handed to the transport
    # (HTTPResponse#sent_header), so a field written more than once is reported
    # once per occurrence, joined by the view's separator, and a value that
    # contains the separator is quoted so the boundary stays readable.
    #
    # With a view, a field outside its selection is reported as nil (which the
    # caller renders as "-") and a masked field is replaced by the mask, so
    # masking applies no matter which specifier is used.

    def format_response_header(params, name)
      response = params["o"]
      view = response_header_view_of(params)
      view.selected_value(response, name)
    end

    ##
    # Formats every selected response header field as <tt>name=value</tt>
    # pairs.

    def format_response_headers(params)
      view = response_header_view_of(params)
      view.to_log_string(params["o"])
    end

    ##
    # The view used to format the response header fields in this log line.
    # The view built by setup_params wins; when there is none the permissive
    # default is used, which reads every field from the header snapshot and
    # masks nothing.

    def response_header_view_of(params)
      view = params["W"]
      view.is_a?(ResponseHeaderView) ? view : DEFAULT_HEADER_VIEW
    end

    ##
    # Reports how much of the response header reached the transport, which
    # tells a failed request apart from one whose header was fully written.
    #
    # %x:: <tt>unsent</tt>, <tt>partial</tt> or <tt>complete</tt>
    # %{bytes}x:: Number of header bytes confirmed written
    # %{size}x:: Size of the header block handed to the transport
    # %{status}x:: Status carried by a written status line, or "-"

    def format_header_write(params, param)
      response = params["o"]
      return "-" unless response.respond_to?(:header_write_state)
      case param && param.downcase
      when 'bytes'
        escape(response.header_bytes_written.to_s)
      when 'size'
        escape(response.sent_header_size.to_s)
      when 'status'
        status = response.sent_status
        status.nil? ? "-" : escape(status.to_s)
      else
        escape(response.header_write_state.to_s)
      end
    end

    ##
    # Escapes control characters in +data+

    def escape(data)
      data = data.gsub(/[[:cntrl:]\\]+/) {$&.dump[1...-1]}
      data.untaint if RUBY_VERSION < '2.7'
      data
    end
  end
end
