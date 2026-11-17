# --
# Copyright 2007 Nominet UK
# 
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
# 
#     http://www.apache.org/licenses/LICENSE-2.0
# 
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
# ++


# # This class implements a cache.
# It stores data under qname-qclass-qtype tuples.
# Each tuple indexes a CacheData object (which
# stores a Message, and an expiration).
# If a new Message is stored to a tuple, it will
# overwrite the previous Message.
# When a Message is retrieved from the cache, the header
# and ttls will be "fixed" - i.e. AA cleared, etc.



module Dnsruby
  class Cache # :nodoc: all
    @@max_size = 16 * 1024
    @@resolution_generation = 0
    @@generation_mutex = Mutex.new

    def self.invalidate_resolutions
      @@generation_mutex.synchronize { @@resolution_generation += 1 }
    end
    def initialize(preserve_security: false)
    @preserve_security = preserve_security
    @cache = Hash.new
    @mutex = Mutex.new
    end
    def cache
      @cache
    end
    def clear()
      @mutex.synchronize {
        @cache = Hash.new
      }
    end
    def delete(qname, qtype, qclass = Classes.IN, scope: nil)
      qname = Name.create(qname)
      qname.absolute = true
      keys = [CacheKey.new(qname, qtype, qclass).to_s,
        CacheKey.new(qname, Types::ANY, qclass).to_s + ' NXDOMAIN']
      keys.map! { |key| [scope, key] } if scope
      @mutex.synchronize { keys.each { |key| @cache.delete(key) } }
    end
    def length
      @mutex.synchronize { @cache.length }
    end
    def Cache.max_size=(length)
       @@max_size = length
    end
    # A negative response requires an enclosing SOA. Following aliases here
    # keeps a CNAME-only positive answer distinct from a NODATA response.
    def self.negative_soa(message)
      q = message.question.first
      return unless q && [RCode::NOERROR, RCode::NXDOMAIN].include?(message.rcode)
      terminal = q.qname
      seen = {}
      loop do
        return if seen[terminal]
        seen[terminal] = true
        return if message.rcode == RCode::NOERROR && message.answer.any? { |rr|
          rr.name == terminal && rr.klass == q.qclass &&
            (rr.type == q.qtype || (q.qtype == Types::ANY && rr.type != Types::RRSIG)) }
        alias_rr = message.answer.find { |rr| rr.name == terminal && rr.klass == q.qclass && rr.type == Types::CNAME }
        if alias_rr
          terminal = alias_rr.cname
        else
          # RFC 6672 permits omission of the synthesized CNAME. Follow the
          # closest enclosing DNAME using complete labels, just as validation
          # does, before deciding whether this response is negative.
          dname = message.answer.select do |rr|
            rr.type == Types::DNAME && rr.klass == q.qclass && terminal.subdomain_of?(rr.name)
          end.max_by { |rr| rr.name.labels.length }
          break unless dname
          terminal = Name.new(terminal.labels.first(terminal.labels.length - dname.name.labels.length) +
            dname.domainname.labels, true)
        end
      end
      message.authority.select { |rr| rr.type == Types::SOA && rr.klass == q.qclass &&
        (terminal == rr.name || terminal.subdomain_of?(rr.name)) }.max_by { |rr| rr.name.labels.length }
    rescue ResolvError
      nil
    end

    def add(message, scope: nil, error: nil)
      return if message.header.tc
      q = message.question.first
      return unless q
      soa = Cache.negative_soa(message)
      return if message.rcode == RCode::NXDOMAIN && !soa
      return if message.rcode == RCode::NOERROR && message.answer.empty? && !soa
      key = CacheKey.new(q.qname, q.qtype, q.qclass).to_s
      # A bare NXDOMAIN applies to all types. With aliases, retain the full
      # original question so a CNAME query never receives a false name error.
      if soa && message.rcode == RCode::NXDOMAIN && message.answer.empty?
        key = CacheKey.new(q.qname, Types::ANY, q.qclass).to_s + ' NXDOMAIN'
      end
      key = [scope, key] if scope
      data = CacheData.new(message, preserve_security: @preserve_security, error: error)
      return if data.expiration <= Time.now.to_i
      @mutex.synchronize do
        @cache.delete(key)
        @cache[key] = data
        @cache.shift while @cache.size > @@max_size
      end
      data
    end
    #  This method "fixes up" the response, so that the header and ttls are OK
    #  The resolver will still need to copy the flags and ID across from the query
    def find(qname, qtype, qclass = Classes.IN, scope: nil)
      qn = Name.create(qname)
      qn.absolute = true
      key = CacheKey.new(qn, qtype, qclass).to_s
      name_error_key = CacheKey.new(qn, Types::ANY, qclass).to_s + ' NXDOMAIN'
      key, name_error_key = [scope, key], [scope, name_error_key] if scope
      data = @mutex.synchronize {
        key = name_error_key if !@cache[key] && @cache[name_error_key]
        data = @cache[key]
        if (!data)
          return nil
        end
        if (data.expiration <= Time.now.to_i)
          @cache.delete(key)
          TheLog.debug("CACHE lookup stale\n")
          return nil
        end
        data
      }
      # CacheData owns an immutable wire snapshot, so decoding does not need to
      # serialize other cache lookups, insertions or clears behind this lock.
      m = data.message
      if data.expiration <= Time.now.to_i
        @mutex.synchronize { @cache.delete(key) if @cache[key].equal?(data) }
        return nil
      end
      TheLog.debug("CACHE found\n")
      m.question[0] = Question.new(qname, qtype, qclass)
      m
    end
    def Cache.delete(qname, qtype, qclass = Classes.IN)
      key = CacheKey.new(qname, qtype, qclass)
      @mutex.synchronize {
        @cache.delete(key)
      }
    end
    def self.validation_scope
      [@@generation_mutex.synchronize { @@resolution_generation },
        Dnssec.validation_policy, Dnssec.validation_limits,
        [Dnssec.root_verifier, Dnssec.anchor_verifier, Dnssec.dlv_verifier].map(&:validation_cache_identity),
        validation_discovery_scope]
    end

    DISCOVERY_SCOPE_FIELDS = %i[port use_tcp no_tcp tcp_pipelining tcp_pipelining_max_queries
      tcp_idle_timeout packet_timeout query_timeout retry_times retry_delay dnssec do_validation
      do_caching recurse udp_size ignore_truncation src_address src_address6 src_port tsig].freeze
    private_constant :DISCOVERY_SCOPE_FIELDS

    def self.validation_discovery_scope
      resolver = Dnssec.default_resolver
      # This snapshot may run while the caller owns this resolver's packet
      # mutex. Read the existing configuration without invoking lazy setup or
      # acquiring resolver locks; insertion rechecks the complete snapshot.
      snapshot = lambda do |object, fields|
        fields.map { |field| immutable_scope_value(object.instance_variable_get("@#{field}")) }
      end
      configuration = if resolver
        senders = resolver.instance_variable_get(:@single_resolvers) || []
        config = resolver.instance_variable_get(:@config)
        [snapshot.call(resolver, DISCOVERY_SCOPE_FIELDS),
          immutable_scope_value(config&.instance_variable_get(:@nameserver)),
          senders.map { |sender| [sender.object_id, snapshot.call(sender, [:server] + DISCOVERY_SCOPE_FIELDS)] }.
            sort_by(&:inspect)]
      end
      [Dnssec.do_validation_with_recursor?, resolver&.object_id, configuration]
    end

    def self.immutable_scope_value(value)
      case value
      when String then value.dup.freeze
      when Array then value.map { |entry| immutable_scope_value(entry) }.freeze
      when Hash then value.to_h { |key, entry| [immutable_scope_value(key), immutable_scope_value(entry)] }.freeze
      when Numeric, Symbol, NilClass, TrueClass, FalseClass then value
      else
        if value.is_a?(RR::TSIG)
          [value.name.to_s, value.algorithm.to_s, value.key.to_s, value.fudge].map { |entry| immutable_scope_value(entry) }
        else
          value.to_s.dup.freeze
        end
      end
    end
    private_class_method :validation_discovery_scope, :immutable_scope_value

    # A resolver's completed negative answers and resolution failures. Failure
    # retention is deliberately short (RFC 9520 requires at least one second).
    # The caller supplies a scope captured before the resolution begins.
    class ResolutionCache # :nodoc:
      FAILURE_LIFETIME = 1
      FAILURE_LIMIT = 1024

      def initialize
        @negative = Cache.new(preserve_security: true)
        @failures = {}
        @mutex = Mutex.new
      end

      def find(query, scope)
        return unless scope
        question = query.question.first
        @mutex.synchronize do
          response = @negative.find(question.qname, question.qtype, question.qclass, scope: scope)
          if response
            response.header.id = query.header.id
            failure = response.instance_variable_get(:@dnsruby_completion_error)
            return [response, failure && failure[0].new(failure[1])]
          end
          key = [scope, CacheKey.new(question.qname, question.qtype, question.qclass).to_s]
          entry = @failures[key]
          return unless entry
          if entry[0] <= TransportClock.now
            @failures.delete(key)
            return
          end
          response = entry[1]&.message
          response.header.id = query.header.id if response
          [response, entry[2] && entry[2].new(entry[3])]
        end
      end

      def add(query, scope, response, error, control: nil, no_response: false)
        return unless scope
        begin
          control&.check!
        rescue QueryControl::Stopped
          return
        end
        return if response && (response.tsig || response.signed? || response.header.tc)
        @mutex.synchronize do
          question = query.question.first
          key = [scope, CacheKey.new(question.qname, question.qtype, question.qclass).to_s]
          if response && (!error || error.is_a?(NXDomain)) &&
              [RCode::NOERROR, RCode::NXDOMAIN].include?(response.rcode) &&
              response.security_level != Message::SecurityLevel::BOGUS
            # A later successful completion supersedes earlier failures and
            # contradictory negatives, even when the success itself has TTL 0.
            @failures.delete(key)
            @negative.delete(question.qname, question.qtype, question.qclass, scope: scope)
          end
          if response && (!error || error.is_a?(NXDomain)) && Cache.negative_soa(response)
            if query.do_validation && response.security_level == Message::SecurityLevel::INSECURE
              proof_expiration = response.instance_variable_get(:@dnsruby_authenticated_expiration)
              return unless proof_expiration && proof_expiration > Time.now.to_i
            end
            return if response.security_level == Message::SecurityLevel::BOGUS
            @negative.add(response, scope: scope, error: error)
          # Nested lookups may inherit an already-consumed validation budget;
          # its exhaustion does not establish that this question itself fails.
          elsif no_response || (error && !error.is_a?(ValidationCancelled) &&
              !(control && error.is_a?(ValidationLimitError)) &&
              [ResolvTimeout, ServFail, Refused, FormErr, VerifyError, SocketEofResolvError,
                IOError, SystemCallError].any? { |klass| error.is_a?(klass) })
            @negative.delete(question.qname, question.qtype, question.qclass, scope: scope)
            snapshot = response && CacheData.new(response, lifetime: FAILURE_LIFETIME, preserve_security: true)
            @failures.delete(key)
            @failures[key] = [TransportClock.now + FAILURE_LIFETIME,
              snapshot, error&.class, error&.message&.dup]
            @failures.shift while @failures.length > FAILURE_LIMIT
          end
        end
      end
    end

    class CacheKey # :nodoc: all
      attr_accessor :qname, :qtype, :qclass
      def initialize(*args)
        self.qclass = Classes.IN
        if (args.length > 0)
          self.qname = Name.create(args[0])
          self.qname.absolute = true
          if (args.length > 1)
            self.qtype = Types.new(args[1])
            if (args.length > 2)
              self.qclass = Classes.new(args[2])
            end
          end
        end
      end
      def to_s
        # Numeric presentation spellings (TYPE1, TYPE0001, CLASS1, ...) are
        # aliases of the same wire values, including when a decoded answer
        # uses the registered mnemonics instead of the query's spelling.
        return "#{qname.inspect.downcase} #{qclass.code} #{qtype.code}"
      end
    end
    class CacheData # :nodoc: all
      Snapshot = Struct.new(:wire, :expiration, :record_expirations, :authenticated_expirations)
      private_constant :Snapshot

      def expiration
        @snapshot.expiration
      end

      def message=(m)
        now = Time.now.to_i
        record_expirations = []
        authenticated_expirations = []
        @authenticated_expiration = m.instance_variable_get(:@dnsruby_authenticated_expiration)
        soa = Cache.negative_soa(m)
        m.each_resource do |rr|
          authenticated = [rr.instance_variable_get(:@dnsruby_authenticated_expiration),
            @authenticated_expiration].compact.min
          authenticated_expirations << authenticated
          record_expirations << if rr.type != Types::OPT
            received_at = rr.instance_variable_get(:@dnsruby_received_at) || now
            ttl = rr.equal?(soa) ? [rr.ttl, soa.minimum].min : rr.ttl
            [received_at + ttl, authenticated].compact.min
          end
        end
        @snapshot = Snapshot.new(m.encode(true).freeze,
          @lifetime ? now + @lifetime : (record_expirations.compact.min || 0), record_expirations.freeze,
          authenticated_expirations.freeze).freeze
      end
      def message
        snapshot = @snapshot
        m = Message.decode(snapshot.wire)
        m.cached = true
        m.instance_variable_set(:@dnsruby_authenticated_expiration, @authenticated_expiration) if @authenticated_expiration
        m.instance_variable_set(:@dnsruby_completion_error, @completion_error) if @completion_error
        m.security_level = @security_level if @security_level
        m.security_error = if @security_error.is_a?(Exception)
          @security_error.exception(@security_error.message)
        elsif @security_error
          @security_error.dup
        end
        #  @TODO@ What do we do about answerfrom, answersize, etc.?
        m.header.aa = false # Anything else to do here?
        #  Fix up TTLs!!
        now = Time.now.to_i
        index = -1
        m.each_resource {|rr|
          index += 1
          next if rr.type == Types::OPT
          rr.ttl = [[snapshot.record_expirations[index] - now, rr.ttl].min, 0].max
          rr.instance_variable_set(:@dnsruby_received_at, now)
          authenticated = snapshot.authenticated_expirations[index]
          rr.instance_variable_set(:@dnsruby_authenticated_expiration, authenticated) if authenticated
        }
        return m
      end
      def get_expiration(m)
        now = Time.now.to_i
        expirations = []
        soa = Cache.negative_soa(m)
        m.each_resource do |rr|
          next if rr.type == Types::OPT
          received_at = rr.instance_variable_get(:@dnsruby_received_at) || now
          ttl = rr.equal?(soa) ? [rr.ttl, soa.minimum].min : rr.ttl
          expirations << [received_at + ttl,
            rr.instance_variable_get(:@dnsruby_authenticated_expiration),
            m.instance_variable_get(:@dnsruby_authenticated_expiration)].compact.min
        end
        expirations.min || 0
      end
      def initialize(message, lifetime: nil, preserve_security: false, error: nil)
        @lifetime = lifetime
        @completion_error = error && [error.class, error.message.dup.freeze].freeze
        if preserve_security
          @security_level = message.security_level
          @security_error = message.security_error
        end
        self.message = message
      end
      def to_s
        return "#{self.message}"
      end
    end
  end
end
