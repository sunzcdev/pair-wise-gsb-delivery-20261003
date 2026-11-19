# frozen_string_literal: true
require "digest"
require "json"
require "fileutils"

module Rake

  # Stores content fingerprints for file tasks that run in fingerprint mode.
  #
  # A manifest is a small JSON document mapping a task name to a single
  # successful build entry:
  #
  #   {
  #     "version": 1,
  #     "entries": {
  #       "build/app.o": {
  #         "scope": ["src/app.c", "include/app.h"],
  #         "inputs": { "src/app.c": "…sha256…", "include/app.h": "…sha256…" },
  #         "parameters": "…sha256 of the normalized parameters…",
  #         "output": { "digest": "…sha256…", "size": 1234 }
  #       }
  #     }
  #   }
  #
  # The +scope+ list holds only the declared input paths, so a change to the
  # declaration itself is detected as well.  Only digests are persisted.  Parameter values (and therefore anything a
  # Rakefile derives from the environment) are never written to the manifest;
  # they are normalized and hashed.  The manifest only covers the paths and
  # parameters a task declares, so no directory or workspace scan takes place.
  #
  class FingerprintManifest
    # Version of the on-disk manifest format.
    FORMAT_VERSION = 1

    # Default manifest file name, relative to the directory rake runs in.
    DEFAULT_PATH = ".rake_fingerprint.json"

    class << self
      # Shared manifest objects keyed by expanded path, so every task
      # pointing at the same manifest shares the same write lock.
      def for(path)
        key = File.expand_path((path || DEFAULT_PATH).to_s)
        registry_mutex.synchronize do
          registry[key] ||= new(key)
        end
      end

      # Forget cached manifest objects.
      def reset_cache
        registry_mutex.synchronize { registry.clear }
      end

      # Digest of the contents of +path+, or nil when the file cannot be
      # read as a regular file.  The path is used exactly as given; callers
      # decide which files are in scope.
      def digest_file(path)
        return nil unless path
        return nil unless File.file?(path)
        Digest::SHA256.file(path).hexdigest
      rescue SystemCallError, IOError
        nil
      end

      # Digest of declared build parameters.  Returns nil for empty
      # parameters so that tasks without parameters compare consistently.
      def digest_parameters(parameters)
        return nil if parameters.nil?
        return nil if parameters.respond_to?(:empty?) && parameters.empty?

        Digest::SHA256.hexdigest(normalize(parameters))
      end

      # Deterministic, type preserving rendering of +value+ used as the
      # hashing input.  Values never reach the manifest, only this digest.
      def normalize(value)
        case value
        when Hash
          pairs = value.map { |k, v| [k.to_s, normalize(v)] }.sort_by(&:first)
          "{" + pairs.map { |k, v| "#{k.bytesize}:#{k}=#{v}" }.join(",") + "}"
        when Array
          "[" + value.map { |v| normalize(v) }.join(",") + "]"
        when Symbol
          "s#{value.inspect}"
        when String
          "t#{value.bytesize}:#{value}"
        when nil
          "nil"
        else
          "#{value.class}:#{value.inspect}"
        end
      end

      private

      def registry
        @registry ||= {}
      end

      def registry_mutex
        @registry_mutex ||= Mutex.new
      end
    end

    # Path of the manifest file.
    attr_reader :path

    def initialize(path = nil)
      @path = (path || DEFAULT_PATH).to_s
      @mutex = Mutex.new
    end

    # The recorded entry for +task_name+, or nil.
    def entry(task_name)
      entries[task_name.to_s]
    end

    # All recorded entries.
    def entries
      data = load
      data["entries"] ||= {}
      data["entries"]
    end

    # Record a successful build entry for +task_name+.
    def record(task_name, new_entry)
      @mutex.synchronize do
        data = load
        data["entries"][task_name.to_s] = new_entry
        write(data)
      end
      new_entry
    end

    # Remove the entry for +task_name+.  Returns true when something was
    # removed.
    def delete(task_name)
      @mutex.synchronize do
        data = load
        return false unless data["entries"].key?(task_name.to_s)

        data["entries"].delete(task_name.to_s)
        write(data)
        true
      end
    end

    # Remove every entry.
    def clear
      @mutex.synchronize do
        write("version" => FORMAT_VERSION, "entries" => {})
      end
    end

    # Names of all tasks with a recorded entry, sorted.
    def keys
      entries.keys.sort
    end

    private

    def load
      data = read
      unless data.is_a?(Hash) && data["entries"].is_a?(Hash)
        data = { "version" => FORMAT_VERSION, "entries" => {} }
      end
      data["version"] ||= FORMAT_VERSION
      data
    end

    def read
      return { "version" => FORMAT_VERSION, "entries" => {} } unless
        File.file?(@path)
      JSON.parse(File.read(@path))
    rescue JSON::ParserError, SystemCallError, IOError
      # A damaged manifest is treated as empty: everything in scope is
      # rebuilt and a fresh manifest replaces it.
      { "version" => FORMAT_VERSION, "entries" => {} }
    end

    # Write the manifest atomically so a failed build or a concurrent reader
    # never observes a half written document.
    def write(data)
      data = { "version" => FORMAT_VERSION, "entries" => {} }.merge(data)
      dir = File.dirname(@path)
      FileUtils.mkdir_p(dir) unless dir.empty? || dir == "."
      tmp = File.join(dir, ".#{File.basename(@path)}.#{Process.pid}.#{object_id}.tmp")
      begin
        File.open(tmp, "w") { |f| f.write(JSON.pretty_generate(data)) }
        replace(tmp, @path)
      ensure
        File.delete(tmp) if File.exist?(tmp)
      end
      data
    end

    def replace(from, to)
      File.rename(from, to)
    rescue SystemCallError
      # Windows can refuse to rename over an existing file; remove the old
      # manifest first, accepting a short window where no manifest exists.
      File.delete(to) if File.exist?(to)
      File.rename(from, to)
    end
  end
end
