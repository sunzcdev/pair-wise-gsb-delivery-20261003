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
#
# Negative responses (RFC 2308) are cached only when the response carries
# an authoritative basis: an SOA in the authority section enclosing the
# name where non-existence was proved. Each negative is described by a
# NegativeEntry recording the exact question scope, the authoritative SOA,
# the lifetime computed from it (the lesser of the SOA TTL and MINIMUM),
# and the position on the alias chain where non-existence was found.
# NXDOMAIN for the queried name itself covers every type; NODATA covers
# only its own qname-qclass-qtype tuple and is never widened into a name
# error. Entries expire on their computed lifetime or when the zone
# changes (a different SOA serial), and an unexpired positive answer
# always outranks a contradictory negative.



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
    @negative_index = Hash.new
    @mutex = Mutex.new
    end
    def cache
      @cache
    end
    def clear()
      @mutex.synchronize {
        @cache = Hash.new
        @negative_index = Hash.new
      }
    end
    def delete(qname, qtype, qclass = Classes.IN, scope: nil)
      qname = Name.create(qname)
      qname.absolute = true
      keys = [CacheKey.new(qname, qtype, qclass).to_s,
        CacheKey.new(qname, Types::ANY, qclass).to_s + ' NXDOMAIN']
      keys.map! { |key| [scope, key] } if scope
      @mutex.synchronize { keys.each { |key| drop(key) } }
    end
    def length
      @mutex.synchronize { @cache.length }
    end
    def Cache.max_size=(length)
       @@max_size = length
    end
    # The authoritative content of a negative response: the question it
    # answers, the name on the alias chain where non-existence was proved,
    # the enclosing SOA, and whether the whole name is denied.
    NegativeProof = Struct.new(:question, :rcode, :terminal, :soa, :name_error) do
      alias name_error? name_error
    end

    # A negative response requires an enclosing SOA. Following aliases here
    # keeps a CNAME-only positive answer distinct from a NODATA response.
    def self.negative_proof(message)
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
      soa = message.authority.select { |rr| rr.type == Types::SOA && rr.klass == q.qclass &&
        (terminal == rr.name || terminal.subdomain_of?(rr.name)) }.max_by { |rr| rr.name.labels.length }
      return unless soa
      # Only a bare NXDOMAIN (no aliases in the answer) denies the queried
      # name itself, and with it every type at that name.
      NegativeProof.new(q, message.rcode, terminal, soa,
        message.rcode == RCode::NXDOMAIN && message.answer.empty?)
    rescue ResolvError
      nil
    end

    def self.negative_soa(message)
      proof = negative_proof(message)
      proof && proof.soa
    end

    def add(message, scope: nil, error: nil)
      return if message.header.tc
      q = message.question.first
      return unless q
      now = Time.now.to_i
      proof = Cache.negative_proof(message)
      # Failures without an authoritative basis never enter the negative
      # cache: no enclosing SOA, no negative entry.
      return if message.rcode == RCode::NXDOMAIN && !proof
      return if message.rcode == RCode::NOERROR && message.answer.empty? && !proof
      key = CacheKey.new(q.qname, q.qtype, q.qclass).to_s
      entry = proof && NegativeEntry.new(proof, now)
      return if entry && entry.expiration <= now
      # A bare NXDOMAIN applies to all types. With aliases, retain the full
      # original question so a CNAME query never receives a false name error.
      key = CacheKey.new(q.qname, Types::ANY, q.qclass).to_s + ' NXDOMAIN' if entry && entry.name_error?
      key = [scope, key] if scope
      data = CacheData.new(message, preserve_security: @preserve_security, error: error)
      data.negative_entry = entry if entry
      return if data.expiration <= now
      @mutex.synchronize do
        if entry
          # Fixed priority: an unexpired positive answer outranks any
          # negative for the identical question.
          existing = @cache[key]
          return if existing && !existing.negative? && existing.expiration > now
          purge_conflicting_negatives(entry, key)
          purge_superseded_zone_entries(entry)
        elsif message.rcode == RCode::NOERROR && !message.answer.empty?
          # Fixed priority: a positive answer supersedes a contradictory
          # whole-name error for the same name.
          name_error_key = CacheKey.new(q.qname, Types::ANY, q.qclass).to_s + ' NXDOMAIN'
          name_error_key = [scope, name_error_key] if scope
          drop(name_error_key)
        end
        drop(key)
        @cache[key] = data
        @negative_index[key] = entry if entry
        while @cache.size > @@max_size
          evicted, = @cache.shift
          @negative_index.delete(evicted)
        end
      end
      data
    end
    #  This method "fixes up" the response, so that the header and ttls are OK
    #  The resolver will still need to copy the flags and ID across from the query
    def find(qname, qtype, qclass = Classes.IN, scope: nil)
      qn = Name.create(qname)
      qn.absolute = true
      # One fixed clock reading per lookup decides the hit scope and the
      # remaining TTL of everything this call returns.
      now = Time.now.to_i
      key = CacheKey.new(qn, qtype, qclass).to_s
      name_error_key = CacheKey.new(qn, Types::ANY, qclass).to_s + ' NXDOMAIN'
      key, name_error_key = [scope, key], [scope, name_error_key] if scope
      data = @mutex.synchronize {
        key = name_error_key if !@cache[key] && @cache[name_error_key]
        data = @cache[key]
        if (!data)
          return nil
        end
        if (data.expiration <= now)
          drop(key)
          TheLog.debug("CACHE lookup stale\n")
          return nil
        end
        data
      }
      entry = data.negative_entry
      # A cached negative answers only its recorded question scope: one
      # type's absence is never widened into the absence of the whole name.
      if entry && !entry.covers?(qn, qtype, qclass)
        TheLog.debug("CACHE negative scope mismatch\n")
        return nil
      end
      # CacheData owns an immutable wire snapshot, so decoding does not need to
      # serialize other cache lookups, insertions or clears behind this lock.
      m = data.message
      if (data.expiration <= Time.now.to_i)
        @mutex.synchronize { drop(key) if @cache[key].equal?(data) }
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

    private

    # Caller holds @mutex.
    def drop(key)
      @cache.delete(key)
      @negative_index.delete(key)
    end

    # A whole-name error and a type-specific NODATA at the same name cannot
    # both be true; the most recent authoritative answer wins. Caller holds
    # @mutex.
    def purge_conflicting_negatives(entry, keep_key)
      @negative_index.each do |key, other|
        next if key == keep_key
        next unless other.qname == entry.qname && other.qclass == entry.qclass
        next unless other.name_error? || entry.name_error?
        drop(key)
      end
    end

    # Negatives are authorised by the zone's SOA. Once the zone changes
    # (a different serial), earlier negatives from that zone must not be
    # reused. Caller holds @mutex.
    def purge_superseded_zone_entries(entry)
      @negative_index.each do |key, other|
        next unless other.zone_key == entry.zone_key && other.zone_serial != entry.zone_serial
        drop(key)
      end
    end

    public

    # A negative cache entry: the exact question scope it may answer, the
    # authoritative SOA, the lifetime computed from that SOA, and the
    # position on the alias chain where non-existence was proved.
    class NegativeEntry # :nodoc: all
      attr_reader :qname, :qtype, :qclass, :terminal, :soa, :received_at

      def initialize(proof, received_at = Time.now.to_i)
        @qname = proof.question.qname
        @qclass = proof.question.qclass
        @name_error = proof.name_error?
        @qtype = @name_error ? Types::ANY : proof.question.qtype
        @terminal = proof.terminal
        @soa = proof.soa
        @received_at = received_at
      end

      # NXDOMAIN for the queried name itself denies every type at that
      # name. Anything else (NODATA, or non-existence further along an
      # alias chain) denies only the recorded question.
      def name_error?
        @name_error
      end

      def nodata?
        !@name_error
      end

      # RFC 2308: the negative lifetime is the lesser of the SOA TTL and
      # the SOA MINIMUM field, counted from when the response was received.
      def lifetime
        [@soa.ttl, @soa.minimum].min
      end

      def expiration
        @received_at + lifetime
      end

      def expired?(now = Time.now.to_i)
        expiration <= now
      end

      def remaining_ttl(now = Time.now.to_i)
        [expiration - now, 0].max
      end

      # The exact question scope this entry may answer. One type's absence
      # is never widened into the absence of the whole name.
      def covers?(qname, qtype, qclass = Classes::IN)
        name = Name.create(qname)
        name.absolute = true
        return false unless name == @qname && Classes.new(qclass) == @qclass
        name_error? || Types.new(qtype) == @qtype
      end

      # Identity of the zone whose SOA authorised this entry.
      def zone_key
        "#{@soa.name.inspect.downcase} #{@soa.klass.code}"
      end

      def zone_serial
        @soa.serial
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

      # The NegativeEntry describing this response, when it is negative.
      attr_accessor :negative_entry

      def negative?
        !@negative_entry.nil?
      end

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
      def message(now = Time.now.to_i)
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
