require 'time'
require 'vcr/cassette/http_interaction_list'
require 'vcr/cassette/erb_renderer'
require 'vcr/cassette/serializers'

module VCR
  # The media VCR uses to store HTTP interactions for later re-use.
  class Cassette
    include Logger::Mixin

    # The supported record modes.
    #
    #   * :all -- Record every HTTP interactions; do not play any back.
    #   * :none -- Do not record any HTTP interactions; play them back.
    #   * :new_episodes -- Playback previously recorded HTTP interactions and record new ones.
    #   * :once -- Record the HTTP interactions if the cassette has not already been recorded;
    #              otherwise, playback the HTTP interactions.
    VALID_RECORD_MODES = [:all, :none, :new_episodes, :once]

    # The supported re-record modes. These only apply when a cassette is
    # re-recorded because {#re_record_interval} has elapsed.
    #
    #   * :overwrite -- Write the newly recorded interactions to the cassette
    #                   file as soon as the cassette is ejected, exactly like
    #                   a regular `:all` run. A partially refreshed cassette is
    #                   therefore indistinguishable from a fully refreshed one.
    #   * :complete  -- Treat the refresh as all-or-nothing. The previous
    #                   cassette file is left in place until every previously
    #                   recorded interaction has been recorded again in the
    #                   same run. Incomplete attempts are staged in a sibling
    #                   `<cassette>-pending` file (which also carries
    #                   diagnostics about the interactions that were not
    #                   refreshed) and the cassette keeps being re-recorded on
    #                   the next run.
    VALID_RE_RECORD_MODES = [:overwrite, :complete]

    # Suffix used for the staged / diagnostic copy of a cassette while a
    # `:complete` re-record attempt is in progress, so it is never picked up
    # as the cassette itself.
    PENDING_CASSETTE_SUFFIX = '-pending'

    # @return [#to_s] The name of the cassette. Used to determine the cassette's file name.
    # @see #file
    attr_reader :name

    # @return [Symbol] The record mode. Determines whether the cassette records HTTP interactions,
    #  plays them back, or does both.
    attr_reader :record_mode

    # @return [Boolean] The cassette's record_on_error mode. When the code that uses the cassette
    #  raises an error (for example a test failure) and record_on_error is set to false, no
    #  cassette will be recorded. This is useful when you are TDD'ing an API integration: when
    #  an error is raised that often means your request is invalid, so you don't want the cassette
    #  to be recorded.
    attr_reader :record_on_error

    # @return [Array<Symbol, #call>] List of request matchers. Used to find a response from an
    #  existing HTTP interaction to play back.
    attr_reader :match_requests_on

    # @return [Boolean, Hash] The cassette's ERB option. The file will be treated as an
    #  ERB template if this has a truthy value. A hash, if provided, will be used as local
    #  variables for the ERB template.
    attr_reader :erb

    # @return [Integer, nil] How frequently (in seconds) the cassette should be re-recorded.
    attr_reader :re_record_interval

    # @return [Symbol] How an interval-triggered re-record should be persisted.
    #  `:overwrite` (the default) keeps the historical behaviour; `:complete`
    #  only replaces the cassette once every previously recorded interaction
    #  has been recorded again during the same run.
    # @see VALID_RE_RECORD_MODES
    attr_reader :re_record_mode

    # @return [Boolean, nil] Should outdated interactions be recorded back to file
    attr_reader :clean_outdated_http_interactions

    # @return [Boolean] Should unused requests be dropped from the cassette?
    attr_reader :drop_unused_requests

    # @return [Array<Symbol>] If set, {VCR::Configuration#before_record} and
    #  {VCR::Configuration#before_playback} hooks with a corresponding tag will apply.
    attr_reader :tags

    # @param (see VCR#insert_cassette)
    # @see VCR#insert_cassette
    def initialize(name, options = {})
      @name    = name
      @options = VCR.configuration.default_cassette_options.merge(options)
      @mutex   = Mutex.new

      assert_valid_options!
      extract_options
      raise_error_unless_valid_record_mode
      raise_error_unless_valid_re_record_mode

      log "Initialized with options: #{format_options_for_log(@options)}"
    end

    # Ejects the current cassette. The cassette will no longer be used.
    # In addition, any newly recorded HTTP interactions will be written to
    # disk.
    #
    # @note This is not intended to be called directly. Use `VCR.eject_cassette` instead.
    #
    # @param (see VCR#eject_casssette)
    # @see VCR#eject_cassette
    def eject(options = {})
      if complete_re_record?
        persist_complete_re_record
      elsif should_write_recorded_interactions_to_disk?
        write_recorded_interactions_to_disk
      end

      if should_assert_no_unused_interactions? && !options[:skip_no_unused_interactions_assertion]
        http_interactions.assert_no_unused_interactions!
      end
    end

    # @private
    def run_failed!
      @run_failed = true
    end

    # @private
    def run_failed?
      @run_failed = false unless defined?(@run_failed)
      @run_failed
    end

    def should_write_recorded_interactions_to_disk?
      !run_failed? || record_on_error
    end

    # @private
    def http_interactions
      # Without this mutex, under threaded access, an HTTPInteractionList will overwrite
      # the first.
      @mutex.synchronize do
        @http_interactions ||= HTTPInteractionList.new \
          should_stub_requests? ? previously_recorded_interactions : [],
          match_requests_on,
          @allow_playback_repeats,
          @parent_list,
          log_prefix
      end
    end

    # @private
    def record_http_interaction(interaction)
      VCR::CassetteMutex.synchronize do
        log "Recorded HTTP interaction #{request_summary(interaction.request)} => #{response_summary(interaction.response)}"
        new_recorded_interactions << interaction
      end
    end

    # @private
    def new_recorded_interactions
      @new_recorded_interactions ||= []
    end

    # @return [String] The file for this cassette.
    # @raise [NotImplementedError] if the configured cassette persister
    #  does not support resolving file paths.
    # @note VCR will take care of sanitizing the cassette name to make it a valid file name.
    def file
      unless @persister.respond_to?(:absolute_path_to_file)
        raise NotImplementedError, "The configured cassette persister does not support resolving file paths"
      end
      @persister.absolute_path_to_file(storage_key)
    end

    # @return [Boolean] Whether or not the cassette is recording.
    def recording?
      case record_mode
        when :none; false
        when :once; raw_cassette_bytes.to_s.empty?
        else true
      end
    end

    # @return [Hash] The hash that will be serialized when the cassette is written to disk.
    def serializable_hash
      build_serializable_hash(interactions_to_record)
    end

    # @return [Hash] Custom metadata associated with the cassette.
    def metadata
      @metadata ||= recording? ? {} : (deserialized_hash['metadata'] || {})
    end

    # @return [Time, nil] The `recorded_at` time of the first HTTP interaction
    #                     or nil if the cassette has no prior HTTP interactions.
    #
    # @example
    #
    #   VCR.use_cassette("some cassette") do |cassette|
    #     Timecop.freeze(cassette.originally_recorded_at || Time.now) do
    #       # ...
    #     end
    #   end
    def originally_recorded_at
      @originally_recorded_at ||= previously_recorded_interactions.map(&:recorded_at).min
    end

    # @return [Boolean] false unless wrapped with LinkedCassette
    def linked?
      false
    end

    # @return [Boolean] Whether this cassette is being re-recorded because
    #  {#re_record_interval} has elapsed.
    def re_recording?
      !!@re_record_triggered
    end

    # @return [Boolean] Whether this cassette uses the all-or-nothing
    #  `:complete` re-record mode. When true, {#file} keeps serving the
    #  previous cassette until every interaction has been recorded again.
    def complete_re_record?
      re_recording? && re_record_mode == :complete
    end

    # @return [Hash, nil] Diagnostics for the most recent `:complete`
    #  re-record attempt (`nil` if no complete re-record was attempted).
    #  When the attempt was incomplete this includes every interaction from
    #  the previous cassette that could not be recorded again, so callers can
    #  report exactly what is still stale.
    def re_record_diagnostics
      @re_record_diagnostics
    end

    # @return [String, nil] Path to the staged / diagnostic copy used while a
    #  `:complete` re-record is in progress. Returns nil when the configured
    #  persister cannot resolve file paths.
    def re_record_pending_file
      return nil unless @persister.respond_to?(:absolute_path_to_file)
      @persister.absolute_path_to_file(pending_re_record_storage_key)
    end

  private

    def assert_valid_options!
      invalid_options = @options.keys - [
        :record, :record_on_error, :erb, :match_requests_on, :re_record_interval, :re_record_mode,
        :tag, :tags, :update_content_length_header, :allow_playback_repeats, :allow_unused_http_interactions,
        :exclusive, :serialize_with, :preserve_exact_body_bytes, :decode_compressed_response,
        :recompress_response, :persist_with, :persister_options, :clean_outdated_http_interactions,
        :drop_unused_requests
      ]

      if invalid_options.size > 0
        raise ArgumentError.new("You passed the following invalid options to VCR::Cassette.new: #{invalid_options.inspect}.")
      end
    end

    def extract_options
      [:record_on_error, :erb, :match_requests_on, :re_record_interval, :re_record_mode,
       :clean_outdated_http_interactions, :allow_playback_repeats, :allow_unused_http_interactions,
       :exclusive, :drop_unused_requests].each do |name|
        instance_variable_set("@#{name}", @options[name])
      end

      @re_record_mode ||= :overwrite

      assign_tags

      @serializer  = VCR.cassette_serializers[@options[:serialize_with]]
      @persister   = VCR.cassette_persisters[@options[:persist_with]]
      @re_record_triggered = should_re_record?(@options[:record])
      @record_mode = @re_record_triggered ? :all : @options[:record]
      @parent_list = @exclusive ? HTTPInteractionList::NullList : VCR.http_interactions
    end

    def assign_tags
      @tags = Array(@options.fetch(:tags) { @options[:tag] })

      [:update_content_length_header, :preserve_exact_body_bytes, :decode_compressed_response, :recompress_response].each do |tag|
        @tags << tag if @options[tag]
      end
    end

    def previously_recorded_interactions
      @previously_recorded_interactions ||= if !raw_cassette_bytes.to_s.empty?
        deserialized_hash['http_interactions'].map { |h| HTTPInteraction.from_hash(h) }.tap do |interactions|
          invoke_hook(:before_playback, interactions)
          invoke_hook(:recompress_response, interactions)

          interactions.reject! do |i|
            i.request.uri.is_a?(String) && VCR.request_ignorer.ignore?(i.request)
          end
        end
      else
        []
      end
    end

    def storage_key
      @storage_key ||= [name, @serializer.file_extension].join('.')
    end

    def raise_error_unless_valid_record_mode
      unless VALID_RECORD_MODES.include?(record_mode)
        raise ArgumentError.new("#{record_mode} is not a valid cassette record mode.  Valid modes are: #{VALID_RECORD_MODES.inspect}")
      end
    end

    def raise_error_unless_valid_re_record_mode
      unless VALID_RE_RECORD_MODES.include?(re_record_mode)
        raise ArgumentError.new("#{re_record_mode} is not a valid cassette re-record mode.  Valid modes are: #{VALID_RE_RECORD_MODES.inspect}")
      end
    end

    def should_re_record?(record_mode)
      return false unless @re_record_interval
      return false unless originally_recorded_at
      return false if record_mode == :none

      now = Time.now

      (originally_recorded_at + @re_record_interval < now).tap do |value|
        info = "previously recorded at: '#{originally_recorded_at}'; now: '#{now}'; interval: #{@re_record_interval} seconds"

        if !value
          log "Not re-recording since the interval has not elapsed (#{info})."
        elsif InternetConnection.available?
          log "re-recording (#{info})."
        else
          log "Not re-recording because no internet connection is available (#{info})."
          return false
        end
      end
    end

    def should_stub_requests?
      record_mode != :all
    end

    def should_remove_matching_existing_interactions?
      record_mode == :all
    end

    def should_remove_unused_interactions?
      @drop_unused_requests
    end

    def should_assert_no_unused_interactions?
      !(@allow_unused_http_interactions || $!)
    end

    def raw_cassette_bytes
      @raw_cassette_bytes ||= VCR::Cassette::ERBRenderer.new(@persister[storage_key], erb, name).render
    end

    def merged_interactions
      merged_interactions_with_origin.map(&:first)
    end

    # Like {#merged_interactions}, but tags each interaction with whether it
    # was recorded during this run (`true`) or carried over from the previous
    # cassette (`false`).
    #
    # @return [Array<Array(HTTPInteraction, Boolean)>]
    def merged_interactions_with_origin
      old_interactions = previously_recorded_interactions
      new_interactions = new_recorded_interactions

      if should_remove_matching_existing_interactions?
        new_interaction_list = HTTPInteractionList.new(new_interactions, match_requests_on)
        old_interactions = old_interactions.reject do |i|
          new_interaction_list.response_for(i.request)
        end
      end

      if should_remove_unused_interactions?
        new_interactions.map { |i| [i, true] }
      else
        up_to_date_interactions(old_interactions).map { |i| [i, false] } +
          new_interactions.map { |i| [i, true] }
      end
    end

    def up_to_date_interactions(interactions)
      return interactions unless clean_outdated_http_interactions && re_record_interval
      interactions.take_while { |x| x[:recorded_at] > Time.now - re_record_interval }
    end

    def build_serializable_hash(interactions)
      hash = {
        "http_interactions" => interactions.map(&:to_hash),
        "recorded_with"     => "VCR #{VCR.version}"
      }
      hash["metadata"] = metadata unless metadata.empty?
      hash
    end

    def interactions_to_record
      interactions_to_record_with_origin.map(&:first)
    end

    # Applies the `before_record` hooks once and keeps track of which
    # interactions were freshly recorded, so the completeness check for a
    # `:complete` re-record can rely on identity rather than on timestamps
    # (which lose sub-second precision when serialized).
    #
    # @return [Array<Array(HTTPInteraction, Boolean)>]
    def interactions_to_record_with_origin
      # We deep-dup the interactions by roundtripping them to/from a hash.
      # This is necessary because `before_record` can mutate the interactions.
      merged_interactions_with_origin.map do |interaction, fresh|
        [HTTPInteraction.from_hash(interaction.to_hash), fresh]
      end.tap do |tagged|
        interactions = tagged.map(&:first)
        invoke_hook(:before_record, interactions)

        # Hooks may drop interactions (`ignore!`); a dropped interaction is
        # not part of the cassette, so it must not count as refreshed.
        tagged.keep_if { |interaction, _| interactions.any? { |i| i.equal?(interaction) } }
      end
    end

    def write_recorded_interactions_to_disk
      return if new_recorded_interactions.none?
      hash = serializable_hash
      return if hash["http_interactions"].none?

      persist_cassette_content(storage_key, @serializer.serialize(hash))
    end

    # Persists the outcome of a `:complete` re-record attempt.
    #
    # The previously recorded cassette is only replaced once every interaction
    # from that cassette has a *fresh* recording in the same run. Otherwise the
    # old file is left untouched (so it keeps playing back offline and keeps its
    # original `recorded_at`), and the partial attempt plus diagnostics about
    # the missing interactions are written to a sibling `-pending` file.
    def persist_complete_re_record
      # Run the `before_record` hooks exactly once and keep the result: both
      # the completeness check and the write below must agree on what would
      # actually end up in the cassette.
      to_record = interactions_to_record_with_origin
      missing = missing_previously_recorded_interactions(to_record)
      writable = should_write_recorded_interactions_to_disk?

      if !writable
        @re_record_diagnostics = build_re_record_diagnostics(:aborted, missing)
        log "Complete re-record aborted by `record_on_error: false`; " +
            "kept #{file_description} unchanged and persisted nothing."
        missing.each do |interaction|
          log "  still stale: #{request_summary(interaction.request)} (recorded at #{interaction.recorded_at})"
        end
      elsif missing.any?
        @re_record_diagnostics = build_re_record_diagnostics(:incomplete, missing)
        persist_pending_re_record(to_record)
        log_incomplete_re_record(missing)
      elsif write_complete_cassette(to_record)
        @re_record_diagnostics = build_re_record_diagnostics(:complete, missing)
        delete_pending_re_record_file
        log "Complete re-record succeeded; replaced #{file_description}."
      else
        # Everything was filtered out by hooks (or the previous cassette was
        # already empty), so there is nothing to write. Never drop the
        # previous cassette on the floor for that.
        @re_record_diagnostics = build_re_record_diagnostics(:empty, missing)
        delete_pending_re_record_file
        log "Complete re-record produced no interactions to record; " +
            "kept #{file_description} unchanged."
      end
    end

    # @param to_record [Array<Array(HTTPInteraction, Boolean)>]
    # @return [Boolean] whether the cassette file was written
    def write_complete_cassette(to_record)
      return false if to_record.none?
      persist_cassette_content(storage_key, @serializer.serialize(build_serializable_hash(to_record.map(&:first))))
      true
    end

    # The staged payload records only the interactions captured during this
    # attempt (never the stale ones), so it can never be mistaken for a
    # complete refresh of the cassette. Diagnostics accompany it.
    #
    # @param to_record [Array<Array(HTTPInteraction, Boolean)>]
    def persist_pending_re_record(to_record)
      hash = {
        "http_interactions"     => fresh_interactions(to_record).map(&:to_hash),
        "recorded_with"         => "VCR #{VCR.version}",
        "re_record_diagnostics" => @re_record_diagnostics
      }
      persist_cassette_content(pending_re_record_storage_key, @serializer.serialize(hash))
    end

    def delete_pending_re_record_file
      key = pending_re_record_storage_key
      if @persister.respond_to?(:delete)
        @persister.delete(key)
      elsif (path = re_record_pending_file) && ::File.exist?(path)
        ::File.delete(path)
      end
    end

    # @param to_record [Array<Array(HTTPInteraction, Boolean)>]
    # @return [Array<HTTPInteraction>] previously recorded interactions for
    #  which no fresh recording exists, compared using the cassette's request
    #  matchers and taking duplicate requests into account.
    def missing_previously_recorded_interactions(to_record = interactions_to_record_with_origin)
      remaining = expected_previously_recorded_interactions.dup

      fresh_interactions(to_record).each do |fresh_interaction|
        index = remaining.index { |old_interaction| interaction_matches?(old_interaction, fresh_interaction) }
        remaining.delete_at(index) if index
      end

      remaining
    end

    # @param to_record [Array<Array(HTTPInteraction, Boolean)>]
    # @return [Array<HTTPInteraction>] interactions recorded during this run.
    def fresh_interactions(to_record)
      to_record.select { |_, fresh| fresh }.map(&:first)
    end

    # Interactions that a complete refresh is expected to reproduce. When
    # `clean_outdated_http_interactions` is enabled the outdated ones are
    # dropped rather than refreshed, matching the behaviour of that option.
    # With `drop_unused_requests` only the interactions that were actually
    # requested again are kept, so there is nothing to fall short of.
    def expected_previously_recorded_interactions
      return [] if drop_unused_requests

      if clean_outdated_http_interactions && re_record_interval
        up_to_date_interactions(previously_recorded_interactions)
      else
        previously_recorded_interactions
      end
    end

    def interaction_matches?(interaction_1, interaction_2)
      match_requests_on.all? do |matcher_name|
        VCR.request_matchers[matcher_name].matches?(interaction_1.request, interaction_2.request)
      end
    end

    def build_re_record_diagnostics(status, missing)
      expected = expected_previously_recorded_interactions
      {
        'status'                          => status.to_s,
        'cassette'                        => name.to_s,
        're_record_interval'              => re_record_interval,
        'attempted_at'                    => Time.now.httpdate,
        'previous_recorded_at'            => originally_recorded_at && originally_recorded_at.httpdate,
        'expected_interaction_count'      => expected.size,
        'recorded_interaction_count'      => new_recorded_interactions.size,
        'missing_interaction_count'       => missing.size,
        'missing_interactions'            => missing.map { |i| interaction_diagnostics(i) },
        'run_failed'                      => run_failed?,
        'previous_cassette_preserved'     => status != :complete
      }
    end

    def interaction_diagnostics(interaction)
      {
        'method'      => interaction.request.method.to_s,
        'uri'         => interaction.request.uri.to_s,
        'recorded_at' => interaction.recorded_at && interaction.recorded_at.httpdate
      }
    end

    def log_incomplete_re_record(missing)
      log "Complete re-record is incomplete: #{missing.size} of " +
          "#{expected_previously_recorded_interactions.size} previously recorded interaction(s) " +
          "were not recorded again. Kept #{file_description} unchanged and wrote diagnostics to " +
          "#{re_record_pending_file || pending_re_record_storage_key}."
      missing.each do |interaction|
        log "  missing: #{request_summary(interaction.request)} (recorded at #{interaction.recorded_at})"
      end
    end

    # {#file} raises for persisters that cannot resolve paths; logging must not.
    def file_description
      file
    rescue NotImplementedError
      storage_key
    end

    # Writes through the persister. When the persister supports `replace`
    # (the bundled file system persister does) the write is atomic, so a crash
    # can never leave a half-written cassette behind.
    def persist_cassette_content(key, content)
      if @persister.respond_to?(:replace)
        @persister.replace(key, content)
      else
        @persister[key] = content
      end
    end

    def pending_re_record_storage_key
      @pending_re_record_storage_key ||= "#{name}#{PENDING_CASSETTE_SUFFIX}.#{@serializer.file_extension}"
    end

    def invoke_hook(type, interactions)
      interactions.delete_if do |i|
        i.hook_aware.tap do |hw|
          VCR.configuration.invoke_hook(type, hw, self)
        end.ignored?
      end
    end

    def deserialized_hash
      @deserialized_hash ||= @serializer.deserialize(raw_cassette_bytes).tap do |hash|
        unless hash.is_a?(Hash) && hash['http_interactions'].is_a?(Array)
          raise Errors::InvalidCassetteFormatError.new \
            "#{file} does not appear to be a valid VCR 2.0 cassette. " +
            "VCR 1.x cassettes are not valid with VCR 2.0. When upgrading from " +
            "VCR 1.x, it is recommended that you delete all your existing cassettes and " +
            "re-record them, or use the provided vcr:migrate_cassettes rake task to migrate " +
            "them. For more info, see the VCR upgrade guide."
        end
      end
    end

    def log_prefix
      @log_prefix ||= "[Cassette: '#{name}'] "
    end

    # Use the legacy `{:key=>val}` format (Ruby 3.4 changed Hash#inspect for symbol keys).
    def format_options_for_log(value)
      case value
      when Hash
        "{#{value.map { |k, v| "#{k.inspect}=>#{format_options_for_log(v)}" }.join(', ')}}"
      else
        value.inspect
      end
    end

    def request_summary(request)
      super(request, match_requests_on)
    end
  end
end

