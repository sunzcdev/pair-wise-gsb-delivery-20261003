require 'fileutils'

module VCR
  class Cassette
    class Persisters
      # The only built-in cassette persister. Persists cassettes to the file system.
      module FileSystem
        extend self

        # @private
        attr_reader :storage_location

        # @private
        def storage_location=(dir)
          FileUtils.mkdir_p(dir) if dir
          @storage_location = dir ? absolute_path_for(dir) : nil
        end

        # Gets the cassette for the given storage key (file name).
        #
        # @param [String] file_name the file name
        # @return [String] the cassette content
        def [](file_name)
          path = absolute_path_to_file(file_name)
          return nil unless File.exist?(path)
          File.binread(path)
        end

        # Sets the cassette for the given storage key (file name).
        #
        # @param [String] file_name the file name
        # @param [String] content the content to store
        def []=(file_name, content)
          path = absolute_path_to_file(file_name)
          directory = File.dirname(path)
          FileUtils.mkdir_p(directory) unless File.exist?(directory)
          File.binwrite(path, content)
        end

        # Explicit, concurrency-aware cassette save.
        #
        # Unlike {#[]=}, this re-reads the target file immediately before
        # writing and merges the incoming interactions with anything another
        # process (or thread) may have landed in the meantime. The whole
        # read/merge/write sequence is file-locked and the final write is
        # atomic, so two recording processes started from the same cassette
        # version can safely commit in either order.
        #
        # @param [String] file_name the file name
        # @param [Hash] attributes the safe-write attributes
        # @option attributes [#serialize, #deserialize] :serializer the
        #  cassette serializer
        # @option attributes [Array<VCR::HTTPInteraction>] :incoming_interactions
        #  interactions recorded by this writer since the cassette was loaded
        # @option attributes [String, nil] :base_bytes the raw cassette content
        #  that was on disk when this writer's cassette was loaded
        # @option attributes [Array<Symbol, #call>] :request_matchers matchers
        #  defining which requests count as the same request
        # @option attributes [Hash] :full_cassette_hash the exact hash the
        #  cassette would write in a single-process run
        # @return [void]
        # @raise [VCR::Errors::CassetteWriteConflictError] if another writer
        #  already stored a different response for an incoming request
        def safe_write(file_name, attributes)
          SafeWriter.new(
            path: absolute_path_to_file(file_name),
            serializer: attributes.fetch(:serializer),
            incoming_interactions: attributes.fetch(:incoming_interactions),
            base_bytes: attributes.fetch(:base_bytes),
            request_matchers: attributes.fetch(:request_matchers),
            full_cassette_hash: attributes.fetch(:full_cassette_hash)
          ).commit
        end

        # @private
        def absolute_path_to_file(file_name)
          return nil unless storage_location
          File.join(storage_location, sanitized_file_name_from(file_name))
        end

      private
        def absolute_path_for(path)
          Dir.chdir(path) { Dir.pwd }
        end

        def sanitized_file_name_from(file_name)
          parts = file_name.to_s.split('.')

          if parts.size > 1 && !parts.last.include?(File::SEPARATOR)
            file_extension = '.' + parts.pop
          end

          file_name = parts.join('.').gsub(/[^[:word:]\-\/]+/, '_') + file_extension.to_s
          file_name.downcase! if downcase_cassette_names?
          file_name
        end

        def downcase_cassette_names?
          !!VCR.configuration
            .default_cassette_options
            .dig(:persister_options, :downcase_cassette_names)
        end
      end
    end
  end
end
