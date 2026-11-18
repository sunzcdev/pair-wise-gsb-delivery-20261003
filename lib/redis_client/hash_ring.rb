# frozen_string_literal: true

require 'zlib'

class RedisClient
  # Consistent hashing ring with explicit per-node weights and a read-only
  # keyspace migration report.
  #
  # The layout (which node owns each point of the 32 bit keyspace) is a pure
  # function of:
  #
  #   * each node's stable identity (`#id`, falling back to the server URL),
  #   * the per-node weight (number of virtual node replicas),
  #   * `replicas`, the number of virtual points per unit of weight,
  #   * `digest`, the hash algorithm used to place virtual points,
  #   * `seed`, an optional value mixed into the virtual point keys.
  #
  # Ruby's process randomized `Hash` is never used to place points, and the
  # nodes may be supplied in any order, so a given configuration always
  # produces the same layout across restarts and processes.
  class HashRing
    POINTS_PER_SERVER = 160

    # The key hash (`Zlib.crc32`) and the virtual point hash both live in a
    # 32 bit keyspace.
    KEYSPACE = 1 << 32

    DEFAULT_WEIGHT = 1

    class << self
      attr_writer :digest

      def digest
        @digest ||= begin
          require 'digest/md5'
          Digest::MD5
        end
      end

      # Compare two rings without modifying either. Equivalent to
      # `source.migration_plan(target)`.
      def migration_plan(source, target)
        source.migration_plan(target)
      end
      alias_method :compare, :migration_plan
    end

    # A contiguous slice of the keyspace whose owner changes between two
    # layouts. `start` is inclusive and `finish` is exclusive. `finish` may be
    # greater than `KEYSPACE` when the slice wraps past the end of the keyspace.
    #
    # This describes keyspace ranges only: it is a plan, not a record of keys
    # that were read, moved or deleted.
    class MigrationInterval
      attr_reader :start, :finish, :from, :to, :from_node, :to_node, :keyspace

      def initialize(start:, finish:, from:, to:, from_node:, to_node:, from_removed: false, to_added: false,
        keyspace: KEYSPACE)
        @start = start
        @finish = finish
        @from = from
        @to = to
        @from_node = from_node
        @to_node = to_node
        @from_removed = from_removed
        @to_added = to_added
        @keyspace = keyspace
        freeze
      end

      # Number of hashes (and therefore keyspace positions) covered.
      def size
        @finish - @start
      end

      def fraction
        size.fdiv(@keyspace)
      end

      def wraps?
        @finish > @keyspace
      end

      # The destination node does not exist in the source layout.
      def added?
        @to_added
      end

      # The source node does not exist in the target layout. In the target
      # layout this range has a deterministic successor recorded in `#to`.
      def removed?
        @from_removed
      end

      def relocated?
        !added? && !removed?
      end

      # One or two non-wrapping `[start, finish)` slices.
      def slices
        slices = wraps? ? [[@start, @keyspace], [0, @finish - @keyspace]] : [[@start, @finish]]
        slices.reject { |start_at, finish_at| finish_at <= start_at }
      end

      def covers?(hash)
        hash = Integer(hash) % @keyspace
        if wraps?
          hash >= @start || hash < @finish - @keyspace
        else
          hash >= @start && hash < @finish
        end
      end

      def to_h
        {
          from: @from,
          to: @to,
          start: @start,
          finish: @finish,
          size: size,
          fraction: fraction,
          wraps: wraps?,
          added: added?,
          removed: removed?,
        }
      end

      def ==(other)
        other.is_a?(MigrationInterval) && to_h == other.to_h
      end
      alias_method :eql?, :==

      def hash
        to_h.hash
      end

      def inspect
        "#<#{self.class.name} #{@from || "(none)"} -> #{@to || "(none)"} keyspace=[#{@start}, #{@finish})>"
      end
    end

    # The result of comparing two rings. It lists the keyspace intervals whose
    # owner differs, grouped by source and destination node.
    #
    # It never reads or moves keys, and it does not modify either ring.
    class MigrationPlan
      include Enumerable

      NOTE = "Describes keyspace ranges whose owner changes; no keys are read, moved or deleted."

      attr_reader :source, :target, :intervals

      def self.build(source, target)
        source_points = source.keyspace_points
        target_points = target.keyspace_points
        boundaries = (source_points | target_points).sort
        return new(source, target, []) if boundaries.empty?

        source_ids = source.node_ids.to_h { |id| [id, true] }
        target_ids = target.node_ids.to_h { |id| [id, true] }
        raw = []
        boundaries.each_with_index do |point, index|
          finish = if index + 1 < boundaries.length
            boundaries[index + 1]
          else
            boundaries[0] + KEYSPACE
          end

          from_node = source.node_for_hash(point)
          to_node = target.node_for_hash(point)
          from = source.node_id_of(from_node)
          to = target.node_id_of(to_node)
          next if from == to

          raw << MigrationInterval.new(
            start: point,
            finish: finish,
            from: from,
            to: to,
            from_node: from_node,
            to_node: to_node,
            from_removed: !from.nil? && !target_ids.key?(from),
            to_added: !to.nil? && !source_ids.key?(to),
          )
        end

        new(source, target, merge_intervals(raw))
      end

      # Adjacent slices with the same source/destination pair are coalesced,
      # including the wrap-around seam, so reports stay readable.
      def self.merge_intervals(intervals)
        return intervals.freeze if intervals.empty?

        merged = []
        intervals.each do |interval|
          previous = merged.last
          if previous && previous.finish == interval.start &&
             previous.from == interval.from && previous.to == interval.to
            merged[-1] = coalesce(previous, interval)
          else
            merged << interval
          end
        end

        # Fold the wrap-around seam, including any further intervals that
        # become adjacent to the merged range.
        loop do
          break if merged.length < 2

          first = merged.first
          last = merged.last
          break unless last.finish == first.start + KEYSPACE &&
            last.from == first.from && last.to == first.to

          merged.pop
          merged.shift
          merged << coalesce(last, first, finish: first.finish + KEYSPACE)
        end

        merged.freeze
      end

      def self.coalesce(previous, following, finish: following.finish)
        MigrationInterval.new(
          start: previous.start,
          finish: finish,
          from: previous.from,
          to: previous.to,
          from_node: previous.from_node,
          to_node: previous.to_node,
          from_removed: previous.removed?,
          to_added: previous.added?,
        )
      end

      def initialize(source, target, intervals)
        @source = source
        @target = target
        @intervals = intervals.freeze
        freeze
      end

      def each(&block)
        return enum_for(:each) unless block

        @intervals.each(&block)
      end

      def empty?
        @intervals.empty?
      end

      def size
        @intervals.size
      end
      alias_method :length, :size

      def changed_keyspace
        @intervals.sum(&:size)
      end
      alias_method :changed_keyspaces, :changed_keyspace

      def changed_fraction
        changed_keyspace.fdiv(KEYSPACE)
      end

      def unchanged_fraction
        1.0 - changed_fraction
      end

      # Nodes that only exist in the target layout.
      def added_nodes
        @intervals.select(&:added?).map(&:to).uniq.sort
      end

      # Nodes that only existed in the source layout. Their keyspace is handed
      # over to the deterministic successors listed in #successors.
      def removed_nodes
        @intervals.select(&:removed?).map(&:from).uniq.sort
      end

      # Map of source node id to the sorted destination ids that take over part
      # of its keyspace.
      def successors
        result = {}
        @intervals.each do |interval|
          next unless interval.from && interval.to

          (result[interval.from] ||= []) << interval.to
        end
        result.each_with_object({}) { |(id, ids), hash| hash[id] = ids.uniq.sort }
      end

      def to_h
        {
          keyspace: KEYSPACE,
          changed_keyspace: changed_keyspace,
          changed_fraction: changed_fraction,
          unchanged_fraction: unchanged_fraction,
          note: NOTE,
          intervals: @intervals.map(&:to_h),
        }
      end

      def describe
        lines = []
        lines << "HashRing migration plan over a #{KEYSPACE} position keyspace"
        lines << format("changed keyspace: %d positions (%.2f%%)", changed_keyspace, changed_fraction * 100)
        lines << NOTE
        @intervals.each do |interval|
          lines << format(
            "  [%d, %d) %s -> %s (%.2f%%)",
            interval.start,
            interval.finish,
            interval.from || "(none)",
            interval.to || "(none)",
            interval.fraction * 100,
          )
        end
        lines << "  (no changes)" if @intervals.empty?
        lines.join("\n")
      end

      def to_s
        describe
      end

      def ==(other)
        other.is_a?(MigrationPlan) && to_h == other.to_h
      end
      alias_method :eql?, :==

      def hash
        to_h.hash
      end

      def inspect
        "#<#{self.class.name} intervals=#{size} changed_fraction=#{changed_fraction}>"
      end
    end

    attr_reader :nodes, :seed, :weights

    def initialize(nodes = [], replicas: POINTS_PER_SERVER, digest: self.class.digest, seed: nil, weights: nil)
      @replicas = replicas
      unless @replicas.is_a?(Integer) && @replicas > 0
        raise ArgumentError, "replicas must be a positive Integer, got: #{replicas.inspect}"
      end

      @digest = normalize_digest(digest)
      @seed = normalize_seed(seed)
      @nodes = nodes.dup.freeze
      @node_lookup = build_node_lookup(@nodes)

      specs = build_specs(@nodes, weights)

      @weight_by_id = specs.each_with_object({}) { |(id, weight, _node), hash| hash[id] = weight }.freeze
      @weights = @weight_by_id.sort.to_h.freeze
      build_ring(specs)
    end

    # Get the node in the hash ring for this key.
    def node_for(key)
      return nil if @sorted_keys.empty?

      idx = binary_search(@sorted_keys, hash_for(key))
      @ring[@sorted_keys[idx]]
    end

    # Stable identity of the node owning this key, or nil for an empty ring.
    def node_id_for(key)
      node = node_for(key)
      node && node_id_of(node)
    end

    def nodes_for(*keys)
      keys.flatten!
      mapping = {}
      keys.each do |key|
        (mapping[node_for(key)] ||= []) << key
      end
      mapping
    end

    # Read-only comparison against another ring. Returns a MigrationPlan that
    # lists the keyspace intervals whose owner differs. Neither ring is
    # modified and no keys are read or moved.
    def migration_plan(target)
      unless target.is_a?(HashRing)
        raise ArgumentError, "expected a #{self.class}, got: #{target.class}"
      end

      MigrationPlan.build(self, target)
    end
    alias_method :compare, :migration_plan
    alias_method :migration_report, :migration_plan

    # Deterministic digest of the point-to-node layout, independent of the
    # order in which nodes were supplied. Useful to verify that two ring
    # configurations produce the same layout.
    def layout_fingerprint
      require 'digest/sha1'
      material = @sorted_keys.map { |point| "#{point}=#{node_id_of(@ring[point])}" }.join("\n")
      Digest::SHA1.hexdigest(material)
    end

    # Frozen sorted list of virtual point positions.
    def keyspace_points
      @sorted_keys
    end

    # Node owning a raw 32 bit position, or nil for an empty ring.
    def node_for_hash(hash)
      return nil if @sorted_keys.empty?

      @ring[@sorted_keys[binary_search(@sorted_keys, hash)]]
    end

    # Stable identity of the node owning a raw 32 bit position.
    def node_id_for_hash(hash)
      node = node_for_hash(hash)
      node && node_id_of(node)
    end

    # Stable identity of a node belonging to this ring.
    def node_id_of(node)
      return nil if node.nil?

      @id_by_node[node]
    end

    # Sorted list of node identities in this ring.
    def node_ids
      @weights.keys
    end

    # Map of node identity to node.
    def nodes_by_id
      @nodes_by_id
    end

    # Weight of a node, by identity, id or node object. Returns nil for a node
    # that isn't part of this ring.
    def weight_for(node)
      id = case node
      when String, Symbol then node.to_s
      else
        node_id_of(node) || begin
          if node.is_a?(Hash)
            node[:id] || node["id"] || node[:server_url] || node["server_url"]
          elsif node.respond_to?(:id) && !node.id.nil?
            node.id
          end
        end&.to_s
      end
      @weight_by_id[id]
    end

    private

    def normalize_digest(digest)
      digest = self.class.digest if digest.nil?
      return digest if digest.respond_to?(:digest)

      raise ArgumentError, "digest must respond to #digest, got: #{digest.inspect}"
    end

    def normalize_seed(seed)
      return nil if seed.nil?

      seed = seed.to_s
      return nil if seed.empty?

      seed.freeze
    end

    # Index nodes by identity and by object so nodes may be referenced by id,
    # symbol, or the node itself, and lookup doesn't depend on equality.
    def build_node_lookup(nodes)
      lookup = { by_identity: {}, by_object: {}.compare_by_identity }
      nodes.each do |node|
        id = node_identity(node)
        if lookup[:by_identity].key?(id)
          raise ArgumentError, "duplicate node id: #{id.inspect}"
        end

        lookup[:by_identity][id] = node
        lookup[:by_object][node] = id
      end
      lookup
    end

    def build_specs(nodes, weights)
      explicit = normalize_weights(weights)
      specs = []
      nodes.each do |node|
        id = @node_lookup[:by_object][node]
        weight = resolve_weight(node, id, explicit)
        validate_weight!(id, weight)
        specs << [id, weight, node]
      end

      unknown = explicit.keys.reject { |id| @node_lookup[:by_identity].key?(id) }
      unless unknown.empty?
        raise ArgumentError, "weights given for unknown node(s): #{unknown.map(&:inspect).join(", ")}"
      end

      specs
    end

    # Accepts a Hash of node id (String or Symbol) or node object to weight.
    # Also accepts a parallel Array of weights, one per node in order.
    def normalize_weights(weights)
      return {} if weights.nil?

      if weights.is_a?(Array)
        if weights.size != @nodes.size
          raise ArgumentError, "weights count #{weights.size} does not match node count #{@nodes.size}"
        end

        return @nodes.each_with_index.to_h { |node, index| [node_identity(node), weights[index]] }
      end

      unless weights.is_a?(Hash)
        raise ArgumentError, "weights must be a Hash of node id => positive Integer, got: #{weights.inspect}"
      end

      normalized = {}
      weights.each do |key, weight|
        id = case key
        when String then key
        when Symbol then key.to_s
        else
          known = @node_lookup[:by_object][key]
          if known
            known
          elsif node_like?(key)
            # Accept an equivalent node instance; the unknown-node check below
            # still rejects ids that aren't part of this ring.
            node_identity(key)
          else
            raise ArgumentError, "unknown node in weights: #{key.inspect}"
          end
        end

        if normalized.key?(id)
          raise ArgumentError, "duplicate weight for node: #{id.inspect}"
        end

        normalized[id] = weight
      end
      normalized
    end

    def node_like?(object)
      object.is_a?(Hash) || object.respond_to?(:id) || object.respond_to?(:config)
    end

    def resolve_weight(node, id, explicit)
      return explicit[id] if explicit.key?(id)

      if node.respond_to?(:weight)
        weight = node.weight
        return weight unless weight.nil?
      end

      DEFAULT_WEIGHT
    end

    def validate_weight!(id, weight)
      unless weight.is_a?(Integer)
        raise ArgumentError, "weight for node #{id.inspect} must be a positive Integer, got: #{weight.inspect}"
      end
      if weight <= 0
        raise ArgumentError, "weight for node #{id.inspect} must be greater than zero, got: #{weight.inspect}"
      end
    end

    def node_identity(node)
      if node.is_a?(String) || node.is_a?(Symbol)
        raise ArgumentError, "node must respond to #id or #config, got: #{node.inspect}"
      end

      id = node.id if node.respond_to?(:id)
      if id.nil? && node.is_a?(Hash)
        id = node[:id] || node["id"]
      end
      if id.nil? && node.respond_to?(:config)
        config = node.config
        id = config.server_url if config.respond_to?(:server_url)
      end
      if id.nil? && node.is_a?(Hash)
        id = node[:server_url] || node["server_url"]
      end
      if id.nil?
        raise ArgumentError, "node has no stable identity: #{node.inspect}"
      end

      id.to_s
    end

    def build_ring(specs)
      # Sorting by [point, id] before insertion makes the layout independent of
      # the order in which nodes were supplied, including the (unlikely) case
      # of two virtual points landing on the same position.
      entries = []
      specs.each do |id, weight, node|
        (@replicas * weight).times do |index|
          entries << [server_hash_for(virtual_node_key(id, index)), id, node]
        end
      end
      entries.sort_by! { |point, id, _node| [point, id] }

      ring = {}
      entries.each do |point, _id, node|
        ring[point] ||= node
      end

      @id_by_node = {}.compare_by_identity
      @nodes_by_id = {}
      specs.each do |id, _weight, node|
        @id_by_node[node] = id
        @nodes_by_id[id] = node
      end
      @id_by_node.freeze
      @nodes_by_id.freeze

      @ring = ring.freeze
      @sorted_keys = ring.keys.sort.freeze
    end

    # The default (no seed) key is byte-for-byte identical to the historical
    # equal-weight layout, so existing rings keep their distribution.
    def virtual_node_key(id, index)
      if @seed
        "#{@seed}\0#{id}:#{index}"
      else
        "#{id}:#{index}"
      end
    end

    def hash_for(key)
      Zlib.crc32(key)
    end

    def server_hash_for(key)
      @digest.digest(key).unpack1("L>")
    end

    # Find the closest index in HashRing with value <= the given value
    def binary_search(ary, value)
      upper = ary.size
      lower = 0

      while lower < upper
        mid = (lower + upper) / 2
        if ary[mid] > value
          upper = mid
        else
          lower = mid + 1
        end
      end

      upper - 1
    end
  end
end
