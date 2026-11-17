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

require 'dnsruby/serial_time'

module Dnsruby
  class KeyCache #:nodoc: all
    #  Cache includes expiration time for keys
    #  Cache removes expired records
    def initialize(keys = nil, max_keys: nil)
      # Store DNSKEY identity against [expiry, key]. A 16-bit key tag can collide.
      @keys = {}
      @candidate_index = {}
      @key_candidates = {}
      @name_index = {}
      raise ArgumentError, 'max_keys must be a positive integer' if max_keys && (!max_keys.is_a?(Integer) || max_keys <= 0)
      @max_keys = max_keys
      add(keys)
    end

    # TTL and cached OpenSSL objects are not part of a DNSKEY's identity.
    # Copy mutable fields so later changes cannot corrupt the hash index.
    def self.key_identity(key)
      [key.name.canonical.freeze, key.klass.code, key.type.code,
       key.flags, key.protocol, key.algorithm.code, key.key.dup.freeze].freeze
    end
    def add_key_with_expiration(k, expiration)
      priv_add_key(k, expiration)
    end
    def add(k, expiration = nil)
      if (k == nil)
        return false
      elsif (k.instance_of?RRSet)
        add_rrset(k, expiration)
      elsif (k.kind_of?KeyCache)
        k.keys_and_expirations.each { |expiration, key|
          priv_add_key(key, expiration)
        }
      else
        raise ArgumentError.new("Expected an RRSet or KeyCache! Got #{k.class}")
      end
      return true
    end

    def add_rrset(k, expiration = nil)
      return unless k.type == Types.DNSKEY
      now = Time.now.to_i
      # Verification supplies an absolute deadline derived from the signature
      # that actually verified. Never extend it using other supplied RRSIGs.
      # Explicit callers without that deadline get a conservative TTL bound.
      unless expiration
        signatures = k.sigs.select { |sig| SerialTime.valid?(sig.inception, sig.expiration, now) }
        return if signatures.empty?
        expiration = ([now + k.ttl] + signatures.map { |sig| SerialTime.absolute(sig.expiration, now) }).min
      end
      return if expiration <= now
      k.rrs.each { |rr| priv_add_key(rr, expiration) }
    end

    # A tag identifies a bucket, not a unique key. Keep every colliding key.
    def self.candidate_identity(name, klass, algorithm, tag)
      [name.canonical.freeze, klass.code, algorithm.code, tag].freeze
    end

    def priv_add_key(k, exp)
      identity = self.class.key_identity(k)
      previous = @keys[identity]
      return if previous && previous[0] >= exp
      if @max_keys && !previous && @keys.length >= @max_keys
        remove_expired_keys
        delete_identity(@keys.each_key.first) while @keys.length >= @max_keys
      end
      @keys[identity] = [exp, k]
      candidate = self.class.candidate_identity(k.name, k.klass, k.algorithm, k.key_tag)
      (@candidate_index[candidate] ||= {})[identity] = true
      @key_candidates[identity] = candidate
      (@name_index[identity.first] ||= {})[identity] = true
    end

    def matching_keys(signatures)
      identities = signatures.flat_map do |sig|
        candidate = self.class.candidate_identity(sig.signers_name, sig.klass, sig.algorithm, sig.key_tag)
        (@candidate_index[candidate] || {}).keys
      end.uniq
      now = Time.now.to_i
      identities.filter_map do |identity|
        expiration, key = @keys.fetch(identity)
        if expiration <= now
          delete_identity(identity)
          nil
        else
          key
        end
      end
    end

    def keys_for_name(name)
      now = Time.now.to_i
      (@name_index[name.canonical] || {}).keys.filter_map do |identity|
        expiration, key = @keys.fetch(identity)
        if expiration <= now
          delete_identity(identity)
          nil
        else
          key
        end
      end
    end

    def include_identity?(identity)
      @keys.key?(identity)
    end

    def each
      #  Only offer currently-valid keys here
      remove_expired_keys
      @keys.values.each {|v| yield v[1]}
    end
    def delete(key)
      delete_identity(self.class.key_identity(key))
    end
    def delete_identity(identity)
      cached = @keys.delete(identity)
      return unless cached
      key = cached[1]
      names = @name_index.fetch(identity.first)
      names.delete(identity)
      @name_index.delete(identity.first) if names.empty?
      candidate = @key_candidates.delete(identity)
      bucket = @candidate_index[candidate]
      if bucket
        bucket.delete(identity)
        @candidate_index.delete(candidate) if bucket.empty?
      end
      key
    end
    def keys
      #  Only offer currently-valid keys here
      remove_expired_keys
      ks = []
      @keys.values.each {|a| ks.push(a[1])}
      return ks
      #         return @keys.keys
    end
    def keys_and_expirations
      remove_expired_keys
      @keys.values.map(&:dup)
    end
    def remove_expired_keys
      now = Time.now.to_i
      @keys.each { |identity, (expiration, _key)| delete_identity(identity) if expiration <= now }
    end
    def find_key_for(name)
      each {|key| return key if key.name == name}
      return false
    end
  end

  # Learned DS/DLV RRsets are evicted as a whole. The record limit bounds both
  # retained records and index entries, even when an RRset contains many DSes.
  class DSCache # :nodoc: all
    def initialize(max_records:)
      if max_records && (!max_records.is_a?(Integer) || max_records <= 0)
        raise ArgumentError, 'max_records must be a positive integer'
      end
      @max_records = max_records
      @record_count = 0
      @sets = {}
      @candidate_index = {}
      @name_index = {}
    end

    def self.set_identity(rrset)
      [rrset.name.canonical.freeze, rrset.klass.code, rrset.type.code].freeze
    end

    def self.candidate_identity(record)
      # DLV owners belong to a lookaside registry, not the DNSKEY's zone.
      [record.type == Types.DLV ? nil : record.name.canonical.freeze,
        record.klass.code, record.algorithm.code, record.key_tag].freeze
    end

    def add(rrset, expiration)
      identity = self.class.set_identity(rrset)
      delete(identity)
      count = rrset.rrs.length + rrset.sigs.length
      return if expiration <= Time.now.to_i || (@max_records && count > @max_records)
      remove_expired if @max_records && @record_count + count > @max_records
      delete(@sets.each_key.first) while @max_records && @record_count + count > @max_records
      candidates = rrset.rrs.map { |record| self.class.candidate_identity(record) }.uniq
      @sets[identity] = [expiration, rrset, count, candidates]
      @record_count += count
      (@name_index[identity.first] ||= {})[identity] = true
      candidates.each { |candidate| (@candidate_index[candidate] ||= {})[identity] = true }
    end

    def matching_rrsets(keys)
      identities = keys.flat_map do |key|
        [key.name.canonical, nil].flat_map do |owner|
          (@candidate_index[[owner, key.klass.code, key.algorithm.code, key.key_tag]] || {}).keys
        end
      end.uniq
      now = Time.now.to_i
      identities.filter_map do |identity|
        expiration, rrset = @sets.fetch(identity)
        if expiration <= now
          delete(identity)
          nil
        else
          rrset
        end
      end
    end

    def rrsets_for_name(name)
      now = Time.now.to_i
      (@name_index[name.canonical] || {}).keys.filter_map do |identity|
        expiration, rrset = @sets.fetch(identity)
        if expiration <= now
          delete(identity)
          nil
        else
          rrset
        end
      end
    end

    def rrsets
      remove_expired
      @sets.values.map { |entry| entry[1] }
    end

    private

    def remove_expired
      now = Time.now.to_i
      @sets.each { |identity, entry| delete(identity) if entry[0] <= now }
    end

    def delete(identity)
      entry = @sets.delete(identity)
      return unless entry
      @record_count -= entry[2]
      names = @name_index.fetch(identity.first)
      names.delete(identity)
      @name_index.delete(identity.first) if names.empty?
      entry[3].each do |candidate|
        bucket = @candidate_index.fetch(candidate)
        bucket.delete(identity)
        @candidate_index.delete(candidate) if bucket.empty?
      end
    end
  end

end
