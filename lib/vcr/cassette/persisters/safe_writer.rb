require 'fileutils'

module VCR
  class Cassette
    class Persisters
      # Performs an explicit, concurrency-aware save of a cassette.
      #
      # Two recording processes that start from the same cassette version can
      # finish at the same time. A plain overwrite would silently drop the
      # interactions recorded by the process that finishes first. This writer
      # instead re-reads the cassette immediately before writing, three-way
      # merges the newly recorded interactions into it, and only commits when
      # the two writers did not record different responses for the same
      # request.
      #
      # The read/merge/write critical section is guarded by an exclusive file
      # lock (cross-process) and by an in-process mutex (multi-threaded VCR),
      # and the commit itself is an atomic rename of a temporary file.
      #
      # @see VCR::Cassette::Persisters::FileSystem.safe_write
      class SafeWriter
        # @return [String] absolute path of the cassette being written
        attr_reader :path

        # @return [#serialize, #deserialize] serializer used for the cassette
        attr_reader :serializer

        # @return [Array<Symbol, #call>] request matchers used to detect clashes
        attr_reader :request_matchers

        # @param path [String] absolute path of the cassette file
        # @param serializer [#serialize, #deserialize] the cassette serializer
        # @param incoming_interactions [Array<VCR::HTTPInteraction>] interactions
        #   recorded by this writer since the cassette was loaded
        # @param base_bytes [String, nil] the raw cassette content that was on
        #   disk when this writer's cassette was loaded
        # @param request_matchers [Array<Symbol, #call>] matchers defining which
        #   requests count as "the same request"
        # @param full_cassette_hash [Hash] the exact hash the cassette would
        #   write in a single-process run (base interactions merged with the
        #   incoming ones, with all `before_record` hooks already applied)
        def initialize(path:, serializer:, incoming_interactions:, base_bytes:,
                       request_matchers:, full_cassette_hash:)
          @path = path
          @serializer = serializer
          @incoming_interactions = incoming_interactions
          @base_bytes = base_bytes
          @request_matchers = request_matchers
          @full_cassette_hash = full_cassette_hash
          # Use a real logger rather than the configured one: conflict
          # diagnostics must be populated even when debug logging is off and
          # the configured logger is the null object.
          @summary_logger = VCR::Logger.new(nil)
        end

        # Merges the incoming interactions into the cassette on disk and
        # commits the result atomically.
        #
        # @return [void]
        # @raise [VCR::Errors::CassetteWriteConflictError] when another writer
        #   already stored a different response for an incoming request
        def commit
          with_in_process_mutex do
            with_exclusive_file_lock do
              current_bytes = read_current_bytes

              merged_bytes =
                if current_bytes == @base_bytes
                  # Nobody else wrote in the meantime: the cassette's own
                  # serialized hash is already the correct merge.
                  @serializer.serialize(@full_cassette_hash)
                else
                  merge_with(current_bytes)
                end

              atomically_write_to(@path, merged_bytes)
            end
          end
        end

        private

        # Re-reads the cassette another writer may have committed and merges
        # the incoming interactions into it.
        def merge_with(current_bytes)
          current_hash = valid_cassette_hash(current_bytes)
          current_interactions = interactions_from_hash(current_hash)
          base_interactions = interactions_from(@base_bytes)

          interactions_added_by_others = diff_interactions(current_interactions, base_interactions)
          conflicts = detect_conflicts(interactions_added_by_others)
          reject!(conflicts) if conflicts.any?

          merged = current_interactions + @incoming_interactions
          @serializer.serialize(cassette_hash_from(current_hash, merged))
        end

        # Returns the interactions present in `newer` but absent from `older`.
        # `newer` is the revision another writer committed and `older` is the
        # common base revision both writers started from, so anything in the
        # difference was added by another writer.
        def diff_interactions(newer, older)
          older_keys = older.map { |i| identity_key(i) }
          newer.reject { |i| older_keys.include?(identity_key(i)) }
        end

        def detect_conflicts(foreign_interactions)
          @incoming_interactions.flat_map do |incoming|
            foreign_interactions.map do |foreign|
              next unless same_request?(incoming.request, foreign.request)
              next if same_response?(incoming.response, foreign.response)

              {
                incoming: incoming,
                request_summary: request_summary(incoming.request),
                kept_response_summary: response_summary(foreign.response),
                rejected_response_summary: response_summary(incoming.response)
              }
            end
          end.compact
        end

        def reject!(conflicts)
          rejected = conflicts.map { |c| c[:incoming] }
          rejected_file = write_rejected_interactions(rejected)
          public_conflicts = conflicts.map do |conflict|
            conflict.reject { |key, _| key == :incoming }
          end
          raise VCR::Errors::CassetteWriteConflictError.new(
            cassette_file: @path,
            rejected_interactions_file: rejected_file,
            conflicts: public_conflicts
          )
        end

        def write_rejected_interactions(interactions)
          rejected_path = rejected_interactions_path
          hash = {
            "http_interactions" => interactions.map(&:to_hash),
            "recorded_with" => "VCR #{VCR.version}",
            "metadata" => {
              "rejected_at" => Time.now.utc.httpdate,
              "reason" => "conflicting response already recorded in #{File.basename(@path)}"
            }
          }
          atomically_write_to(rejected_path, @serializer.serialize(hash))
          rejected_path
        end

        def rejected_interactions_path
          extension = File.extname(@path)
          stamp = Time.now.utc.strftime('%Y%m%d%H%M%S%N')
          base = extension.empty? ? @path : @path[0...(-extension.length)]
          "#{base}.rejected-#{stamp}#{extension}"
        end

        def same_request?(request_1, request_2)
          @request_matchers.all? do |matcher|
            VCR.request_matchers[matcher].matches?(request_1, request_2)
          end
        end

        def same_response?(response_1, response_2)
          response_1.to_hash == response_2.to_hash
        end

        # Stable identity for an interaction so the same request/response pair
        # recorded by both writers is not treated as a foreign addition. The
        # recording timestamp is deliberately ignored: two writers never share
        # one, but it does not make two otherwise identical interactions
        # different.
        def identity_key(interaction)
          [interaction.request.to_hash, interaction.response.to_hash]
        end

        def interactions_from(bytes)
          return [] if bytes.to_s.empty?
          interactions_from_hash(valid_cassette_hash(bytes))
        end

        def valid_cassette_hash(bytes)
          hash = @serializer.deserialize(bytes)
          unless hash.is_a?(Hash) && hash['http_interactions'].is_a?(Array)
            raise VCR::Errors::InvalidCassetteFormatError,
                  "#{@path} does not contain a valid VCR cassette and cannot be safely merged"
          end
          hash
        end

        def interactions_from_hash(hash)
          hash['http_interactions'].map { |h| VCR::HTTPInteraction.from_hash(h) }
        end

        # Builds the hash to serialize for a merged commit. The revision
        # currently on disk wins for every top-level key (metadata,
        # recorded_with, ...) so a concurrent writer cannot have its
        # non-interaction data dropped; only the interaction list is replaced.
        def cassette_hash_from(current_hash, interactions)
          current_hash.merge(
            "http_interactions" => interactions.map(&:to_hash)
          )
        end

        def read_current_bytes
          File.exist?(@path) ? File.binread(@path) : nil
        end

        def atomically_write_to(target, content)
          FileUtils.mkdir_p(File.dirname(target))
          temporary = File.join(
            File.dirname(target),
            ".#{File.basename(target)}.#{Process.pid}.#{Thread.current.object_id}.#{object_id}.tmp"
          )

          File.binwrite(temporary, content)
          File.rename(temporary, target)
        ensure
          File.delete(temporary) if temporary && File.exist?(temporary)
        end

        def with_exclusive_file_lock
          FileUtils.mkdir_p(File.dirname(@path))
          lock_path = "#{@path}.lock"
          lock_file = nil
          # Keep the handle in a local variable and close it explicitly:
          # relying on GC to release an flock makes unlock timing depend on
          # the interpreter's memory pressure.
          lock_file = File.open(lock_path, File::CREAT | File::RDWR, 0o644)
          lock_file.flock(File::LOCK_EX)
          yield
        ensure
          if lock_file
            lock_file.flock(File::LOCK_UN)
            lock_file.close
          end
        end

        # One mutex per cassette path, shared across threads in this process.
        # Paths are bounded in practice by the number of distinct cassettes,
        # and a mutex is only retained while its path remains in the registry.
        IN_PROCESS_MUTEXES = Hash.new { |hash, key| hash[key] = Mutex.new }

        def with_in_process_mutex
          IN_PROCESS_MUTEXES[@path].synchronize { yield }
        end

        def request_summary(request)
          @summary_logger.request_summary(request, @request_matchers)
        end

        def response_summary(response)
          @summary_logger.response_summary(response)
        end
      end
    end
  end
end
