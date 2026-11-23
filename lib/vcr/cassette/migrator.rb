require 'vcr'

module VCR
  class Cassette
    # @private
    class Migrator
      # Reviews, then publishes, a directory-to-directory cassette migration.
      # The source directory is never modified; see DirectoryMigration for details.
      #
      # @return [DirectoryMigration] the migration, so callers can inspect its entries
      def self.migrate_directory(source_dir, target_dir, out = $stdout, default_serializer = :yaml)
        migration = DirectoryMigration.new(source_dir, target_dir, out, default_serializer)
        migration.review
        migration.publish!
        migration
      end

      def initialize(dir, out = $stdout)
        @dir, @out = dir, out
        @yaml_load_errors = yaml_load_errors
      end

      def migrate!
        @out.puts "Migrating VCR cassettes in #{@dir}..."
        Dir["#{@dir}/**/*.yml"].each do |cassette|
          migrate_cassette(cassette)
        end
      end

    private

      def migrate_cassette(cassette)
        unless http_interactions = load_yaml(cassette)
          @out.puts "  - Ignored #{relative_casssette_name(cassette)} since it could not be parsed as YAML (does it have some ERB?)"
          return
        end

        unless valid_vcr_1_cassette?(http_interactions)
          @out.puts "  - Ignored #{relative_casssette_name(cassette)} since it does not appear to be a valid VCR 1.x cassette"
          return
        end

        http_interactions.map! do |interaction|
          VCR1InteractionNormalization.normalize_interaction(interaction, File.mtime(cassette))
          interaction.to_hash
        end

        hash = {
          "http_interactions" => http_interactions,
          "recorded_with"     => "VCR #{VCR.version}"
        }

        File.open(cassette, 'w') { |f| f.write ::YAML.dump(hash) }
        @out.puts "  - Migrated #{relative_casssette_name(cassette)}"
      end

      def load_yaml(cassette)
        if ::YAML.respond_to?(:unsafe_load_file)
          ::YAML.unsafe_load_file(cassette)
        else
          ::YAML.load_file(cassette)
        end
      rescue *@yaml_load_errors
        return nil
      end

      def yaml_load_errors
        [ArgumentError].tap do |errors|
          errors << Psych::SyntaxError if defined?(Psych::SyntaxError)
        end
      end

      def relative_casssette_name(cassette)
        cassette.gsub(%r|\A#{Regexp.escape(@dir)}/?|, '')
      end

      def valid_vcr_1_cassette?(content)
        content.is_a?(Array) &&
        content.map(&:class).uniq == [HTTPInteraction]
      end
    end

    # @private
    #
    # Normalizes VCR 1.x HTTPInteraction structs so they can be converted to the
    # modern cassette hash format. Shared by the in-place Migrator and by
    # DirectoryMigration.
    module VCR1InteractionNormalization
      EMPTY_STRING = if String.method_defined?(:force_encoding)
        String.new.force_encoding("US-ASCII")
      else
        ''
      end

      def self.normalize_interaction(interaction, recorded_at)
        interaction.response.adapter_metadata = {}
        interaction.recorded_at = recorded_at
        remove_unnecessary_standard_port(interaction)
        denormalize_http_header_keys(interaction.request)
        denormalize_http_header_keys(interaction.response)
        normalize_body(interaction.request)
        normalize_body(interaction.response)
        interaction
      end

      def self.remove_unnecessary_standard_port(interaction)
        uri = VCR.configuration.uri_parser.parse(interaction.request.uri)
        if uri.scheme == 'http'  && uri.port == 80 ||
           uri.scheme == 'https' && uri.port == 443
          uri.port = nil
          interaction.request.uri = uri.to_s
        end
      rescue URI::InvalidURIError
        # ignore this URI.
        # This can occur when the user uses the filter_sensitive_data option
        # to put a substitution string in their URI
      end

      def self.denormalize_http_header_keys(object)
        object.headers = {}.tap do |denormalized|
          object.headers.each do |k, v|
            denormalized[denormalize_header_key(k)] = v
          end if object.headers
        end
      end

      def self.denormalize_header_key(key)
        key.split('-').               # 'user-agent' => %w(user agent)
          each { |w| w.capitalize! }. # => %w(User Agent)
          join('-')
      end

      def self.normalize_body(object)
        object.body = EMPTY_STRING if object.body.nil?
      end
    end
  end
end

require 'vcr/cassette/directory_migration'

