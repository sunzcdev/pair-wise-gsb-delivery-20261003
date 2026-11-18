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
module Dnsruby
# == Dnsruby::Hosts class
# Dnsruby::Hosts is a hostname resolver backed by a hosts file (defaulting to
# the system hosts location). It supports atomic hot reloads: a reload parses
# the complete file into an immutable, generation-numbered snapshot before it
# is published, so a half-written or otherwise broken file can never replace
# the currently effective mappings.
#
# === Parsing rules
# * Text from a +#+ to the end of a line is a comment; blank lines are ignored.
# * Each non-comment line must be +address canonical-name [alias ...]+.
# * Addresses must be valid IPv4 or IPv6 literals and are stored canonically
#   (via Dnsruby::IPv4/Dnsruby::IPv6). Other tokens make the whole line invalid.
# * Hostnames are validated as dot-separated LDH/underscore labels with an
#   optional trailing dot, and are normalized to lower case.
# * Lookups use the same normalization, so +Host.EXAMPLE+ and +host.example+
#   resolve identically.
#
# === Duplicate and conflict rules
# Every retained (name, address) pairing remembers the source line it came
# from and whether the name was canonical or an alias. Conflicts are recorded
# in the LoadResult and resolved deterministically:
# * an identical (name, address) pair is kept once (first declaration wins);
# * a name mapped to several addresses keeps every address, with the most
#   recent declaration first;
# * an address mapped to several names keeps every name in first-seen order;
# * differently cased spellings of the same name fold onto one normalized name.
#
# In +strict:+ mode any invalid line rejects the entire reload; otherwise the
# offending lines are skipped and reported while the valid lines are published.
#
# === class methods
# * Dnsruby::Hosts.new(hosts='/etc/hosts', strict: false, reader: ...)
#
# === lookup methods (read a fixed generation snapshot)
# * Dnsruby::Hosts#getaddress(name)
# * Dnsruby::Hosts#getaddresses(name)
# * Dnsruby::Hosts#each_address(name) {|address| ...}
# * Dnsruby::Hosts#getname(address)
# * Dnsruby::Hosts#getnames(address)
# * Dnsruby::Hosts#each_name(address) {|name| ...}
#
# === reload and inspection methods
# * Dnsruby::Hosts#reload!  -> LoadResult
# * Dnsruby::Hosts#last_load_result
# * Dnsruby::Hosts#generation
# * Dnsruby::Hosts#entries_for_name(name) / #entries_for_address(address)
# * Dnsruby::Hosts#on_reload {|load_result| ... }
  class Hosts
    if /mswin32|cygwin|mingw|bccwin/ =~ RUBY_PLATFORM
      require 'win32/resolv'
      DefaultFileName = Win32::Resolv.get_hosts_path
    else
      DefaultFileName = '/etc/hosts'
    end

    # A single retained (name, address) pairing with provenance. The same
    # immutable Entry object is reachable through both lookup directions.
    Entry = Struct.new(:name, :address, :line, :raw_line, :role, keyword_init: true) do
      def canonical?
        role == :canonical
      end

      def alias?
        role == :alias
      end
    end

    # A line that could not be parsed. +reason+ is one of the
    # +Hosts::LINE_ERROR_*+ constants.
    LineError = Struct.new(:line, :token, :reason, :raw_line, keyword_init: true)

    # A deterministic conflict resolution made while parsing. +strategy+ is
    # one of the +Hosts::CONFLICT_*+ constants.
    Conflict = Struct.new(:kind, :name, :address, :line, :strategy, keyword_init: true)

    # The inspectable outcome of a reload, whether or not it was published.
    # The +*_names+ / +*_addresses+ arrays describe the cache-invalidation
    # scope against the previously published generation.
    LoadResult = Struct.new(
      :status, :generation, :filename, :line_count, :entry_count,
      :line_errors, :conflicts,
      :added_names, :removed_names, :updated_names,
      :added_addresses, :removed_addresses, :updated_addresses,
      :rejected, :error,
      keyword_init: true
    ) do
      def published?
        status == :published
      end

      def rejected?
        !published?
      end
    end

    # Raised by the default reader when the file identity (size or mtime)
    # changes while the content is being read, so mixed content is refused.
    class HostsFileReplaced < StandardError; end

    # Conflict strategies, exposed so callers can assert on policy.
    CONFLICT_DUPLICATE_PAIR = :keep_first_declaration
    CONFLICT_NAME_REBOUND   = :last_declaration_first
    CONFLICT_ADDRESS_RENAMED = :accumulate_names
    CONFLICT_CASE_FOLDED    = :normalize_to_lowercase

    # Line-level rejection reasons.
    LINE_ERROR_INVALID_ADDRESS   = :invalid_address
    LINE_ERROR_NO_HOSTNAMES      = :no_hostnames
    LINE_ERROR_INVALID_HOSTNAME  = :invalid_hostname

    # LoadResult statuses.
    STATUS_PUBLISHED        = :published
    STATUS_REJECTED_LINES   = :rejected_in_lines
    STATUS_REPLACED         = :replaced_during_read
    STATUS_FILE_UNAVAILABLE = :file_unavailable
    STATUS_REJECTED         = :rejected

    LABEL = /[a-zA-Z0-9_](?:[a-zA-Z0-9_-]*[a-zA-Z0-9_])?/
    private_constant :LABEL
    NAME_PATTERN = /\A#{LABEL}(?:\.#{LABEL})*\.?\z/
    private_constant :NAME_PATTERN
    MAX_NAME_LENGTH = 253
    private_constant :MAX_NAME_LENGTH

    Snapshot = Struct.new(:generation, :name2entries, :addr2entries, :read_at, :signature,
      keyword_init: true)
    private_constant :Snapshot

    # Reads the raw file content and proves it did not change during the read
    # by comparing size/mtime signatures taken before and afterwards.
    DEFAULT_READER = lambda do |filename|
      before = Hosts.file_signature(filename)
      content = File.read(filename)
      after = Hosts.file_signature(filename)
      unless before == after
        raise Hosts::HostsFileReplaced,
          "#{filename} was replaced while being read; refusing mixed content"
      end
      content
    end

    def self.file_signature(filename)
      stat = File.stat(filename)
      { size: stat.size, mtime: stat.mtime.to_r }
    end

    # Canonicalizes an IP literal. Returns nil for anything that is neither a
    # valid IPv4 nor a valid IPv6 address.
    def self.normalize_address(token)
      IPv4.create(token).to_s
    rescue ArgumentError
      begin
        IPv6.create(token).to_s
      rescue ArgumentError
        nil
      end
    end

    # Validates and lower-cases a hostname token, stripping a trailing dot.
    def self.normalize_name(token)
      return nil unless token.is_a?(String) && token.length <= MAX_NAME_LENGTH
      return nil unless NAME_PATTERN.match?(token)
      labels = token.downcase.split('.')
      return nil if labels.any? { |label| label.length > 63 }
      labels.join('.')
    end

    attr_reader :filename

    # Creates a new Dnsruby::Hosts using +filename+ for its data source.
    # +reader+ is a callable taking the filename and returning the complete
    # content; the default reader also detects replacement during the read.
    # In +strict:+ mode an invalid line rejects the whole reload.
    def initialize(filename = DefaultFileName, reader: DEFAULT_READER, strict: false)
      @filename = filename
      @reader = reader
      @strict = strict
      @mutex = Mutex.new
      @hook_mutex = Mutex.new
      @reload_hooks = []
      @snapshot = empty_snapshot(0)
      @last_load_result = nil
      @initialized = nil
    end

    # Forces a reload. The file is fully parsed into a new immutable snapshot
    # before it replaces the current one; on any failure the previous
    # generation stays effective and a rejected LoadResult is returned.
    def reload!(strict: @strict, reader: @reader)
      content, status, error = read_content(reader)
      return reject(status, error) unless status == :ok

      parsed = parse_content(content)
      if strict && parsed[:line_errors].any?
        return reject(STATUS_REJECTED_LINES,
          "#{parsed[:line_errors].size} invalid line(s) in #{@filename}; strict mode keeps generation #{generation}",
          line_errors: parsed[:line_errors], conflicts: parsed[:conflicts],
          line_count: parsed[:line_count])
      end

      publish(parsed)
    end

    # The LoadResult of the most recent reload attempt, or nil before the
    # first load.
    attr_reader :last_load_result

    # The generation number currently serving queries. Rejected reloads do
    # not advance it, so every query observes one fixed, complete mapping.
    def generation
      @mutex.synchronize { @snapshot.generation }
    end

    # Registers a block called with every LoadResult after each reload
    # attempt (including rejected ones). Use the diff arrays to invalidate an
    # external cache only for the names/addresses that actually changed.
    def on_reload(&block)
      raise ArgumentError, 'on_reload requires a block' unless block
      @hook_mutex.synchronize { @reload_hooks << block }
      self
    end

    # Gets the first IP address for +name+ from the hosts file
    def getaddress(name)
      each_address(name) { |address| return address }
      raise ResolvError.new("#{@filename} has no name: #{name}")
    end

    # Gets all IP addresses for +name+ from the hosts file
    def getaddresses(name)
      ret = []
      each_address(name) { |address| ret << address }
      ret
    end

    # Iterates over all IP addresses for +name+ from one fixed snapshot
    def each_address(name)
      lazy_initialize
      snapshot = @mutex.synchronize { @snapshot }
      normalized = self.class.normalize_name(name.to_s)
      entries = normalized && snapshot.name2entries[normalized]
      entries&.each { |entry| yield entry.address }
      self
    end

    # Gets the first hostname of +address+ from the hosts file
    def getname(address)
      each_name(address) { |name| return name }
      raise ResolvError.new("#{@filename} has no address: #{address}")
    end

    # Gets all hostnames for +address+ from the hosts file
    def getnames(address)
      ret = []
      each_name(address) { |name| ret << name }
      ret
    end

    # Iterates over all hostnames for +address+ from one fixed snapshot
    def each_name(address)
      lazy_initialize
      snapshot = @mutex.synchronize { @snapshot }
      normalized = self.class.normalize_address(address.to_s)
      entries = normalized && snapshot.addr2entries[normalized]
      entries&.each { |entry| yield entry.name }
      self
    end

    # The provenance entries (Entry) that define +name+, most recent first.
    def entries_for_name(name)
      lazy_initialize
      normalized = self.class.normalize_name(name.to_s)
      snapshot = @mutex.synchronize { @snapshot }
      normalized ? (snapshot.name2entries[normalized] || []).dup : []
    end

    # The provenance entries (Entry) that define +address+, first seen first.
    def entries_for_address(address)
      lazy_initialize
      normalized = self.class.normalize_address(address.to_s)
      snapshot = @mutex.synchronize { @snapshot }
      normalized ? (snapshot.addr2entries[normalized] || []).dup : []
    end

    def lazy_initialize # :nodoc:
      do_reload = @mutex.synchronize { !@initialized }
      reload! if do_reload
      self
    end

    private

    def empty_snapshot(generation)
      Snapshot.new(generation: generation, name2entries: {}, addr2entries: {},
        read_at: nil, signature: nil)
    end

    # Default-block hashes used while parsing become plain hashes for the
    # published snapshot, so a missing-key lookup cannot try to write once
    # the snapshot is frozen.
    def plain_hash(hash)
      hash.each_with_object({}) { |(key, value), result| result[key] = value }
    end

    def read_content(reader)
      content = reader.call(@filename)
      [content, :ok, nil]
    rescue HostsFileReplaced => e
      [nil, STATUS_REPLACED, e.message]
    rescue Errno::ENOENT => e
      [nil, STATUS_FILE_UNAVAILABLE, e.message]
    rescue SystemCallError => e
      [nil, STATUS_FILE_UNAVAILABLE, e.message]
    rescue => e
      [nil, STATUS_REJECTED, "#{e.class}: #{e.message}"]
    end

    # Parses the complete content into local structures. Nothing visible to
    # queries is mutated here, so a parse failure can never publish a partial
    # mapping.
    def parse_content(content)
      name2entries = Hash.new { |hash, key| hash[key] = [] }
      addr2entries = Hash.new { |hash, key| hash[key] = [] }
      seen_pairs = {}
      first_spelling = {}
      conflicts = []
      line_errors = []

      content.each_line.with_index(1) do |raw, lineno|
        raw_line = raw.chomp
        line = raw.sub(/#.*/, '').strip
        next if line.empty?

        tokens = line.split(/\s+/)
        address = self.class.normalize_address(tokens.first)
        unless address
          line_errors << LineError.new(line: lineno, token: tokens.first,
            reason: LINE_ERROR_INVALID_ADDRESS, raw_line: raw_line)
          next
        end

        names = tokens[1..]
        if names.empty?
          line_errors << LineError.new(line: lineno, token: address,
            reason: LINE_ERROR_NO_HOSTNAMES, raw_line: raw_line)
          next
        end

        invalid = names.find { |name| self.class.normalize_name(name).nil? }
        if invalid
          line_errors << LineError.new(line: lineno, token: invalid,
            reason: LINE_ERROR_INVALID_HOSTNAME, raw_line: raw_line)
          next
        end

        names.each_with_index do |spelling, index|
          name = self.class.normalize_name(spelling)
          pair = [name, address]
          if seen_pairs.key?(pair)
            conflicts << Conflict.new(kind: :duplicate_pair, name: name,
              address: address, line: lineno, strategy: CONFLICT_DUPLICATE_PAIR)
            next
          end

          if first_spelling.key?(name) && first_spelling[name] != spelling
            conflicts << Conflict.new(kind: :case_folded, name: name,
              address: address, line: lineno, strategy: CONFLICT_CASE_FOLDED)
          end
          if name2entries.key?(name)
            conflicts << Conflict.new(kind: :name_rebound, name: name,
              address: address, line: lineno, strategy: CONFLICT_NAME_REBOUND)
          end
          if addr2entries.key?(address)
            conflicts << Conflict.new(kind: :address_renamed, name: name,
              address: address, line: lineno, strategy: CONFLICT_ADDRESS_RENAMED)
          end

          entry = Entry.new(name: name, address: address, line: lineno,
            raw_line: raw_line, role: index.zero? ? :canonical : :alias)
          seen_pairs[pair] = entry
          # Most recent declaration first, matching the historical lookup order.
          name2entries[name].unshift(entry)
          addr2entries[address] << entry
          first_spelling[name] ||= spelling
        end
      end

      { name2entries: name2entries, addr2entries: addr2entries,
        conflicts: conflicts, line_errors: line_errors,
        line_count: content.lines.size, entry_count: seen_pairs.size }
    end

    def publish(parsed)
      result = nil
      @mutex.synchronize do
        old = @snapshot
        diffs = build_diffs(old, parsed)
        snapshot = Snapshot.new(
          generation: old.generation + 1,
          name2entries: deep_freeze(plain_hash(parsed[:name2entries])),
          addr2entries: deep_freeze(plain_hash(parsed[:addr2entries])),
          read_at: Time.now,
          signature: begin
            self.class.file_signature(@filename)
          rescue SystemCallError
            nil
          end)
        @snapshot = snapshot
        result = LoadResult.new(
          status: STATUS_PUBLISHED, generation: snapshot.generation,
          filename: @filename, line_count: parsed[:line_count],
          entry_count: parsed[:entry_count],
          line_errors: parsed[:line_errors].freeze,
          conflicts: parsed[:conflicts].freeze,
          rejected: false, error: nil, **diffs)
        @last_load_result = result
        @initialized = true
      end
      notify_hooks(result)
      result
    end

    def reject(status, error, line_errors: [], conflicts: [], line_count: 0)
      result = nil
      @mutex.synchronize do
        retained = @snapshot
        TheLog.warn("Dnsruby::Hosts reload rejected (#{status}); keeping generation #{retained.generation}: #{error}")
        result = LoadResult.new(
          status: status, generation: retained.generation, filename: @filename,
          line_count: line_count, entry_count: count_entries(retained),
          line_errors: line_errors.dup.freeze, conflicts: conflicts.dup.freeze,
          added_names: [].freeze, removed_names: [].freeze, updated_names: [].freeze,
          added_addresses: [].freeze, removed_addresses: [].freeze,
          updated_addresses: [].freeze,
          rejected: true, error: error)
        @last_load_result = result
        @initialized = true
      end
      notify_hooks(result)
      result
    end

    def count_entries(snapshot)
      snapshot.name2entries.values.flatten.uniq { |entry| [entry.name, entry.address] }.size
    end

    def build_diffs(old, parsed)
      name_diffs = diff_arrays(old.name2entries, parsed[:name2entries], :address)
      addr_diffs = diff_arrays(old.addr2entries, parsed[:addr2entries], :name)
      {
        added_names: name_diffs[:added].freeze,
        removed_names: name_diffs[:removed].freeze,
        updated_names: name_diffs[:updated].freeze,
        added_addresses: addr_diffs[:added].freeze,
        removed_addresses: addr_diffs[:removed].freeze,
        updated_addresses: addr_diffs[:updated].freeze
      }
    end

    def diff_arrays(old_map, new_map, value_field)
      added = []
      removed = []
      updated = []
      (old_map.keys | new_map.keys).sort.each do |key|
        old_values = old_map.key?(key) ? old_map[key].map(&value_field) : []
        new_values = new_map.key?(key) ? new_map[key].map(&value_field) : []
        if old_values.empty?
          added << key
        elsif new_values.empty?
          removed << key
        elsif old_values != new_values
          updated << key
        end
      end
      { added: added, removed: removed, updated: updated }
    end

    def deep_freeze(value)
      case value
      when Hash
        value.each { |key, entry| deep_freeze(key); deep_freeze(entry) }
        value.freeze
      when Array
        value.each { |entry| deep_freeze(entry) }
        value.freeze
      when Entry
        value.raw_line.freeze
        value.freeze
      when String
        value.freeze
      else
        value.freeze
      end
      value
    end

    def notify_hooks(result)
      hooks = @hook_mutex.synchronize { @reload_hooks.dup }
      hooks.each { |hook| hook.call(result) }
    rescue => e
      TheLog.warn("Dnsruby::Hosts reload hook failed: #{e.class}: #{e.message}")
    end
  end
end
