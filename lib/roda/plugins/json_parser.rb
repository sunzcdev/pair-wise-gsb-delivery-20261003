# frozen-string-literal: true

require 'stringio'

class Roda
  module RodaPlugins
    # The json_parser plugin parses request bodies in JSON format
    # if the request's content type specifies json. This is mostly
    # designed for use with JSON API sites.
    #
    # This only parses the request body as JSON if the Content-Type
    # header for the request indicates it is json, as determined by
    # the +content_type_regexp+ plugin keyword argument.
    #
    # The parsed JSON body will be available in +r.POST+, just as a
    # parsed HTML form body would be. It will also be available in
    # +r.params+ (which merges +r.GET+ with +r.POST+).
    #
    # == Media type specific parsers
    #
    # By default, all JSON media types are parsed by the same parser.  If
    # different media types require different parsing rules, use the
    # +:parsers+ plugin keyword argument, which maps media types to parsers:
    #
    #   plugin :json_parser,
    #     :content_type_regexp=>/\Aapplication\/(?:vnd\.[a-z]+\+)?json\b/i,
    #     :parsers=>{
    #       'application/json'=>JSON.method(:parse),
    #       'application/vnd.acme+json'=>method(:parse_acme_json),
    #       'application/vnd.other+json'=>method(:parse_other_json)
    #     }
    #
    # Media types are matched case insensitively, and media type parameters
    # (such as <tt>charset</tt>) are ignored when matching.  So a request
    # with the following header:
    #
    #   Content-Type: Application/VND.Acme+JSON; charset=utf-8
    #
    # matches the <tt>application/vnd.acme+json</tt> parser.
    #
    # If the request has a JSON media type (as determined by the
    # +content_type_regexp+ option), but no parser is declared for the
    # media type, accessing +r.POST+ (or +r.params+) raises
    # Roda::RodaPlugins::JsonParser::UnsupportedMediaType.  This is
    # distinct from a parser raising an exception while decoding the
    # body, which is handled by the +:error_handler+ option.
    #
    # == Request body caching and introspection
    #
    # The request body is read at most once per request.  The body is
    # cached in the environment, and <tt>env['rack.input']</tt> is replaced
    # with a rewindable StringIO containing the cached body, so the body
    # can be read again without accessing the original stream.  Parsed
    # results are cached per application class, so a subapplication that
    # uses different +:parsers+ will parse the cached body using its own
    # parser, without reading the original stream again.
    #
    # The following request methods describe how the current application
    # handled (or would handle) the request body:
    #
    # json_parser_media_type :: The normalized media type of the request,
    #                           or nil if there is no Content-Type header.
    # json_parser_result :: The outcome of parsing: +:none+ (not a JSON media
    #                       type this application parses), +:empty+, +:parsed+,
    #                       +:invalid+, or +:unsupported_media_type+.  Returns
    #                       nil if the application has not attempted to parse
    #                       the request body.
    # json_parser_error :: The exception raised by the parser if the result
    #                      is +:invalid+, or nil otherwise.
    #
    # The parser that would be used for a given media type can be determined
    # using the +json_parser_parser_for+ class method.
    module JsonParser
      DEFAULT_ERROR_HANDLER = proc{|r| r.halt [400, {}, []]}

      # Exception raised when the request has a JSON media type, but no
      # parser is declared for the media type using the +:parsers+ option.
      class UnsupportedMediaType < RodaError; end

      # Internal result of parsing the request body for a given application
      # class.  Stores the outcome, the parsed parameters (or the result of
      # the error handler), the exception raised by the parser, and whether
      # the error handler has already been called.
      class Result
        attr_reader :outcome
        attr_reader :error
        attr_accessor :params

        def initialize(outcome, params=nil, error=nil)
          @outcome = outcome
          @params = params
          @error = error
          @handled = false
        end

        # Whether the error handler has already been called for this result.
        def handled?
          @handled
        end

        # Mark the error handler as having been called for this result.
        def handled!
          @handled = true
        end
      end
      private_constant :Result

      JSON_PARSER_BODY = "roda.json_parser_body".freeze
      JSON_PARSER_RESULTS = "roda.json_parser_results".freeze
      private_constant :JSON_PARSER_BODY, :JSON_PARSER_RESULTS

      # simplecov:disable
      MATCH_METHOD = RUBY_VERSION >= '2.4' ? :match? : :match
      # simplecov:enable
      private_constant :MATCH_METHOD

      # Normalize the given media type by removing media type parameters
      # and converting it to lower case.  Returns nil for nil, empty, or
      # parameter-only values.
      def self.normalize_media_type(media_type)
        return unless media_type
        media_type = media_type.to_s.split(';', 2).first.to_s.strip.downcase
        media_type unless media_type.empty?
      end

      # Validate and normalize the +:parsers+ option value, raising a
      # RodaError for invalid values.
      def self.normalize_parsers(parsers)
        unless parsers.is_a?(Hash)
          raise RodaError, "the json_parser plugin :parsers option must be a Hash, not #{parsers.class}"
        end

        normalized = {}
        parsers.each do |media_type, parser|
          unless media_type.is_a?(String)
            raise RodaError, "the json_parser plugin :parsers option only accepts String media types, not #{media_type.inspect}"
          end
          unless normalized_media_type = normalize_media_type(media_type)
            raise RodaError, "the json_parser plugin :parsers option does not accept an empty media type"
          end
          if normalized.key?(normalized_media_type)
            raise RodaError, "the json_parser plugin :parsers option has conflicting entries for the #{normalized_media_type.inspect} media type"
          end
          unless parser.respond_to?(:call)
            raise RodaError, "the json_parser plugin :parsers option value for #{media_type.inspect} must respond to #call"
          end

          normalized[normalized_media_type] = parser
        end
        normalized.freeze
      end
      private_class_method :normalize_parsers

      def self.load_dependencies(app, opts=OPTS)
        app.plugin :_json
      end

      # Handle keyword arguments for the json_parser plugin:
      # +content_type_regexp+ :: A regexp used to determine if the request's
      #                          content type is JSON. For backwards compatibility,
      #                          uses the insecure /json/ if not provided. Warns
      #                          if this option is not provided. The recommended
      #                          value to use is /\Aapplication\/json\b/i or
      #                          /\Aapplication\/(?:vnd\.api\+)?json\b/i. Roda 4
      #                          will default to the latter.
      # +error_handler+ :: A proc to call if an exception is raised when
      #                    parsing a JSON request body.  The proc is called
      #                    with the request object, and should probably call
      #                    halt on the request or raise an exception.
      # +parser+ :: The parser to use for parsing incoming json.  Should be
      #             an object that responds to +call(str)+ and returns the
      #             parsed data.  The default is to call JSON.parse.  This is
      #             used for all JSON media types when the +:parsers+ option is
      #             not given.
      # +parsers+ :: A hash mapping normalized media types (lower case, without
      #              media type parameters) to the parser to use for that media
      #              type.  This allows parsing different JSON media types using
      #              different rules.  Media types are matched case insensitively,
      #              and media type parameters in the request's Content-Type header
      #              are ignored.  When this option is given, it is authoritative:
      #              if the request's Content-Type is a JSON media type but there
      #              is no matching entry, accessing +r.POST+ raises
      #              UnsupportedMediaType (the +:parser+ option is not used as a
      #              fallback).  This is distinct from a parser raising an
      #              exception, which is handled by the +:error_handler+ option.
      # +include_request+ :: If true, the parser will be called with the request
      #                      object as the second argument, so the parser needs
      #                      to respond to +call(str, request)+.
      # +wrap+ :: Whether to wrap uploaded JSON data in a hash with a "_json"
      #           key.  Without this, calls to +r.params+ will fail if a non-Hash
      #           (such as an array) is uploaded in JSON format.  A value of
      #           :always will wrap all values, and a value of :unless_hash will
      #           only wrap values that are not already hashes.
      def self.configure(app, opts=(no_positional_opts = true; OPTS),
          content_type_regexp: opts[:content_type_regexp],
          error_handler: opts[:error_handler],
          parser: opts[:parser],
          parsers: opts[:parsers],
          include_request: opts[:include_request],
          wrap: opts[:wrap],
          **kw)
        RodaPlugins.check_keywords("the json_parser plugin", no_positional_opts, opts, kw)

        app.opts[:json_parser_error_handler] = error_handler || app.json_parser_error_handler || DEFAULT_ERROR_HANDLER
        app.opts[:json_parser_parser] = parser || app.json_parser_parser || app.json_parser
        app.opts[:json_parser_parsers] = normalize_parsers(parsers) unless parsers.nil?
        app.opts[:json_parser_include_request] = include_request unless include_request.nil?
        app.opts[:json_parser_content_type_regexp] = content_type_regexp || app.json_parser_content_type_regexp

        unless app.json_parser_content_type_regexp
          # RODA4: Switch to /\Aapplication\/(?:vnd\.api\+)?json\b/i by default
          RodaPlugins.warn(':content_type_regexp option not provided to json_parser plugin, using /json/ for backwards compatibility, which may be insecure, using /\Aapplication\/json\b/i or /\Aapplication\/(?:vnd\.api\+)?json\b/i is recommended')
          app.opts[:json_parser_content_type_regexp] = /json/
        end

        case wrap
        when :unless_hash, :always
          app.opts[:json_parser_wrap] = wrap
        when nil
          # Nothing
        else
          raise RodaError, "unsupported option value for json_parser plugin wrap argument: #{wrap.inspect} (should be :unless_hash or :always)"
        end
      end

      module ClassMethods
        RodaPlugins.opt_attr_reader(self, :json_parser_error_handler)
        RodaPlugins.opt_attr_reader(self, :json_parser_parser)
        RodaPlugins.opt_attr_reader(self, :json_parser_parsers)
        RodaPlugins.opt_attr_reader(self, :json_parser_include_request, name: :json_parser_include_request?)
        RodaPlugins.opt_attr_reader(self, :json_parser_content_type_regexp)
        RodaPlugins.opt_attr_reader(self, :json_parser_wrap)

        # The parser the application would use for the given media type, or nil
        # if the application does not have a parser for the media type.  If the
        # +:parsers+ option was given, only media types present in the +:parsers+
        # option have parsers.  Otherwise, the +:parser+ option is used for all
        # media types.  The given media type should already be normalized.
        def json_parser_parser_for(media_type)
          if parsers = json_parser_parsers
            parsers[media_type]
          else
            json_parser_parser
          end
        end
      end

      module RequestMethods
        # If the Content-Type header in the request indicates the body is JSON,
        # parse the request body as JSON.  Ignore an empty request body.  If the
        # application declares media type specific parsers using the +:parsers+
        # option, the parser for the request's media type is used, and if no
        # parser is declared for the request's media type, UnsupportedMediaType
        # is raised.
        def POST
          env = @env

          if result = json_parser_result_for(env)
            case result.outcome
            when :parsed
              return result.params
            when :empty, :none
              # The request body was empty or is not a JSON media type this
              # application parses, so use the default request body parsing.
              return super
            when :invalid
              return _handle_json_parser_error(result)
            when :unsupported_media_type
              raise UnsupportedMediaType, _json_parser_unsupported_message
            end
          end

          unless (input = env["rack.input"]) && json_parser_json_media_type?
            _set_json_parser_result(env, Result.new(:none))
            return super
          end

          unless parser = roda_class.json_parser_parser_for(json_parser_media_type)
            _set_json_parser_result(env, Result.new(:unsupported_media_type))
            raise UnsupportedMediaType, _json_parser_unsupported_message
          end

          str = _read_json_parser_body(env, input)
          if str.empty?
            _set_json_parser_result(env, Result.new(:empty))
            return super
          end

          begin
            json_params = _wrap_json_params(parse_json(str, parser))
          rescue => e
            result = Result.new(:invalid, nil, e)
            _set_json_parser_result(env, result)
            return _handle_json_parser_error(result)
          end

          _set_json_parser_result(env, Result.new(:parsed, json_params))
          # Roda 3 set this environment key with the first application's parsed
          # parameters.  It is no longer used for caching (results are cached
          # per application class), but it is still set for backwards
          # compatibility with code that reads it directly.
          env["roda.json_params"] ||= json_params
          json_params
        end

        # The normalized media type of the request (lower case, without media
        # type parameters), or nil if the request has no Content-Type header.
        # Media types are normalized so that matching is case insensitive and
        # not affected by media type parameters.
        def json_parser_media_type
          JsonParser.normalize_media_type(content_type)
        end

        # The parser this application would use for the request's media type,
        # or nil if the application does not parse the request's media type as
        # JSON.  If the application declares media type specific parsers using
        # the +:parsers+ option, the parser for the request's media type is
        # returned, and nil is returned if there is no parser declared for the
        # request's media type.
        def json_parser_parser
          return unless json_parser_json_media_type?
          roda_class.json_parser_parser_for(json_parser_media_type)
        end

        # The outcome of this application's attempt to parse the request body.
        # Returns nil if this application has not yet attempted to parse the
        # body, and one of the following symbols otherwise:
        #
        # :none :: The request is not a JSON media type this application parses,
        #          so the application did not parse the body.
        # :empty :: The request body was empty.
        # :parsed :: The request body was parsed successfully.
        # :invalid :: The request body could not be decoded by the parser.  The
        #             exception raised is available via +json_parser_error+.
        # :unsupported_media_type :: The request has a JSON media type, but the
        #                             application has no parser declared for the
        #                             media type.
        def json_parser_result
          if result = json_parser_result_for(@env)
            result.outcome
          end
        end

        # The exception raised by the parser when the request body could not be
        # decoded, or nil if the body was not parsed, parsed successfully, or
        # had no parser declared for its media type.
        def json_parser_error
          if result = json_parser_result_for(@env)
            result.error
          end
        end

        private

        # The key used to cache parsing results per application class.  Using
        # the class as the key allows a subapplication with different parsers
        # to parse the cached body with its own parser and keep its own result.
        def json_parser_result_key
          roda_class
        end

        # The parsing result for the current application class, if any.
        def json_parser_result_for(env)
          if results = env[JSON_PARSER_RESULTS]
            results[json_parser_result_key]
          end
        end

        # Cache the given result for the current application class.
        def _set_json_parser_result(env, result)
          results = env[JSON_PARSER_RESULTS] ||= {}
          results[json_parser_result_key] = result
        end

        # Whether the request's content type is a JSON media type according to
        # the +content_type_regexp+ option.
        def json_parser_json_media_type?
          content_type = self.content_type
          !!(content_type && roda_class.json_parser_content_type_regexp.send(MATCH_METHOD, content_type))
        end

        # The error message to use when there is no parser declared for the
        # request's media type.
        def _json_parser_unsupported_message
          "no JSON parser declared for media type #{json_parser_media_type.inspect}"
        end

        # Call the configured error handler for the given invalid result, at
        # most once per result.  The error handler is expected to halt or
        # raise.  If it returns normally, the POST params are nil (wrapped
        # according to the +:wrap+ option), matching the behavior of previous
        # versions that did not use the error handler's return value.
        def _handle_json_parser_error(result)
          return result.params if result.handled?

          # Mark the result as handled before calling the error handler, so the
          # handler is not called a second time if it raises or halts and the
          # request is handled and POST is called again.
          result.handled!
          roda_class.json_parser_error_handler.call(self)
          result.params = _wrap_json_params(nil)
        end

        # Wrap the given parsed JSON data (or the result of the error handler)
        # based on the +:wrap+ option.
        def _wrap_json_params(json_params)
          wrap = roda_class.json_parser_wrap
          if wrap == :always || (wrap == :unless_hash && !json_params.is_a?(Hash))
            json_params = {"_json"=>json_params}
          end
          json_params
        end

        def parse_json(str, parser)
          if roda_class.json_parser_include_request?
            parser.call(str, self)
          else
            parser.call(str)
          end
        end

        # Read the request body, caching it in the environment so it is never
        # read from the original stream more than once.  The cached body is
        # exposed via +rack.input+ as a rewindable StringIO, so reads by nested
        # applications (or by other code that wants to read the body again) do
        # not touch the original stream.
        def _read_json_parser_body(env, input)
          if str = env[JSON_PARSER_BODY]
            return str
          end

          if Rack.release >= '3'
            # Rack 3 dropped requirement that input be rewindable
            str = input.read
          else
            # Rack 2 and earlier require input to be rewindable, and leave it
            # rewound after reading, so it can be read again.
            input.rewind
            str = input.read
            input.rewind
          end

          env[JSON_PARSER_BODY] = str
          env["rack.input"] = StringIO.new(str)
          str
        end
      end
    end

    register_plugin(:json_parser, JsonParser)
  end
end
