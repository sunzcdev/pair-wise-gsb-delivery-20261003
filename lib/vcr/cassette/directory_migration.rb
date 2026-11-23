require 'fileutils'

module VCR
  class Cassette
    class Migrator
      # @private
      #
      # A two-phase ("review, then publish") directory-to-directory cassette
      # migration.
      #
      # `review` scans the source directory, detects the format of each
      # cassette (a supported VCR 1.x YAML interaction list, one of the
      # registered serializer formats, or an unknown format) and reports, per
      # relative path, the detected format and the destination it would be
      # published to -- without writing anything.
      #
      # `publish!` then performs the migration. Every cassette is converted to
      # the current cassette hash format, saved through the target serializer,
      # and re-read to confirm the original body bytes survived the round trip
      # before the new file is atomically moved into place. As a result:
      #
      #  * the source directory is never modified and can be re-read as-is, so
      #    the migration can be run as many times as needed;
      #  * a pre-existing destination is never left as a half-written file
      #    when a conversion fails part-way through -- the old destination
      #    remains intact and replayable, and the entry is reported with the
      #    stage at which it failed.
      class DirectoryMigration
        # @private
        # One line of the migration report.
        Entry = Struct.new(:relative_path, :format, :destination, :failed_at, :error) do
          def skipped?
            format == :unknown
          end
        end

        # @private
        SERIALIZERS_BY_EXTENSION = {
          'yml'  => :yaml,
          'yaml' => :yaml,
          'json' => :json
        }.freeze

        # @private
        TEMP_FILE_SUFFIX = '.vcr-migration-tmp'

        attr_reader :source_dir, :target_dir, :entries

        def initialize(source_dir, target_dir, out = $stdout, default_serializer = :yaml)
          @source_dir = File.expand_path(source_dir)
          @target_dir = File.expand_path(target_dir)
          @out = out
          @default_serializer = default_serializer
          @entries = []
        end

        # Scans the source directory and reports what would be published,
        # without modifying anything.
        #
        # @return [Array<Entry>] the review entries
        def review
          @entries = source_files.map { |path| build_entry(path) }
          @out.puts "Reviewing VCR cassettes in #{source_dir} for migration to #{target_dir}..."
          @entries.each do |entry|
            if entry.failed_at
              @out.puts "  - #{entry.relative_path}: failed during #{entry.failed_at}: #{entry.error.message}"
            elsif entry.skipped?
              @out.puts "  - #{entry.relative_path}: format=unknown, skipped"
            else
              @out.puts "  - #{entry.relative_path}: format=#{entry.format}, destination=#{entry.destination}"
            end
          end
          @entries
        end

        # Publishes the reviewed migration. Safe to run multiple times.
        #
        # @return [Array<Entry>] the entries, updated with any failure stages
        def publish!
          review if @entries.empty?
          @out.puts "Publishing migrated cassettes to #{target_dir}..."
          @entries.each { |entry| publish_entry(entry) }
          @entries
        end

      private

        def source_files
          Dir[File.join(source_dir, '**', '*')].select { |f| File.file?(f) }.sort
        end

        def build_entry(path)
          relative = relative_path_for(path, source_dir)
          format = detect_format(File.binread(path), extension_of(path))
          Entry.new(relative, format, format == :unknown ? nil : relative)
        rescue => e
          Entry.new(relative_path_for(path, source_dir), :unknown, nil, :read, e)
        end

        def publish_entry(entry)
          if entry.skipped?
            @out.puts "  - Skipped #{entry.relative_path} (unknown format)"
            return
          end

          entry.failed_at = nil
          entry.error = nil
          stage = :read
          tmp_path = nil
          source_content = File.binread(File.join(source_dir, entry.relative_path))
          stage = :convert
          hash = convert(entry, source_content)
          stage = :serialize
          serializer = serializer_for(entry)
          content = serializer.serialize(hash)
          destination = File.join(target_dir, entry.destination)

          if File.exist?(destination) && File.binread(destination).b == content.b
            @out.puts "  - Unchanged #{entry.relative_path} (#{entry.format})"
            return
          end

          stage = :write
          FileUtils.mkdir_p(File.dirname(destination))
          tmp_path = destination + TEMP_FILE_SUFFIX
          File.binwrite(tmp_path, content)
          stage = :verify
          verify_round_trip(tmp_path, hash, serializer)
          stage = :publish
          File.rename(tmp_path, destination)
          @out.puts "  - Migrated #{entry.relative_path} (#{entry.format} -> #{entry.destination})"
        rescue => e
          entry.failed_at = stage
          entry.error = e
          File.delete(tmp_path) if tmp_path && File.exist?(tmp_path)
          @out.puts "  - Failed #{entry.relative_path} during #{stage}: #{e.class}: #{e.message}"
        end

        def detect_format(content, extension)
          ([SERIALIZERS_BY_EXTENSION[extension]] + [:yaml, :json]).compact.uniq.each do |name|
            parsed = begin
              VCR.cassette_serializers[name].deserialize(content)
            rescue StandardError
              next
            end

            return :vcr_1_yaml if vcr_1_cassette?(parsed)
            return name if current_cassette?(parsed)
          end
          :unknown
        end

        def vcr_1_cassette?(content)
          content.is_a?(Array) &&
          !content.empty? &&
          content.all? { |interaction| interaction.is_a?(VCR::HTTPInteraction) }
        end

        def current_cassette?(content)
          content.is_a?(Hash) && content['http_interactions'].is_a?(Array)
        end

        def convert(entry, source_content)
          case entry.format
          when :vcr_1_yaml
            interactions = VCR.cassette_serializers[:yaml].deserialize(source_content)
            recorded_at = File.mtime(File.join(source_dir, entry.relative_path))
            hashes = interactions.map do |interaction|
              VCR1InteractionNormalization.normalize_interaction(interaction, recorded_at)
              preserve_body_bytes!(interaction.to_hash)
            end
            { 'http_interactions' => hashes, 'recorded_with' => "VCR #{VCR.version}" }
          when :yaml, :json
            hash = VCR.cassette_serializers[entry.format].deserialize(source_content)
            hashes = hash['http_interactions'].map do |interaction_hash|
              interaction_hash = interaction_hash.dup
              interaction_hash['recorded_at'] ||= Time.now.httpdate
              preserve_body_bytes!(VCR::HTTPInteraction.from_hash(interaction_hash).to_hash)
            end
            {
              'http_interactions' => hashes,
              'recorded_with'     => hash['recorded_with'] || "VCR #{VCR.version}"
            }
          else
            raise "Cannot convert #{entry.relative_path}: unsupported format #{entry.format.inspect}"
          end
        end

        # Ensures every body can be saved through any target serializer and
        # re-read with its exact original bytes, base64-encoding bodies that
        # are not valid UTF-8 (e.g. binary response bodies).
        def preserve_body_bytes!(interaction_hash)
          %w[request response].each do |key|
            message = interaction_hash[key]
            next unless message.is_a?(Hash)
            body = message['body']
            next unless body.is_a?(Hash)
            string = body['string']
            next if string.nil?
            next if string.dup.force_encoding(Encoding::UTF_8).valid_encoding?

            message['body'] = {
              'encoding'      => string.encoding.name,
              'base64_string' => [string.b].pack('m0')
            }
          end
          interaction_hash
        end

        def verify_round_trip(tmp_path, hash, serializer)
          reparsed = serializer.deserialize(File.binread(tmp_path))
          unless bodies_in(reparsed) == bodies_in(hash)
            raise "The re-read cassette bodies do not match the original bytes"
          end
        end

        def bodies_in(cassette_hash)
          Array(cassette_hash['http_interactions']).flat_map do |interaction_hash|
            request  = interaction_hash['request']  || {}
            response = interaction_hash['response'] || {}
            [
              VCR::Request.body_from(request['body']),
              VCR::Response.body_from(response['body'])
            ].map { |body| body.to_s.b }
          end
        end

        def serializer_for(entry)
          name = SERIALIZERS_BY_EXTENSION[extension_of(entry.destination)] || @default_serializer
          VCR.cassette_serializers[name]
        end

        def extension_of(path)
          File.extname(path.to_s).sub(/\A\./, '').downcase
        end

        def relative_path_for(path, dir)
          path.sub(%r|\A#{Regexp.escape(dir)}/?|, '')
        end
      end
    end
  end
end
