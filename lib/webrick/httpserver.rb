# frozen_string_literal: true
#
# httpserver.rb -- HTTPServer Class
#
# Author: IPR -- Internet Programming with Ruby -- writers
# Copyright (c) 2000, 2001 TAKAHASHI Masayoshi, GOTOU Yuuzou
# Copyright (c) 2002 Internet Programming with Ruby writers. All rights
# reserved.
#
# $IPR: httpserver.rb,v 1.63 2002/10/01 17:16:32 gotoyuzo Exp $

require 'io/wait' if RUBY_VERSION < "3.2"
require 'ipaddr'
require_relative 'server'
require_relative 'httputils'
require_relative 'httpstatus'
require_relative 'httprequest'
require_relative 'httpresponse'
require_relative 'httpservlet'
require_relative 'accesslog'

module WEBrick
  class HTTPServerError < ServerError; end

  ##
  # Raised when a request with a host name matches several virtual hosts with
  # the same specificity, so there is no way to tell which one should serve
  # it.  Give the virtual hosts distinct +:BindAddress+, +:Port+ or
  # +:ServerAlias+ values to disambiguate them.
  #
  # #candidates lists the tied virtual hosts together with the location where
  # each one was declared, and that list (when it is available) is also used
  # as the backtrace of the error, so a bare <tt>rescue => ex; warn ex</tt>
  # prints the declaration sites.

  class AmbiguousVirtualHostError < HTTPServerError
    ##
    # The host of the request that could not be routed

    attr_reader :request_host

    ##
    # The port the request that could not be routed arrived on

    attr_reader :request_port

    ##
    # An Array of <tt>[virtual_host, declaration_location]</tt> pairs.  The
    # declaration location is a String such as <tt>"server.rb:42:in
    # `&lt;main&gt;'"</tt>, or nil when it could not be determined.

    attr_reader :candidates

    ##
    # Creates the error for a request for +request_host+ on +request_port+
    # that matches +candidates+, an Array of
    # <tt>[virtual_host, declaration_location]</tt> pairs.

    def initialize(request_host, request_port, candidates)
      @request_host = request_host
      @request_port = request_port
      @candidates = candidates

      details = candidates.each_with_index.map{|(server, location), i|
        "  candidate #{i + 1}: #{describe(server)} " \
          "declared at #{location || "(unknown location)"}"
      }
      super(
        "ambiguous virtual host for host=#{request_host.inspect} " \
        "port=#{request_port.inspect}: #{candidates.size} virtual hosts " \
        "match with the same specificity; declare distinct " \
        ":BindAddress, :Port or :ServerAlias values to disambiguate them\n" \
        "#{details.join("\n")}"
      )

      frames = candidates.map{|_, location| location }.compact.uniq
      set_backtrace(frames) unless frames.empty?
    end

    ##
    # A one line description of the configuration of +server+.  Only the
    # attributes the virtual host actually declared are shown; the rest are
    # inherited from the defaults and take no part in routing.

    def describe(server)
      return server.inspect unless server.respond_to?(:[])
      declared = server.respond_to?(:declared_config_keys) ?
        server.declared_config_keys : [:BindAddress, :Port, :ServerName,
                                       :ServerAlias]
      parts = [:BindAddress, :Port, :ServerName, :ServerAlias].map{|key|
        next unless declared.include?(key)
        next if server[key].nil?
        "#{key}=#{server[key].inspect}"
      }.compact
      parts.empty? ? "no declared attributes" : parts.join(" ")
    end
  end

  ##
  # An HTTP Server

  class HTTPServer < ::WEBrick::GenericServer
    ##
    # The configuration keys that were given to this server explicitly, as an
    # Array of Symbols.  Keys that only exist because of the configuration
    # defaults are not included.

    attr_reader :declared_config_keys

    ##
    # Creates a new HTTP server according to +config+
    #
    # An HTTP server uses the following attributes:
    #
    # :AccessLog:: An array of access logs.  See WEBrick::AccessLog
    # :BindAddress:: Local address for the server to bind to
    # :DocumentRoot:: Root path to serve files from
    # :DocumentRootOptions:: Options for the default HTTPServlet::FileHandler
    # :HTTPVersion:: The HTTP version of this server
    # :Port:: Port to listen on
    # :RequestCallback:: Called with a request and response before each
    #                    request is serviced.
    # :RequestTimeout:: Maximum time to wait between requests
    # :ServerAlias:: Array of alternate names for this server for virtual
    #                hosting
    # :ServerName:: Name for this server for virtual hosting
    #
    # == Virtual host selection
    #
    # #virtual_host registers extra servers ("virtual hosts") that the
    # listening server dispatches to.  #lookup_server chooses which one serves
    # a request; when none matches, the listening server itself (the default
    # host) serves it.
    #
    # A virtual host only constrains the attributes that were given to it
    # explicitly and are not nil:
    #
    # :BindAddress:: the local address of the TCP connection,
    #                <tt>req.addr[3]</tt>.  <tt>"0.0.0.0"</tt> and
    #                <tt>"::"</tt> match every local address, and IP
    #                addresses are compared by value, so an IPv6 address
    #                matches whether it is written with brackets or not.
    # :Port:: the port that accepted the connection, <tt>req.addr[1]</tt> -
    #         the port the client connected to, not the one in the request
    #         URI.  The port named by the request (<tt>req.port</tt>), the
    #         one in the +Host+ header, is accepted too, so a virtual host
    #         declared for a listening port still serves clients that send a
    #         bare +Host+ header.  Requests without a socket address (test
    #         doubles, for example) fall back to <tt>req.port</tt>.  The
    #         listening port is what tells apart virtual hosts that share an
    #         alias but listen on different ports.
    # :ServerName, :ServerAlias:: the host of the request, <tt>req.host</tt>,
    #         taken from the +Host+ header, an absolute request URI or
    #         +X-Forwarded-Host+.  Strings compare exactly; an IPv6 host
    #         therefore keeps the bracketed form the +Host+ header uses, just
    #         as it always has, while a +:BindAddress+ is compared as an
    #         address.  A virtual host that declares no name accepts any
    #         host.
    #
    # A request that names no host at all (HTTP/1.0 without a +Host+ header)
    # gets the local address of the connection as its host, so a virtual host
    # that constrains only the address or the port still serves such a
    # request and the listening server serves it otherwise.
    #
    # Every attribute the virtual host declares must match.  Among the
    # matching virtual hosts the most specific one wins, comparing in turn:
    # a concrete +:BindAddress+ over a wildcard one over none at all, then a
    # declared +:Port+, then a declared name (+:ServerName+ or a non-empty
    # +:ServerAlias+).  The address and the port therefore disambiguate
    # virtual hosts that share a host name, and the host name disambiguates
    # virtual hosts that share an address and a port.
    #
    # When several virtual hosts still match with the same specificity, no
    # virtual host can be chosen on the strength of the request alone:
    # #lookup_server raises AmbiguousVirtualHostError, whose #candidates
    # report the tied virtual hosts and where each was declared.  A request
    # that carries no host name of its own - HTTP/1.0 without a +Host+ header
    # - cannot disambiguate anything either, so there WEBrick keeps its
    # historical behaviour: the first declared of the most specific virtual
    # hosts serves the request, and a request that matches no virtual host is
    # served by the listening server.

    def initialize(config={}, default=Config::HTTP)
      # Attributes that were actually given to this server.  Attributes that
      # are only present because the defaults supply them (Config::HTTP gives
      # every server :Port => 80 and a :ServerName) must not constrain it
      # when it is attached to another server with #virtual_host.
      @declared_config_keys =
        config.respond_to?(:keys) ? config.keys.map{|key| key.to_sym } : []
      super(config, default)
      @http_version = HTTPVersion::convert(@config[:HTTPVersion])

      @mount_tab = MountTable.new
      if @config[:DocumentRoot]
        mount("/", HTTPServlet::FileHandler, @config[:DocumentRoot],
              @config[:DocumentRootOptions])
      end

      unless @config[:AccessLog]
        @config[:AccessLog] = [
          [ $stderr, AccessLog::COMMON_LOG_FORMAT ],
          [ $stderr, AccessLog::REFERER_LOG_FORMAT ]
        ]
      end

      @virtual_hosts = Array.new
      @virtual_host_declarations = {}.compare_by_identity
    end

    ##
    # Processes requests on +sock+

    def run(sock)
      while true
        req = create_request(@config)
        res = create_response(@config)
        server = self
        begin
          timeout = @config[:RequestTimeout]
          while timeout > 0
            break if sock.to_io.wait_readable(0.5)
            break if @status != :Running
            timeout -= 0.5
          end
          raise HTTPStatus::EOFError if timeout <= 0 || @status != :Running
          begin
            raise HTTPStatus::EOFError if sock.eof?
          rescue Errno::ECONNRESET
            raise HTTPStatus::EOFError
          end

          req.parse(sock)
          res.request_method = req.request_method
          res.request_uri = req.request_uri
          res.request_http_version = req.http_version
          res.keep_alive = req.keep_alive?
          server = lookup_server(req) || self
          if callback = server[:RequestCallback]
            callback.call(req, res)
          elsif callback = server[:RequestHandler]
            msg = ":RequestHandler is deprecated, please use :RequestCallback"
            @logger.warn(msg)
            callback.call(req, res)
          end
          server.service(req, res)
        rescue HTTPStatus::EOFError, HTTPStatus::RequestTimeout => ex
          res.set_error(ex)
        rescue HTTPStatus::Error => ex
          @logger.error(ex.message)
          res.set_error(ex)
        rescue HTTPStatus::Status => ex
          res.status = ex.code
        rescue StandardError => ex
          @logger.error(ex)
          res.set_error(ex, true)
        ensure
          if req.request_line
            if req.keep_alive? && res.keep_alive?
              req.fixup()
            end
            res.send_response(sock)
            server.access_log(@config, req, res)
          end
        end
        break if @http_version < "1.1"
        break unless req.keep_alive?
        break unless res.keep_alive?
      end
    end

    ##
    # Services +req+ and fills in +res+

    def service(req, res)
      if req.unparsed_uri == "*"
        if req.request_method == "OPTIONS"
          do_OPTIONS(req, res)
          raise HTTPStatus::OK
        end
        raise HTTPStatus::NotFound, "'#{req.unparsed_uri}' not found."
      end

      servlet, options, script_name, path_info = search_servlet(req.path)
      raise HTTPStatus::NotFound, "'#{req.path}' not found." unless servlet
      req.script_name = script_name
      req.path_info = path_info
      si = servlet.get_instance(self, *options)
      @logger.debug(format("%s is invoked.", si.class.name))
      si.service(req, res)
    end

    ##
    # The default OPTIONS request handler says GET, HEAD, POST and OPTIONS
    # requests are allowed.

    def do_OPTIONS(req, res)
      res["allow"] = "GET,HEAD,POST,OPTIONS"
    end

    ##
    # Mounts +servlet+ on +dir+ passing +options+ to the servlet at creation
    # time

    def mount(dir, servlet, *options)
      @logger.debug(sprintf("%s is mounted on %s.", servlet.inspect, dir))
      @mount_tab[dir] = [ servlet, options ]
    end

    ##
    # Mounts +proc+ or +block+ on +dir+ and calls it with a
    # WEBrick::HTTPRequest and WEBrick::HTTPResponse

    def mount_proc(dir, proc=nil, &block)
      proc ||= block
      raise HTTPServerError, "must pass a proc or block" unless proc
      mount(dir, HTTPServlet::ProcHandler.new(proc))
    end

    ##
    # Unmounts +dir+

    def unmount(dir)
      @logger.debug(sprintf("unmount %s.", dir))
      @mount_tab.delete(dir)
    end
    alias umount unmount

    ##
    # Finds a servlet for +path+

    def search_servlet(path)
      script_name, path_info = @mount_tab.scan(path)
      servlet, options = @mount_tab[script_name]
      if servlet
        [ servlet, options, script_name, path_info ]
      end
    end

    ##
    # Adds +server+ as a virtual host.
    #
    # The location in the caller that declared the virtual host is recorded
    # so AmbiguousVirtualHostError can point back to it.

    def virtual_host(server)
      return server if @virtual_hosts.any?{|s| s.equal?(server)}

      @virtual_host_declarations[server] = virtual_host_declaration_location
      @virtual_hosts << server
      server
    end

    ##
    # Where the caller that declared the virtual host is, in the form
    # <tt>"path:line:in `method'"</tt>.  Frames inside webrick itself (the
    # HTTPS variant of #virtual_host, for example) are skipped so the
    # location points at the user's code.  Returns nil when it cannot be
    # determined.

    def virtual_host_declaration_location # :nodoc:
      libdir = File.expand_path("..", __dir__) + File::SEPARATOR
      locations = caller_locations(1) || []
      location = locations.find{|l|
        !(l.absolute_path || l.path).to_s.start_with?(libdir)
      } || locations.last
      location && location.to_s
    end

    ##
    # Finds the appropriate virtual host to handle +req+, or nil when the
    # listening server itself should serve the request.
    #
    # See the "Virtual host selection" documentation on HTTPServer for the
    # matching and specificity rules.  Raises AmbiguousVirtualHostError when
    # the request carries a host name and several virtual hosts match it
    # with the same specificity.

    def lookup_server(req)
      matching = @virtual_hosts.select{|s| virtual_host_match?(s, req) }
      return nil if matching.empty?

      # Keep only the most specific virtual hosts, preserving declaration
      # order among them.
      matching = matching.group_by{|s| virtual_host_specificity(s) }
      matching = matching[matching.keys.max]
      return matching.first if matching.size == 1

      # With no host name in the request there is nothing to disambiguate
      # with, so keep the historical first-declared choice.
      return matching.first unless virtual_host_host_name_given?(req)

      raise AmbiguousVirtualHostError.new(
        req.host, virtual_host_request_port(req),
        matching.map{|s| [s, @virtual_host_declarations[s]] })
    end

    ##
    # How specifically virtual host +server+ matches, as an Array that
    # compares lexicographically: [address, port, name].
    #
    # The address counts 2 when the virtual host binds a concrete address and
    # 1 when it binds a wildcard address (<tt>"0.0.0.0"</tt>, <tt>"::"</tt>),
    # so a concrete address beats a wildcard one.  The port counts 1 when the
    # virtual host declares one.  The name counts 1 when it declares
    # +:ServerName+ or a non-empty +:ServerAlias+, whether or not it also
    # declares the other.
    #
    # Ordering the components this way keeps the historical precedence of
    # WEBrick - an address outranks a port, which outranks a name - while
    # only counting attributes the virtual host really declares and
    # recognising an alias-only virtual host as name-constrained instead of
    # as unconstrained.

    def virtual_host_specificity(server) # :nodoc:
      [
        virtual_host_address_specificity(server),
        virtual_host_declares?(server, :Port) ? 1 : 0,
        virtual_host_declares_name?(server) ? 1 : 0,
      ]
    end

    ##
    # How specific the +:BindAddress+ of +server+ is: 2 for a concrete
    # address, 1 for a wildcard address, 0 when it declares none.

    def virtual_host_address_specificity(server) # :nodoc:
      return 0 unless virtual_host_declares?(server, :BindAddress)
      if virtual_host_unspecified_address?(server[:BindAddress])
        1
      else
        2
      end
    end

    ##
    # Did +server+ declare +key+ with a non-nil value?  An attribute that is
    # only present because of the configuration defaults is not a
    # declaration.

    def virtual_host_declares?(server, key) # :nodoc:
      !server[key].nil? &&
        (!server.respond_to?(:declared_config_keys) ||
         server.declared_config_keys.include?(key))
    end

    ##
    # Does +server+ declare a host name, either +:ServerName+ or
    # +:ServerAlias+?

    def virtual_host_declares_name?(server) # :nodoc:
      virtual_host_declares?(server, :ServerName) ||
        (virtual_host_declares?(server, :ServerAlias) &&
         virtual_host_aliases(server).any?)
    end

    ##
    # Does virtual host +server+ match +req+?  Every attribute the virtual
    # host declares must match; a virtual host that declares no name accepts
    # any host.

    def virtual_host_match?(server, req) # :nodoc:
      if virtual_host_declares?(server, :BindAddress) &&
         !virtual_host_address_match?(server[:BindAddress], req)
        return false
      end
      if virtual_host_declares?(server, :Port) &&
         !virtual_host_port_match?(server[:Port], req)
        return false
      end
      virtual_host_name_constraint_match?(server, req.host)
    end

    ##
    # Does the name declared by +server+ match +actual+?  A virtual host that
    # declares no name at all accepts any host.

    def virtual_host_name_constraint_match?(server, actual) # :nodoc:
      return true unless virtual_host_declares_name?(server)

      name = server[:ServerName]
      (virtual_host_declares?(server, :ServerName) &&
         virtual_host_name_match?(name, actual)) ||
        (virtual_host_declares?(server, :ServerAlias) &&
         virtual_host_aliases(server).any?{|h|
           virtual_host_name_match?(h, actual)
         })
    end

    ##
    # The +:ServerAlias+ of +server+ as an Array, accepting a single String
    # or Regexp as well as the usual Array.

    def virtual_host_aliases(server) # :nodoc:
      Array(server[:ServerAlias])
    end

    ##
    # Does +configured+, the +:BindAddress+ of a virtual host, match the
    # local address of +req+?  An unspecified address (<tt>"0.0.0.0"</tt> or
    # <tt>"::"</tt>) matches any local address, and when the local address
    # is not known the address selector is ignored.  Otherwise the addresses
    # must be equal, comparing IP addresses by value so that the different
    # spellings of an IPv6 address - with or without brackets, leading zeros
    # and so on - agree.

    def virtual_host_address_match?(configured, req) # :nodoc:
      local = req.addr && req.addr[3]
      return true if local.nil? || local.empty? || local == "unknown"
      return true if virtual_host_unspecified_address?(configured)
      return true if local == configured

      configured_address = virtual_host_ip_address(configured)
      local_address = virtual_host_ip_address(local)
      !!(configured_address && local_address &&
         configured_address == local_address)
    end

    ##
    # Is +address+ an unspecified address that a listening socket may bind
    # to, i.e. one that covers every local address?

    def virtual_host_unspecified_address?(address) # :nodoc:
      return true if address == "0.0.0.0" || address == "::"
      parsed = virtual_host_ip_address(address)
      !parsed.nil? && (parsed.to_s == "0.0.0.0" || parsed.to_s == "::")
    end

    ##
    # Does +configured+, the +:Port+ of a virtual host, match +req+?  A
    # virtual host's port matches either the local port that accepted the
    # connection or the port named by the request, so a virtual host declared
    # for the listening port is reachable even when the client omits the port
    # from its +Host+ header.  When neither port is known a virtual host that
    # declares a port does not match.

    def virtual_host_port_match?(configured, req) # :nodoc:
      local = virtual_host_local_port(req)
      return true if !local.nil? && local.to_i == configured.to_i

      port = req.port
      !port.nil? && port.to_i == configured.to_i
    end

    ##
    # The port +req+ arrived on.  This is the local port of the accepted
    # connection, i.e. the listening port, which is what tells apart virtual
    # hosts that share a name but listen on different ports.  Requests
    # without a usable socket address (test doubles, for example) fall back
    # to +req.port+.

    def virtual_host_request_port(req) # :nodoc:
      virtual_host_local_port(req) || req.port
    end

    ##
    # The local port of the connection +req+ arrived on, or nil when it is
    # not known.

    def virtual_host_local_port(req) # :nodoc:
      local = req.addr && req.addr[1]
      local.nil? || local.to_i == 0 ? nil : local
    end

    ##
    # Did +req+ carry a host name supplied by the client?  That is a +Host+
    # header, an +X-Forwarded-Host+ header, or an absolute request URI.  An
    # HTTP/1.0 request without any of them has no name of its own - WEBrick
    # substitutes the local address of the connection or the configured
    # +:ServerName+ - and such a request must not be reported as ambiguous.

    def virtual_host_host_name_given?(req) # :nodoc:
      return false unless req.respond_to?(:host)
      host = req.host
      return false if host.nil? || host.to_s.empty?
      return true unless req.is_a?(HTTPRequest)

      return true if req["host"]
      return true if req["x-forwarded-host"]
      !!(req.unparsed_uri && req.unparsed_uri =~ %r{\A[a-zA-Z][a-zA-Z0-9+.\-]*://})
    end

    ##
    # Does the name +configured+ (+:ServerName+ or an entry of
    # +:ServerAlias+) match the request host +actual+?  A String compares by
    # equality, a Regexp by matching; nil never matches.

    def virtual_host_name_match?(configured, actual) # :nodoc:
      return false if actual.nil?
      configured === actual
    end

    ##
    # Parses +address+ as an IP address, returning nil for anything else.
    # IPAddr::Error is a subclass of ArgumentError, so one rescue covers both.

    def virtual_host_ip_address(address) # :nodoc:
      IPAddr.new(address)
    rescue ArgumentError
      nil
    end

    ##
    # Logs +req+ and +res+ in the access logs.  +config+ is used for the
    # server name.

    def access_log(config, req, res)
      param = AccessLog::setup_params(config, req, res)
      @config[:AccessLog].each{|logger, fmt|
        logger << AccessLog::format(fmt+"\n", param)
      }
    end

    ##
    # Creates the HTTPRequest used when handling the HTTP
    # request. Can be overridden by subclasses.
    def create_request(with_webrick_config)
      HTTPRequest.new(with_webrick_config)
    end

    ##
    # Creates the HTTPResponse used when handling the HTTP
    # request. Can be overridden by subclasses.
    def create_response(with_webrick_config)
      HTTPResponse.new(with_webrick_config)
    end

    ##
    # Mount table for the path a servlet is mounted on in the directory space
    # of the server.  Users of WEBrick can only access this indirectly via
    # WEBrick::HTTPServer#mount, WEBrick::HTTPServer#unmount and
    # WEBrick::HTTPServer#search_servlet

    class MountTable # :nodoc:
      def initialize
        @tab = Hash.new
        compile
      end

      def [](dir)
        dir = normalize(dir)
        @tab[dir]
      end

      def []=(dir, val)
        dir = normalize(dir)
        @tab[dir] = val
        compile
        val
      end

      def delete(dir)
        dir = normalize(dir)
        res = @tab.delete(dir)
        compile
        res
      end

      def scan(path)
        @scanner =~ path
        [ $&, $' ]
      end

      private

      def compile
        k = @tab.keys
        k.sort!
        k.reverse!
        k.collect!{|path| Regexp.escape(path) }
        @scanner = Regexp.new("\\A(" + k.join("|") +")(?=/|\\z)")
      end

      def normalize(dir)
        ret = dir ? dir.dup : +""
        ret.sub!(%r|/+\z|, "")
        ret
      end
    end
  end
end
