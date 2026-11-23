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

        # Sets the cassette for the given storage key, replacing any existing
        # content atomically.
        #
        # The content is first written to a temporary file in the same
        # directory and then renamed over the target, so readers (and a crash
        # mid-write) can never observe a truncated or partially written
        # cassette.
        #
        # @param [String] file_name the file name
        # @param [String] content the content to store
        def replace(file_name, content)
          path = absolute_path_to_file(file_name)
          directory = File.dirname(path)
          FileUtils.mkdir_p(directory) unless File.exist?(directory)

          tmp_path = "#{path}.#{Process.pid}.#{Thread.current.object_id}.tmp"
          begin
            File.binwrite(tmp_path, content)
            rename_over(tmp_path, path)
          ensure
            File.delete(tmp_path) if File.exist?(tmp_path)
          end
        end

        # Deletes the cassette for the given storage key.
        #
        # @param [String] file_name the file name
        # @return [Boolean] whether a file was removed
        def delete(file_name)
          path = absolute_path_to_file(file_name)
          return false unless path && File.exist?(path)
          File.delete(path)
          true
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

        # Renames `tmp_path` over `path`. `File.rename` already replaces the
        # destination on POSIX and on Windows (MOVEFILE_REPLACE_EXISTING), but
        # an antivirus / indexer can transiently hold the destination open, so
        # retry briefly before giving up.
        def rename_over(tmp_path, path)
          attempts = 0
          begin
            File.rename(tmp_path, path)
          rescue Errno::EACCES, Errno::EPERM
            attempts += 1
            raise if attempts >= 5
            sleep 0.01
            retry
          end
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
