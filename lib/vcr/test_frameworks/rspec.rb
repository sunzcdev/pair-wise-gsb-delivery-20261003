module VCR
  # Integrates VCR with RSpec.
  module RSpec
    # @private
    module Metadata
      extend self

      def vcr_cassette_name_for(metadata)
        description = 
          if metadata[:description].empty?
            # we have an "it { is_expected.to be something }" block
            metadata[:scoped_id]
          else
            metadata[:description]
          end
        example_group = 
          if metadata.key?(:example_group)
            metadata[:example_group]
          else
            metadata[:parent_example_group]
          end

        if example_group
          [vcr_cassette_name_for(example_group), description].join('/')
        else
          description
        end
      end

      # @private
      #
      # Ejects the cassette inserted for the given example, applying the
      # `assert_no_unused_interactions` semantics that match the example's
      # result:
      #
      # * Passed example: the assertion runs normally and raises
      #   {VCR::Errors::UnusedHTTPInteractionError} (with a usage summary)
      #   when the cassette has leftover interactions.
      # * Failed example: the example's original exception is preserved as
      #   the reported failure; the assertion is skipped and the unused
      #   interaction details are emitted as a warning for context.
      # * Pending example: the example is not expected to run to
      #   completion, so leftover interactions are expected and the
      #   assertion is skipped silently.
      def eject_cassette_for(example)
        if example_failed?(example)
          cassette = VCR.eject_cassette(skip_no_unused_interactions_assertion: true)
          warn_about_unused_interactions(example, cassette)
        elsif example_pending?(example)
          VCR.eject_cassette(skip_no_unused_interactions_assertion: true)
        else
          VCR.eject_cassette
        end
      end

      def configure!
        ::RSpec.configure do |config|

          when_tagged_with_vcr = { vcr: lambda { |v| !!v } }

          config.before(:each, when_tagged_with_vcr) do |ex|
            example = ex.respond_to?(:metadata) ? ex : ex.example

            cassette_name = nil
            options = example.metadata[:vcr]
            options = case options
                      when Hash #=> vcr: { cassette_name: 'foo' }
                        options.dup
                      when String #=> vcr: 'bar'
                        cassette_name = options.dup
                        {}
                      else #=> :vcr or vcr: true
                        {}
                      end

            cassette_name ||= options.delete(:cassette_name) ||
                              VCR::RSpec::Metadata.vcr_cassette_name_for(example.metadata)
            VCR.insert_cassette(cassette_name, options)
          end

          config.after(:each, when_tagged_with_vcr) do |ex|
            example = ex.respond_to?(:metadata) ? ex : ex.example
            VCR::RSpec::Metadata.eject_cassette_for(example)
          end
        end
      end

    private

      def example_failed?(example)
        !!example.exception
      end

      def example_pending?(example)
        execution_result = example.respond_to?(:execution_result) && example.execution_result
        return true if execution_result && execution_result.pending_message
        !!example.metadata[:pending]
      end

      def warn_about_unused_interactions(example, cassette)
        return unless cassette
        return if cassette.allow_unused_http_interactions

        summary = cassette.unused_interactions_summary
        return unless summary

        exception = example.exception
        warn "VCR: the example already failed with " \
             "#{exception.class}: #{exception.message.inspect}, so the unused " \
             "HTTP interaction assertion for cassette '#{cassette.name}' was " \
             "skipped and the original failure is reported first. " \
             "Unused interaction details:\n#{summary}"
      end
    end
  end
end

