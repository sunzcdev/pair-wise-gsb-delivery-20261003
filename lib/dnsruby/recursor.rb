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
require 'dnsruby/query_control'

module Dnsruby
  # Dnsruby::Recursor - Perform recursive dns lookups
  # 
  #   require 'Dnsruby'
  #   rec = Dnsruby::Recursor.new()
  #   answer = rec.query("rob.com.au")
  # 
  # This module uses a Dnsruby::Resolver to perform recursive queries.
  # 
  # === AUTHOR
  # 
  # Rob Brown, bbb@cpan.org
  # Alex Dalitz, alexd@nominet.org.uk
  # 
  # === SEE ALSO
  # 
  # Dnsruby::Resolver,
  # 
  # === COPYRIGHT
  # 
  # Copyright (c) 2002, Rob Brown.  All rights reserved.
  # Portions Copyright (c) 2005, Olaf M Kolkman.
  # Ruby version with caching and validation Copyright (c) 2008, AlexD (Nominet UK)
  # 
  # Example lookup process:
  # 
  # [root@box root]# dig +trace www.rob.com.au.
  # 
  # ; <<>> DiG 9.2.0 <<>> +trace www.rob.com.au.
  # ;; global options:  printcmd
  # .                       507343  IN      NS      C.ROOT-SERVERS.NET.
  # .                       507343  IN      NS      D.ROOT-SERVERS.NET.
  # .                       507343  IN      NS      E.ROOT-SERVERS.NET.
  # .                       507343  IN      NS      F.ROOT-SERVERS.NET.
  # .                       507343  IN      NS      G.ROOT-SERVERS.NET.
  # .                       507343  IN      NS      H.ROOT-SERVERS.NET.
  # .                       507343  IN      NS      I.ROOT-SERVERS.NET.
  # .                       507343  IN      NS      J.ROOT-SERVERS.NET.
  # .                       507343  IN      NS      K.ROOT-SERVERS.NET.
  # .                       507343  IN      NS      L.ROOT-SERVERS.NET.
  # .                       507343  IN      NS      M.ROOT-SERVERS.NET.
  # .                       507343  IN      NS      A.ROOT-SERVERS.NET.
  # .                       507343  IN      NS      B.ROOT-SERVERS.NET.
  # ;; Received 436 bytes from 127.0.0.1#53(127.0.0.1) in 9 ms
  #   ;;; But these should be hard coded as the hints
  # 
  #   ;;; Ask H.ROOT-SERVERS.NET gave:
  # au.                     172800  IN      NS      NS2.BERKELEY.EDU.
  # au.                     172800  IN      NS      NS1.BERKELEY.EDU.
  # au.                     172800  IN      NS      NS.UU.NET.
  # au.                     172800  IN      NS      BOX2.AUNIC.NET.
  # au.                     172800  IN      NS      SEC1.APNIC.NET.
  # au.                     172800  IN      NS      SEC3.APNIC.NET.
  # ;; Received 300 bytes from 128.63.2.53#53(H.ROOT-SERVERS.NET) in 322 ms
  #   ;;; A little closer than before
  # 
  #   ;;; Ask NS2.BERKELEY.EDU gave:
  # com.au.                 259200  IN      NS      ns4.ausregistry.net.
  # com.au.                 259200  IN      NS      dns1.telstra.net.
  # com.au.                 259200  IN      NS      au2ld.CSIRO.au.
  # com.au.                 259200  IN      NS      audns01.syd.optus.net.
  # com.au.                 259200  IN      NS      ns.ripe.net.
  # com.au.                 259200  IN      NS      ns1.ausregistry.net.
  # com.au.                 259200  IN      NS      ns2.ausregistry.net.
  # com.au.                 259200  IN      NS      ns3.ausregistry.net.
  # com.au.                 259200  IN      NS      ns3.melbourneit.com.
  # ;; Received 387 bytes from 128.32.206.12#53(NS2.BERKELEY.EDU) in 10312 ms
  #   ;;; A little closer than before
  # 
  #   ;;; Ask ns4.ausregistry.net gave:
  # com.au.                 259200  IN      NS      ns1.ausregistry.net.
  # com.au.                 259200  IN      NS      ns2.ausregistry.net.
  # com.au.                 259200  IN      NS      ns3.ausregistry.net.
  # com.au.                 259200  IN      NS      ns4.ausregistry.net.
  # com.au.                 259200  IN      NS      ns3.melbourneit.com.
  # com.au.                 259200  IN      NS      dns1.telstra.net.
  # com.au.                 259200  IN      NS      au2ld.CSIRO.au.
  # com.au.                 259200  IN      NS      ns.ripe.net.
  # com.au.                 259200  IN      NS      audns01.syd.optus.net.
  # ;; Received 259 bytes from 137.39.1.3#53(ns4.ausregistry.net) in 606 ms
  #   ;;; Uh... yeah... I already knew this
  #   ;;; from what NS2.BERKELEY.EDU told me.
  #   ;;; ns4.ausregistry.net must have brain damage
  # 
  #   ;;; Ask ns1.ausregistry.net gave:
  # rob.com.au.             86400   IN      NS      sy-dns02.tmns.net.au.
  # rob.com.au.             86400   IN      NS      sy-dns01.tmns.net.au.
  # ;; Received 87 bytes from 203.18.56.41#53(ns1.ausregistry.net) in 372 ms
  #   ;;; Ah, much better.  Something more useful.
  # 
  #   ;;; Ask sy-dns02.tmns.net.au gave:
  # www.rob.com.au.         7200    IN      A       139.134.5.123
  # rob.com.au.             7200    IN      NS      sy-dns01.tmns.net.au.
  # rob.com.au.             7200    IN      NS      sy-dns02.tmns.net.au.
  # ;; Received 135 bytes from 139.134.2.18#53(sy-dns02.tmns.net.au) in 525 ms
  #   ;;; FINALLY, THE ANSWER!
  #  Now,DNSSEC validation is performed (unless disabled).
  class Recursor
    CACHE_LIMIT = 16_384
    ADDRESS_CACHE_LIMIT = 256
    DEFAULT_RECURSION_LIMITS = {
      timeout: 30.0, max_queries: 128, max_steps: 256, max_workers: 8
    }.freeze

    # TransmissionBudget carries this across address and validation workers.
    class Budget # :nodoc:
      attr_reader :limits, :deadline

      def initialize(limits)
        @limits = limits
        @deadline = TransportClock.now + limits[:timeout]
        @steps = @workers = 0
        @mutex = Mutex.new
      end

      def step!
        QueryControl.current&.check!
        @mutex.synchronize do
          raise RecursionLimitError, 'Recursive lookup work limit exceeded' if @steps >= @limits[:max_steps]
          @steps += 1
        end
      end

      def reserve_worker
        @mutex.synchronize do
          return false if @workers >= @limits[:max_workers]
          @workers += 1
          true
        end
      end

      def release_worker
        @mutex.synchronize { @workers -= 1 }
      end

      # Zero-TTL priming data remains usable by the lookup that received it,
      # including its address workers, but never by an independent lookup.
      def retain_hints(generation, hints)
        @mutex.synchronize { @hints = [generation, hints] }
      end

      def hints_for(generation)
        @mutex.synchronize do
          if @hints && @hints.first == generation &&
              @hints.last.any? { |_name, addresses| !addresses.current_values.empty? }
            @hints.last
          end
        end
      end

      def with_dependency(name, type, klass)
        previous = Thread.current[:dnsruby_recursion_dependencies] || []
        identity = [Name.create(name).canonical, Types.new(type).code, Classes.new(klass).code]
        return nil if previous.include?(identity)
        Thread.current[:dnsruby_recursion_dependencies] = previous + [identity]
        yield
      ensure
        Thread.current[:dnsruby_recursion_dependencies] = previous
      end
    end
    private_constant :Budget

    def recursion_limits
      @recursion_limits || DEFAULT_RECURSION_LIMITS
    end

    def recursion_limits=(settings)
      unless settings.is_a?(Hash) && (settings.keys - DEFAULT_RECURSION_LIMITS.keys).empty?
        raise ArgumentError, 'Unknown recursive lookup limit'
      end
      limits = DEFAULT_RECURSION_LIMITS.merge(settings)
      limits.each do |key, value|
        valid = if key == :timeout
          value.is_a?(Numeric) && value.finite? && value > 0
        else
          value.is_a?(Integer) && value > 0
        end
        raise ArgumentError, "Invalid recursive lookup limit #{key}" unless valid
      end
      @recursion_limits = limits.freeze
    end

    def self.current_budget
      SelectThread::TransmissionBudget.current&.context(:recursion) { nil }
    end
    private_class_method :current_budget

    def self.with_limits(resolver, limits = DEFAULT_RECURSION_LIMITS)
      parent = QueryControl.current
      parent&.check!
      transmissions = SelectThread::TransmissionBudget.current || SelectThread::TransmissionBudget.new
      budget = transmissions.context(:recursion) { Budget.new(limits) }
      transmissions.limit_total!(budget.limits[:max_queries], RecursionLimitError)
      timeout = resolver.query_timeout if resolver.respond_to?(:query_timeout)
      deadline = budget.deadline
      deadline = [deadline, TransportClock.now + timeout].min if timeout && timeout > 0
      control = QueryControl.new(deadline: deadline, parent: parent)
      SelectThread::TransmissionBudget.with(transmissions) do
        QueryControl.with(control) { yield budget }
      end
    rescue QueryControl::DeadlineExceeded
      parent&.check!
      raise ResolvTimeout, 'Recursive query timed out'
    ensure
      control&.cancel
    end
    private_class_method :with_limits

    class AddressCache # :nodoc: all
      #  Like an array, but stores the expiration of each record.
      def initialize(*args)
        @hash = Hash.new # stores addresses against their expiration
        @fresh = {} # Zero-TTL addresses belong only to the current lookup.
        @mutex = Mutex.new # This class is thread-safe
      end
      def push(item)
        address, ttl = item
        expiration = Time.now + ttl
        @mutex.synchronize {
          @hash.delete(address)
          @fresh.delete(address)
          if ttl <= 0
            @fresh[address] = true
          else
            @hash[address] = expiration
          end
          @hash.shift while @hash.length > ADDRESS_CACHE_LIMIT
          @fresh.shift while @fresh.length > ADDRESS_CACHE_LIMIT
        }
      end
      def values
        @mutex.synchronize { expire; @hash.keys }
      end
      def current_values
        @mutex.synchronize { expire; @hash.keys | @fresh.keys }
      end
      # Shared caches contain only reusable addresses. Copies also keep later
      # updates to one lookup's working map out of another lookup's view.
      def snapshot(include_current: false)
        copy = self.class.new
        @mutex.synchronize do
          expire
          copy.instance_variable_set(:@hash, @hash.dup)
          copy.instance_variable_set(:@fresh, @fresh.dup) if include_current
        end
        copy
      end
      private def expire
        now = Time.now
        @hash.delete_if { |_address, expiration| expiration <= now }
      end
      def length
        values.length
      end
      def each()
        values.each {|v|
          yield v
        }
      end
    end
    attr_accessor :nameservers, :callback, :recurse, :ipv6_ok
    attr_reader :hints, :dnssec
    #  The resolver to use for the queries
    attr_accessor :resolver

    #  For guarding access to shared caches.
    @@mutex = Mutex.new # :nodoc: all
    @@hints_mutex = Mutex.new
    @@hints_generation = 0
    @@stale_hints = nil
    @@hints = nil
    @@authority_cache = Hash.new
    @@zones_cache = nil
    # Keep delegation lifetimes independent of the A/AAAA glue lifetimes.
    # The stored Hash identity prevents stale metadata from expiring a replacement.
    @@zone_expirations = {}
    @@nameservers = nil

    def dnssec=(dnssec_on)
      @dnssec = dnssec_on
      @resolver.dnssec = dnssec_on
    end

    def initialize(res = nil)
      if (res)
        @resolver = res
      else
        nameservers = @@mutex.synchronize do
          # Hints already contain addresses. Re-resolving their owner names
          # would start separate bootstrap queries outside this lookup.
          if @@hints && !@@hints.equal?(@@stale_hints)
            @@hints.values.flat_map(&:values).uniq
          end
        end
        if nameservers && !nameservers.empty?
          @resolver = Resolver.new({:nameserver => nameservers})
        else
          @resolver = Resolver.new
        end
      end
      @resolver.dnssec = @dnssec
      @ipv6_ok = false
      @response_cache = Cache::ResolutionCache.new
    end
    # Initialize the hint servers.  Recursive queries need a starting name
    # server to work off of. This method takes a list of IP addresses to use
    # as the starting servers.  These name servers should be authoritative for
    # the root (.) zone.
    # 
    #   res.hints=(ips)
    # 
    # If no hints are passed, the default nameserver is asked for the hints.
    # Normally these IPs can be obtained from the following location:
    # 
    #   ftp://ftp.internic.net/domain/named.root
    # 
    def hints=(hints)
      Recursor.send(:with_limits, @resolver, recursion_limits) { Recursor.set_hints(hints, @resolver) }
    end
    def self.with_hints_configuration
      # Bootstrap can wait on DNS. Keep it outside the cache mutex, and allow
      # a query waiting for another bootstrap to honour its own deadline.
      until @@hints_mutex.try_lock
        QueryControl.current&.check!
        sleep 0.01
      end
      begin
        QueryControl.current&.check!
        yield
      ensure
        @@hints_mutex.unlock
      end
    end
    private_class_method :with_hints_configuration

    def self.reset_hints
      with_hints_configuration do
        @@mutex.synchronize do
          # Existing queries may still hold the previous root bootstrap.
          # Keep it readable until a replacement is ready, but require the
          # next query to refresh it and prevent old work from seeding caches.
          @@stale_hints = @@hints
          @@zones_cache = @@hints && {'.' => @@hints}
          @@nameservers = nil
          @@zone_expirations = {}
          @@authority_cache = {}
          @@hints_generation += 1
          Cache.invalidate_resolutions
        end
      end
    end

    def self.ensure_hints(resolver)
      generation = @@mutex.synchronize do
        if hints_ready?
          current_budget&.retain_hints(@@hints_generation, @@hints)
          @@hints_generation
        elsif current_budget&.hints_for(@@hints_generation)
          @@hints_generation
        end
      end
      return generation if generation
      with_hints_configuration do
        configured = @@mutex.synchronize { hints_ready? || current_budget&.hints_for(@@hints_generation) }
        configure_hints({}, resolver) unless configured
        @@mutex.synchronize { @@hints_generation }
      end
    end
    private_class_method :ensure_hints

    # Called with the cache mutex held. A retained bootstrap is usable only
    # while at least one root address is live and no reset has superseded it.
    def self.hints_ready?
      lifetime = @@zone_expirations['.']
      return false if lifetime && lifetime.first.equal?(@@hints) && lifetime[1] <= Time.now
      @@hints && @@zones_cache && !@@hints.equal?(@@stale_hints) &&
        @@hints.any? { |_name, addresses| !addresses.values.empty? }
    end
    private_class_method :hints_ready?

    # Caller holds the cache mutex. Fresh hints belong to this query's budget.
    def self.query_hints
      current_budget&.hints_for(@@hints_generation) || @@hints
    end
    private_class_method :query_hints

    def self.set_hints(hints, resolver)
      with_limits(resolver) do
        with_hints_configuration { configure_hints(hints, resolver) }
      end
    end

    def self.configure_hints(hints, resolver)
      TheLog.debug(";; hints(#{hints.inspect})\n")
      temporary_resolver = nil
      if resolver.single_resolvers.empty?
        resolver = temporary_resolver = Resolver.new
      end
      resolver.nameservers = hints if hints && !hints.empty?
      resolver.recurse = true
      budget = current_budget
      budget&.step!
      packet=resolver.query_no_validation_or_recursion(".", "NS", "IN")
      received_at = Time.now
      hints = Hash.new
      root_expiration = nil
      if (packet)
        if (ans = packet.answer)
          ans.each do |rr|
            if (rr.name.to_s =~ /^\.?$/ and
                  rr.type == Types::NS)
              #  Found root authority
              server = rr.nsdname.to_s.downcase
              server.sub!(/\.$/,"")
              TheLog.debug(";; FOUND HINT: #{server}\n")
              hints[server] = AddressCache.new
              receipt = rr.instance_variable_get(:@dnsruby_received_at)
              expiration = (receipt ? Time.at(receipt) : received_at) + rr.ttl
              root_expiration = [root_expiration, expiration].compact.min
            end
          end
          packet.additional.each { |rr| add_to_hints(hints, rr) }
          missing = hints.keys.select { |server| hints[server].current_values.empty? }
          unless missing.empty?
            # Resolve missing root addresses in bounded batches. All of the
            # queries keep the bootstrap's deadline and transmission budget.
            control = QueryControl.current
            q = control ? QueryControl::Queue.new : Queue.new
            batch_size = budget ? budget.limits[:max_workers] : DEFAULT_RECURSION_LIMITS[:max_workers]
            missing.product(['A', 'AAAA']).each_slice(batch_size) do |batch|
              query_ids = []
              begin
                batch.each do |server, type|
                  budget&.step!
                  msg = Message.new
                  msg.do_caching = @do_caching
                  msg.header.rd = false
                  msg.do_validation = false
                  msg.add_question(server, type, 'IN')
                  msg.header.cd = true if @dnssec
                  control&.check!
                  query_ids << resolver.send_async(msg, q)
                end
                query_ids.length.times do
                  _id, result, error = control ? control.pop(q) : q.pop
                  raise error if error.is_a?(RecursionLimitError)
                  result&.answer&.each { |rr| add_to_hints(hints, rr) }
                end
              ensure
                if control && (engine = resolver.instance_variable_get(:@resolver_ruby))
                  query_ids.each { |id| engine.cancel_query(id, q) }
                end
              end
            end
          end
        end
        #                       foreach my $server (keys %hints) {
        hints.keys.each do |server|
          if (!hints[server] || hints[server].current_values.empty?)
            #  Wipe the servers without lookups
            hints.delete(server)
          end
        end
      end
      raise ResolvError, 'Nameserver did not supply usable root hints' if hints.empty?

      # Publish a complete bootstrap, replacing delegations learned through the
      # old hints. An unsuccessful bootstrap leaves the previous state intact.
      @@mutex.synchronize do
        fresh_hints = hints
        @@hints = hints.transform_values(&:snapshot)
        @@stale_hints = nil
        @@hints_generation += 1
        @@nameservers = hints.keys
        @@zones_cache = {'.' => @@hints}
        @@zone_expirations = {'.' => [@@hints, root_expiration]}
        budget&.retain_hints(@@hints_generation, fresh_hints)
        @@authority_cache = {}
        Cache.invalidate_resolutions
      end
      @@nameservers
    ensure
      resolver.recurse = false if resolver
      temporary_resolver&.close
    end
    private_class_method :configure_hints


    def Recursor.add_to_hints(hints, rr)
      server = rr.name.to_s.downcase
      server.sub!(/\.$/,"")
      if (server)
        if ( rr.type == Types::A)
          # print ";; ADDITIONAL HELP: $server -> [".$rr->rdatastr."]\n" if $self->{'debug'};
          if (hints[server]!=nil)
            TheLog.debug(";; STORING IP: #{server} IN A "+rr.address.to_s+"\n")
            hints[server].push([rr.address.to_s, rr.ttl])
          end
        end
        if ( rr.type == Types::AAAA)
          # print ";; ADDITIONAL HELP: $server -> [".$rr->rdatastr."]\n" if $self->{'debug'};
          if (hints[server])
            TheLog.debug(";; STORING IP6: #{server} IN AAAA "+rr.address.to_s+"\n")
            hints[server].push([rr.address.to_s, rr.ttl])
          end
        end

      end
    end


    # This method takes a code reference, which is then invoked each time a
    # packet is received during the recursive lookup.  For example to emulate
    # dig's C<+trace> function:
    # 
    #  res.recursion_callback(Proc.new { |packet|
    #      print packet.additional.inspect
    # 
    #      print";; Received %d bytes from %s\n\n",
    #          packetanswersize,
    #          packet.answerfrom);
    #  })
    # 
    def recursion_callback=(sub)
      #           if (sub && UNIVERSAL::isa(sub, 'CODE'))
      @callback = sub
      #           end
    end

    def recursion_callback
      return @callback
    end

    def Recursor.clear_caches(resolver = nil)
      temporary_resolver = resolver.nil?
      resolver ||= Resolver.new
      Recursor.set_hints({}, resolver)
    ensure
      resolver&.close if temporary_resolver
    end

    # These helpers are called with the shared cache mutex held. Root hints
    # are retained as the recursion bootstrap; learned delegations are bounded.
    def self.cache_authority(name, addresses) # :nodoc:
      unless @@mutex.owned?
        return @@mutex.synchronize { cache_authority(name, addresses) }
      end
      generation = Thread.current[:dnsruby_recursor_generation]
      return if generation && generation != @@hints_generation
      @@authority_cache.delete(name)
      @@authority_cache[name] = addresses.is_a?(AddressCache) ? addresses.snapshot : addresses.dup
      @@authority_cache.shift while @@authority_cache.length > CACHE_LIMIT
    end

    def self.cache_zone(name, authorities, ttl, parent: nil) # :nodoc:
      unless @@mutex.owned?
        return @@mutex.synchronize { cache_zone(name, authorities, ttl, parent: parent) }
      end
      generation = Thread.current[:dnsruby_recursor_generation]
      return if generation && generation != @@hints_generation
      name = Name.create(name)
      name.absolute = true
      name = name.to_s(true).downcase
      if parent
        parent = Name.create(parent)
        parent.absolute = true
        parent = parent.to_s(true).downcase
      end
      return if name == "."
      @@zones_cache.delete(name)
      @@zone_expirations.delete(name)
      return if ttl.nil? || ttl <= 0
      expiration = Time.now + ttl
      if parent
        lifetime = @@zone_expirations[parent]
        # Retain the parent's deadline, not a reference to its cache entry.
        # LRU eviction must not invalidate descendants already learned, while
        # a descendant cannot extend its route beyond the parent admission.
        return unless lifetime && lifetime.first.equal?(@@zones_cache[parent])
        expiration = [expiration, lifetime[1]].min
        return if expiration <= Time.now
      end
      snapshot = authorities.transform_values { |addresses| addresses.is_a?(AddressCache) ? addresses.snapshot : addresses.dup }
      @@zones_cache[name] = snapshot
      @@zone_expirations[name] = [snapshot, expiration]
      while @@zones_cache.length > CACHE_LIMIT
        oldest = @@zones_cache.each_key.find { |zone| zone != "." }
        break unless oldest
        @@zones_cache.delete(oldest)
        @@zone_expirations.delete(oldest)
      end
    end

    def query_no_validation_or_recursion(name, type=Types.A, klass=Classes.IN) # :nodoc: all
      return query(name, type, klass, true)
    end

    # This method is much like the normal query() method except it disables
    # the recurse flag in the packet and explicitly performs the recursion.
    # 
    #   packet = res.query( "www.netscape.com.", "A")
    #   packet = res.query( "www.netscape.com.", "A", "IN", true) # no validation
    # 
    # The Recursor maintains a cache of known nameservers.
    # DNSSEC validation is performed unless true is passed as the fourth parameter.
    def query(name, type=Types.A, klass=Classes.IN, no_validation = false)
      Recursor.send(:with_limits, @resolver, recursion_limits) do
        if no_validation
          query_internal(name, type, klass, no_validation)
        else
          transmissions = SelectThread::TransmissionBudget.current
          budget = transmissions.context(:validation) { ValidationBudget.current || ValidationBudget.new }
          ValidationBudget.with(budget) { query_internal(name, type, klass, no_validation) }
        end
      end
    end

    def query_internal(name, type, klass, no_validation)
      previous_generation = Thread.current[:dnsruby_recursor_generation]
      QueryControl.current&.check!
      unless SelectThread::TransmissionBudget.current
        return SelectThread::TransmissionBudget.with(SelectThread::TransmissionBudget.new) do
          query_internal(name, type, klass, no_validation)
        end
      end
      #  @TODO@ PROVIDE AN ASYNCHRONOUS SEND WHICH RETURNS MESSAGE WITH ERROR!!!

      Thread.current[:dnsruby_recursor_generation] = Recursor.send(:ensure_hints, @resolver)
      @resolver.recurse = false

      query_message = Message.new(name, type, klass)
      query_message.do_validation = !no_validation
      scope = recursion_cache_scope(no_validation)
      if scope && (cached = @response_cache.find(query_message, scope))
        response, error = cached
        error.response = response if error.is_a?(ResolvError)
        raise error if error
        return response
      end

      #  So we have normal hashes, but the array of addresses at the end is now an AddressCache
      #  which respects the ttls of the A/AAAA records

      #  Now see if we already know the zone in question
      #  Otherwise, see if we know any of its parents (will know at least ".")
      authority_name = name
      if Types.new(type) == Types::DS
        # DS records belong to the parent side of a delegation (RFC 4035
        # section 3.1.4). A cached child authority cannot answer for them.
        authority_name = Name.create(name)
        authority_name = authority_name.strip_label unless authority_name.labels.empty?
      end
      known_zone, known_authorities = get_closest_known_zone_authorities_for(authority_name) # ".", @hints if nothing else

      #  Seed name servers with the closest known authority
      #       ret =  _dorecursion( name, type, klass, ".", @hints, 0)
      begin
        ret = follow_aliases(name, type, klass, known_zone, known_authorities, 0, no_validation)
        Dnssec.validate(ret) if ret && !no_validation
        if scope && scope == recursion_cache_scope(no_validation)
          @response_cache.add(query_message, scope, ret, nil,
            control: QueryControl.current, no_response: ret.nil?)
        end
        ret
      rescue StandardError => error
        if scope && scope == recursion_cache_scope(no_validation)
          failed_response = ret || (error.response if error.is_a?(ResolvError))
          @response_cache.add(query_message, scope, failed_response, error, control: QueryControl.current)
        end
        raise
      end
    ensure
      Thread.current[:dnsruby_recursor_generation] = previous_generation
    end

    private :query_internal

    # An authoritative server may finish its answer at an alias whose target
    # belongs to another zone. Continue within this lookup's budgets, retaining
    # the caller's question and every answer in the alias chain.
    def follow_aliases(name, type, klass, known_zone, authorities, depth, no_validation)
      original_name = Name.create(name)
      original_name.absolute = true
      qtype, qclass = Types.new(type), Classes.new(klass)
      current_name = original_name
      responses = []
      seen = {}
      loop do
        QueryControl.current&.check!
        begin
          packet = _dorecursion(current_name.to_s(true), type, klass, known_zone, authorities, depth, no_validation)
        rescue ResolvError => error
          if error.response && !responses.empty?
            error.response = combine_alias_responses(responses + [error.response], original_name, qtype, qclass)
          end
          raise
        end
        return nil unless packet
        responses << packet
        # Explicit alias queries and ANY request the records themselves.
        if [Types::CNAME, Types::DNAME, Types::ANY].include?(qtype) ||
            packet.rcode != RCode::NOERROR || Cache.negative_soa(packet)
          return combine_alias_responses(responses, original_name, qtype, qclass)
        end
        target, complete = alias_target(packet, current_name, qtype, qclass, seen)
        if complete || target == current_name
          return combine_alias_responses(responses, original_name, qtype, qclass)
        end
        current_name = target
        depth += 1
        authority_name = qtype == Types::DS && !current_name.labels.empty? ? current_name.strip_label : current_name
        known_zone, authorities = get_closest_known_zone_authorities_for(authority_name)
      end
    end
    private :follow_aliases

    def alias_target(packet, name, type, klass, seen)
      loop do
        QueryControl.current&.check!
        records = packet.answer.select { |rr| rr.klass == klass && rr.name == name }
        return [name, true] if records.any? { |rr| rr.type == type }
        aliases = records.select { |rr| rr.type == Types::CNAME }
        if aliases.empty?
          dnames = packet.answer.select do |rr|
            rr.type == Types::DNAME && rr.klass == klass && name.subdomain_of?(rr.name)
          end
          dname = dnames.max_by { |rr| rr.name.labels.length }
          return [name, false] unless dname
          if dnames.any? { |rr| rr.name == dname.name && rr.domainname != dname.domainname }
            raise ResolvError, 'Conflicting DNAME targets in recursive answer'
          end
          begin
            target = Name.new(name.labels.first(name.labels.length - dname.name.labels.length) +
              dname.domainname.labels, true)
          rescue ResolvError => error
            raise YXDomain, "DNAME target is too long: #{error.message}"
          end
          cname = RR.create(name: name, type: Types::CNAME, klass: klass, ttl: dname.ttl, domainname: target)
          packet.add_answer(cname)
        else
          targets = aliases.map(&:domainname).uniq
          raise ResolvError, 'Conflicting CNAME targets in recursive answer' unless targets.length == 1
          target = targets.first
        end
        Recursor.send(:current_budget)&.step!
        raise RecursionLimitError, 'Recursive alias chain is too long' if seen.length >= 255
        raise RecursionLimitError, 'Recursive alias loop' if seen[name.canonical]
        seen[name.canonical] = true
        name = target
      end
    end
    private :alias_target

    def combine_alias_responses(responses, name, type, klass)
      return responses.first if responses.length == 1
      result = responses.last.clone
      result.question.replace([Question.new(name, type, klass)])
      result.answer.replace(responses.flat_map(&:answer))
      # Earlier wildcard answers can require their own denial proofs when the
      # completed chain is verified. Keep those proofs with their signatures.
      proofs = responses[0...-1].flat_map(&:authority).select do |rr|
        [Types::NSEC, Types::NSEC3].include?(rr.type) ||
          (rr.type == Types::RRSIG && [Types::NSEC, Types::NSEC3].include?(rr.type_covered))
      end
      result.authority.concat(proofs)
      result.header.id = responses.first.header.id
      # The assembled response is no longer one authority's signed message.
      # DNSSEC is checked over the complete answer before it is returned.
      result.header.aa = result.header.ad = false
      expirations = responses.filter_map { |packet| packet.instance_variable_get(:@dnsruby_authenticated_expiration) }
      result.instance_variable_set(:@dnsruby_authenticated_expiration, expirations.min) unless expirations.empty?
      result.answerfrom = responses.last.answerfrom
      result.answerip = responses.last.answerip
      result.update_counts
      result
    end
    private :combine_alias_responses

    HOP_SETTINGS = %i[port use_tcp no_tcp tcp_pipelining tcp_pipelining_max_queries
      tcp_idle_timeout packet_timeout query_timeout retry_times retry_delay
      ignore_truncation udp_size src_address src_address6 src_port].freeze
    private_constant :HOP_SETTINGS

    def hop_settings
      HOP_SETTINGS.each_with_object({}) do |field, settings|
        next unless @resolver && @resolver.respond_to?(field)
        value = @resolver.public_send(field)
        settings[field] = value.is_a?(Array) || value.is_a?(String) ? value.dup : value
      end
    end
    private :hop_settings

    def recursion_cache_scope(no_validation)
      return unless @resolver.respond_to?(:do_caching) && @resolver.do_caching
      return if @resolver.tsig
      hints = @@mutex.synchronize do
        @@hints.map { |name, addresses| [name, addresses.values.map(&:to_s).sort] }.sort_by(&:first)
      end
      [no_validation, @dnssec, @ipv6_ok, hints, @resolver.object_id, hop_settings, recursion_limits,
        @resolver.single_resolvers.map { |sender| [sender.server.to_s.dup, sender.port] }.sort,
        Cache.validation_scope]
    end
    private :recursion_cache_scope

    def get_closest_known_zone_for(n) # :nodoc:
      if (n == nil)
        TheLog.error("Name is nil")
        raise  ResolvError.new("Nameserver invalid!")
      end
      candidate = Name.create(n)
      candidate.absolute = true
      loop do
        QueryControl.current&.check!
        name = candidate.to_s(true).downcase
        @@mutex.synchronize{
          if name == '.' && Recursor.send(:current_budget)&.hints_for(@@hints_generation)
            return name
          end
          zone = @@zones_cache[name]
          lifetime = @@zone_expirations[name]
          if lifetime && lifetime[0].equal?(zone) && lifetime[1] <= Time.now
            @@zones_cache.delete(name)
            @@zone_expirations.delete(name)
            zone = nil
          end
          if (zone != nil)
            return name
          end
        }
        return false if candidate.labels.empty?
        candidate = candidate.strip_label
      end
    end

    def get_closest_known_zone_authorities_for(name) # :nodoc:
      done = false
      known_authorities, known_zone = nil
      while (!done)
        QueryControl.current&.check!
        known_zone = get_closest_known_zone_for(name)
        #         print "GOT KNOWN ZONE : #{known_zone}\n"
        observed_authorities = nil
        @@mutex.synchronize {
          observed_authorities = known_zone == '.' ? Recursor.send(:query_hints) : @@zones_cache[known_zone]
          known_authorities = observed_authorities&.dup # isolate each recursion's working map
          known_authorities&.transform_values! do |addresses|
            addresses.is_a?(AddressCache) ? addresses.snapshot(include_current: known_zone == '.') : addresses.dup
          end
        }
        #         print "Known authorities : #{known_authorities}\n"

        #  Make sure that known_authorities still contains some authorities!
        #  If not, remove the zone from zones_cache, and start again
        if (known_authorities && known_authorities.values.length > 0)
          done = true
        else
          @@mutex.synchronize{
            if @@zones_cache[known_zone].equal?(observed_authorities)
              @@zones_cache.delete(known_zone)
              @@zone_expirations.delete(known_zone)
            end
          }
        end
      end
      return known_zone, known_authorities # @TODO@ Need to synchronize access to these!
    end

    def _dorecursion(name, type, klass, known_zone, known_authorities, depth, no_validation) # :nodoc:
      QueryControl.current&.check!
      Recursor.send(:current_budget)&.step!

      if ( depth > 255 )
        TheLog.debug(";; _dorecursion() Recursion too deep, aborting...\n")
        @errorstring="Recursion too deep, aborted"
        return nil
      end

      known_zone = known_zone.sub(/\.*$/, ".")

      ns = [] # Snapshot each address list once for this hop.
      missing_authorities = []
      @@mutex.synchronize{
        #  Get IPs from authorities
        known_authorities.keys.each do |ns_rec|
          addresses = known_authorities[ns_rec]
          cached = @@authority_cache[ns_rec]
          values = addresses.is_a?(AddressCache) ? addresses.current_values : Array(addresses)
          cached_values = if values.empty?
            cached.respond_to?(:values) ? cached.values : Array(cached)
          end
          if !values.empty?
            Recursor.cache_authority(ns_rec, known_authorities[ns_rec])
            ns.push(values)
          elsif !cached_values.empty?
            known_authorities[ns_rec] = cached.is_a?(AddressCache) ? cached.snapshot : cached.dup
            ns.push(cached_values)
          else
            @@authority_cache.delete(ns_rec)
            missing_authorities << ns_rec
          end
        end
      }

      if known_zone == '.'
        if ns.empty?
          # Root addresses can expire after query admission, including while
          # finding an address for a delegated server. Refresh the bootstrap
          # instead of recursively looking up root servers against empty hints.
          Recursor.send(:ensure_hints, @resolver)
          ns = @@mutex.synchronize { Recursor.send(:query_hints).values.map(&:current_values).reject(&:empty?) }
          raise ResolvError, 'Root hints expired before use' if ns.empty?
          # Retain this query's original generation: a refresh or reset must
          # not let earlier traversal work populate the replacement caches.
        end
        # Missing root addresses need bootstrap refresh, not hostname lookup
        # through the same root servers after their other addresses fail.
        missing_authorities.clear
      end

      if (ns.length == 0)
        found_auth = 0
        TheLog.debug(";; _dorecursion() Failed to extract nameserver IPs:")
        TheLog.debug(known_authorities.inspect + @@authority_cache.inspect)
        missing_authorities.each do |ns_rec|
          TheLog.debug(";; _dorecursion() Manual lookup for authority [#{ns_rec}]")

          # Recursive lookups must run outside the shared cache mutex.
          # Propagate cancellation and validation policy into both workers.
          control = QueryControl.new(parent: QueryControl.current)
          transmission_budget = SelectThread::TransmissionBudget.current
          hints_generation = Thread.current[:dnsruby_recursor_generation]
          hints = @@mutex.synchronize { Recursor.send(:query_hints).dup }
          workers = []
          budget = Recursor.send(:current_budget)
          dependencies = Thread.current[:dnsruby_recursion_dependencies]
          results = QueryControl::Queue.new
          ans = []
          begin
            address_types = @ipv6_ok ? ['AAAA', 'A'] : ['A']
            address_types.each do |address_type|
              lookup = lambda do
                previous_dependencies = Thread.current[:dnsruby_recursion_dependencies]
                Thread.current[:dnsruby_recursion_dependencies] = dependencies
                begin
                  answer = SelectThread::TransmissionBudget.with(transmission_budget) do
                    QueryControl.with(control) do
                      local_hints = hints.transform_values do |addresses|
                        addresses.is_a?(AddressCache) ? addresses.snapshot(include_current: true) : addresses.dup
                      end
                      recurse = -> { follow_aliases(ns_rec, address_type, klass, '.', local_hints,
                        depth + 1, no_validation) }
                      budget ? budget.with_dependency(ns_rec, address_type, klass, &recurse) : recurse.call
                    end
                  end
                  results << [answer, nil]
                rescue StandardError => error
                  results << [nil, error]
                ensure
                  Thread.current[:dnsruby_recursion_dependencies] = previous_dependencies
                end
              end
              reserved = !budget || budget.reserve_worker
              if reserved
                begin
                  workers << Thread.new(lookup) do |operation|
                    Thread.current.report_on_exception = false
                    Thread.current[:dnsruby_recursor_generation] = hints_generation
                    begin
                      operation.call
                    ensure
                      budget&.release_worker
                    end
                  end
                rescue Exception
                  budget&.release_worker
                  raise
                end
              else
                lookup.call
              end
            end
            address_types.length.times do
              answer, error = control.pop(results)
              if error
                # A failed family or nameserver must not hide a usable
                # alternative. Budget failures and cancellation still stop
                # the entire operation, as do unexpected programming errors.
                raise error if error.is_a?(VerifyError) || error.is_a?(RecursionLimitError)
                case error
                when ResolvError, ResolvTimeout, IOError, SystemCallError
                  TheLog.debug(";; _dorecursion() Address lookup failed: #{error}")
                else
                  raise error
                end
              elsif answer
                ans << answer.answer
              end
            end
          ensure
            control.cancel
            # A failed lookup must not leave its sibling running.
            workers.each { |worker| worker.join rescue nil }
          end

          if ( ans.length > 0 )
            TheLog.debug(";; _dorecursion() Answers found for [#{ns_rec}]")
            #           foreach my $rr (@ans) {
            @@mutex.synchronize do
              ans.each do |rr_arr|
                rr_arr.each do |rr|
                  TheLog.debug(";; RR:" + rr.inspect + "")
                  if (rr.type == Types::CNAME)
                    #  Follow CNAME
                    server = rr.name.to_s.downcase
                    if (server)
                      server.sub!(/\.*$/, ".")
                      if (server == ns_rec)
                        cname = rr.cname.to_s.downcase
                        cname.sub!(/\.*$/, ".")
                        TheLog.debug(";; _dorecursion() Following CNAME ns [#{ns_rec}] -> [#{cname}]")
                        if (!(known_authorities[cname]))
                          known_authorities[cname] = AddressCache.new
                        end
                        known_authorities.delete(ns_rec)
                        next
                      end
                    end
                  elsif (rr.type == Types::A || rr.type == Types::AAAA )
                    server = rr.name.to_s.downcase
                    if (server)
                      server.sub!(/\.*$/, ".")
                      if known_authorities.key?(server)
                        ip = rr.address.to_s
                        TheLog.debug(";; _dorecursion() Found ns: #{server} IN A #{ip}")
                        addresses = known_authorities[server]
                        unless addresses.is_a?(AddressCache)
                          addresses = known_authorities[server] = AddressCache.new
                        end
                        addresses.push([ip, rr.ttl])
                        Recursor.cache_authority(server, addresses)
                        found_auth+=1
                        next
                      end
                    end
                  end
                  TheLog.debug(";; _dorecursion() Ignoring useless answer: " + rr.inspect + "")
                end
              end
            end
          else
            TheLog.debug(";; _dorecursion() Could not find A records for [#{ns_rec}]")
          end
        end
        if (found_auth > 0)
          TheLog.debug(";; _dorecursion() Found #{found_auth} new NS authorities...")
          return _dorecursion(name, type, klass, known_zone, known_authorities, depth + 1, no_validation)
        end
        TheLog.debug(";; _dorecursion() No authority information could be obtained.")
        return nil
      end

      #  Cut the deck of IPs in a random place.
      TheLog.debug(";; _dorecursion() cutting deck of (" + ns.length.to_s + ") authorities...")
      splitpos = rand(ns.length)
      start = ns[0, splitpos]
      endarr = ns[splitpos, ns.length - splitpos]
      ns = endarr + start

      nameservers = []
      ns.each do |nss|
        nss.each {|n|
          Recursor.send(:current_budget)&.step!
          nameservers.push(n.to_s)
        }
      end
      # TSIG belongs to the configured upstream, not to arbitrary authorities.
      resolver = Resolver.new(hop_settings.merge(nameserver: nameservers))
      # Validation needs signatures even when requesting DNSSEC records was not
      # explicitly enabled. Non-validating lookups retain the caller's setting.
      resolver.dnssec = @dnssec || !no_validation
      resolver.recurse = false
      authority_error = nil
      begin
        #  Should construct packet ourselves and clear RD bit
        query = Message.new(name, type, klass)
        query.header.rd = false
        query.do_validation = true
        query.do_caching = false
        query.do_validation = false if no_validation
        #             print "Sending msg from resolver, dnssec = #{resolver.dnssec}, do_validation = #{query.do_validation}\n"
        packet = resolver.send_message(query)
        #  @TODO@ Now prune unrelated RRSets (RFC 5452 section 6)
        prune_rrsets_to_rfc5452(packet, known_zone)
      rescue ResolvTimeout, IOError
        #             TheLog.debug(";; nameserver #{levelns.to_s} didn't respond")
        #             next
        QueryControl.current&.check!
        TheLog.debug("No response!")
        packet = nil
      rescue ServFail, Refused, NotImp, FormErr, SocketEofResolvError => error
        raise if missing_authorities.empty?
        QueryControl.current&.check!
        authority_error = error
        packet = nil
      ensure
        resolver.close
      end
      if packet.nil? && !missing_authorities.empty?
        # A referral may contain both glue and names that need address
        # discovery. Try the latter if all immediately usable servers fail.
        unresolved = known_authorities.select { |server, _| missing_authorities.include?(server) }
        begin
          alternative = _dorecursion(name, type, klass, known_zone, unresolved, depth + 1, no_validation)
          return alternative if alternative
        rescue ResolvTimeout, IOError, ServFail, Refused, NotImp, FormErr, SocketEofResolvError => error
          QueryControl.current&.check!
          raise(authority_error || error)
        end
        raise authority_error if authority_error
        return nil
      end
      if (packet) # @TODO@ Check that the packet *is* actually authoritative!!
        if (@callback)
          @callback.call(packet)
        end

        of = nil
        TheLog.debug(";; _dorecursion() Response received from [" + @answerfrom.to_s + "]")
        status = packet.rcode
        authority = packet.authority
        if (status)
          if (status == "NXDOMAIN")
            #  I guess NXDOMAIN is the best we'll ever get
            TheLog.debug(";; _dorecursion() returning NXDOMAIN")
            return packet
          elsif (packet.answer.length > 0)
            TheLog.debug(";; _dorecursion() Answers were found.")
            return packet
          elsif (packet.header.aa)
            TheLog.debug(";; _dorecursion() Authoritative answer found")
            return packet
          elsif (authority.length > 0)
            referral = referral_authorities(packet, known_zone, name, klass)
            return nil unless referral
            of, auth, delegation_ttl = referral
            @@mutex.synchronize { Recursor.cache_zone(of, auth.dup, delegation_ttl, parent: known_zone) }
            return _dorecursion(name, type, klass, of, auth, depth + 1, no_validation)
          end
        end
      end

      return nil
    end

    # A referral advances one enclosing delegation. SOA records describe
    # negative answers; unrelated NS owners must not join this delegation.
    def referral_authorities(packet, known_zone, question_name, question_class) #:nodoc:
      parent = Name.create(known_zone)
      parent.absolute = true
      question = Name.create(question_name)
      question.absolute = true
      klass = Classes.new(question_class)
      candidates = packet.authority.select do |rr|
        rr.type == Types::NS && rr.klass == klass && rr.name.subdomain_of?(parent) &&
          (question == rr.name || question.subdomain_of?(rr.name))
      end
      delegation = candidates.max_by { |rr| rr.name.labels.length }&.name
      return unless delegation
      records = candidates.select { |rr| rr.name == delegation }
      authorities = {}
      records.each do |rr|
        server = rr.nsdname.to_s(true).downcase
        addresses = authorities[server] ||= AddressCache.new
        # RFC 9471 permits sibling glue. Trust addresses within the responding
        # parent's zone, then look up out-of-bailiwick names independently.
        glue = packet.additional.select do |address|
          [Types::A, Types::AAAA].include?(address.type) && address.klass == klass &&
            address.name == rr.nsdname &&
            (address.name == parent || address.name.subdomain_of?(parent))
        end
        # Leave missing addresses empty so _dorecursion resolves them with the
        # same transport settings, deadline and transmission budget. Passing a
        # hostname to Resolver.new would start a separate bootstrap lookup.
        glue.each { |address| addresses.push([address.address.to_s, address.ttl]) }
      end
      [delegation.to_s(true).downcase, authorities, records.map(&:ttl).min]
    end

    def prune_rrsets_to_rfc5452(packet, zone)
      #  Now prune the response of any unrelated rrsets (RFC5452 section6)
      #  "One very simple way to achieve this is to only accept data if it is
      #  part of the domain for which the query was intended."
      # if (!packet.header.aa)
      #   return
      # end
      if (!packet.question()[0])
        return
      end

      section_rrsets = packet.section_rrsets
      section_rrsets.keys.each {|section|
        section_rrsets[section].each {|rrset|
          n = Name.create(rrset.name)
          n.absolute = true
          if ((n.to_s == zone) || (n.to_s == Name.create(zone).to_s) ||
                (n.subdomain_of?(Name.create(zone))) ||
                (rrset.type == Types::OPT))
            #             # @TODO@ Leave in the response if it is an SOA, NSEC or RRSIGfor the parent zone
            # #          elsif ((query_name.subdomain_of?rrset.name) &&
            #           elsif  ((rrset.type == Types.SOA) || (rrset.type == Types.NSEC) || (rrset.type == Types.NSEC3)) #)
          else
            TheLog.debug"Removing #{rrset.name}, #{rrset.type} from response from server for #{zone}"
            packet.send(section).remove_rrset(rrset.name, rrset.type)
          end
        }
      }
    end
  end
end
