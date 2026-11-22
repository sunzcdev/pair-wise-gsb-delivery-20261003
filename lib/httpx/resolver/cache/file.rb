# frozen_string_literal: true

require "pstore"
require "tmpdir"
require "set"

module HTTPX
  module Resolver::Cache
    # Implementation of a file resolver cache.
    #
    # The cache file is loaded in memory on first access, and reads are served from this
    # in-memory view (i.e. the file is not reloaded on every read). Mutations track the
    # keys touched by this process ("dirty" keys), and only those are merged back into
    # the file on save. This way, a process sharing the cache file with others does not
    # overwrite their entries with its own (possibly stale) in-memory view.
    class File < Base
      # default path where the resolver cache is stored. It's versioned, as the file may
      # change format in-between releases, and it'd signal it as corrupted.
      DEFAULT_PATH = ::File.join(Dir.tmpdir, "httpx-ruby-#{VERSION}.cache")

      # errors raised when the cache file can't be unmarshalled, i.e. it's corrupted.
      CORRUPTED_FILE_ERRORS = [PStore::Error, TypeError, ArgumentError].freeze

      def initialize(path = DEFAULT_PATH)
        super()
        @store = PStore.new(path, true)
        @lookups = nil
        @hostnames = nil
        @dirty = Set.new
        @mutex = Thread::Mutex.new
      end

      def get(hostname)
        @mutex.synchronize do
          load_cache

          now = Utils.now

          had_entry = @lookups.key?(hostname)

          _get(hostname, @lookups, @hostnames, now).tap do
            # persist expiration-driven removals
            next unless had_entry && !@lookups.key?(hostname)

            @dirty << hostname
            save
          end
        end
      end

      def set(hostname, family, entries)
        @mutex.synchronize do
          load_cache

          evicted = []

          _set(hostname, family, entries, @lookups, @hostnames, evicted: evicted)

          @dirty << hostname
          entries.each { |entry| @dirty << entry["name"] }
          @dirty.merge(evicted)

          save
        end
      end

      def evict(hostname, ip)
        ip = ip.to_s

        @mutex.synchronize do
          load_cache

          _evict(hostname, ip, @lookups, @hostnames)

          @dirty << hostname

          save
        end
      end

      private

      # loads the whole cache file in memory. A corrupted file is treated as empty,
      # and will be reset on the next save.
      def load_cache
        return if @lookups

        @store.transaction(true) do
          @lookups = @store[:lookups] || {}
          @hostnames = @store[:hostnames] || []
        end
      rescue *CORRUPTED_FILE_ERRORS
        @lookups = {}
        @hostnames = []
      end

      # merges only the keys changed by this process back into the cache file, leaving
      # entries written by other processes untouched. Keys which are not in the
      # in-memory view anymore (e.g. expired or evicted entries) are removed from
      # the file.
      def save
        return if @dirty.empty?

        reset_corrupted = false

        begin
          @store.transaction do
            lookups = @store[:lookups] || {}
            hostnames = @store[:hostnames] || []

            @dirty.each do |hostname|
              entries = @lookups[hostname]

              if entries.nil? || entries.empty?
                lookups.delete(hostname)
                hostnames.delete(hostname)
              else
                lookups[hostname] = entries
                hostnames << hostname unless hostnames.include?(hostname)
              end
            end

            @store[:lookups] = lookups
            @store[:hostnames] = hostnames
          end

          @dirty.clear
        rescue *CORRUPTED_FILE_ERRORS
          raise if reset_corrupted

          # the cache file got corrupted; reset it and try one more time.
          reset_corrupted = true
          ::File.open(@store.path, "w") {}
          retry
        end
      end
    end
  end
end
