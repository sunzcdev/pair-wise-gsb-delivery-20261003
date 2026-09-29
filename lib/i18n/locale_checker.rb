# frozen_string_literal: true

require 'yaml'
require 'json'

module I18n
  # I18n::LocaleChecker compares the interpolation placeholders of every
  # locale found in a directory against a reference locale. It is meant to
  # catch mismatches such as an English "Hello %{user_name}" being translated
  # as "你好 %{username}" before a locale file ships.
  #
  # The checker
  #
  # * extracts placeholders with the same patterns the interpolator uses, so
  #   escaped percent signs ("%%{name}") are not mistaken for placeholders;
  # * understands pluralization branches (:one, :other, ...) and compares
  #   placeholders branch by branch;
  # * follows Symbol aliases ("links") to their target and reports the
  #   target's file and key so shared targets stay traceable;
  # * only reads the given directory: *.rb files are skipped (never
  #   evaluated) and callable values (Procs, lambdas) are never called;
  # * reports a structured list of Issue objects instead of raising.
  #
  # Locales legitimately use different sets of plural branches (e.g. Chinese
  # only needs :other). Such known differences can be silenced:
  #
  #   checker = I18n::LocaleChecker.new(
  #     "config/locales",
  #     reference_locale: :en,
  #     allowed_branch_differences: {
  #       zh: [:one],                            # any key may lack :one
  #       ru: { "inbox.messages" => [:few] }     # one key may lack :few
  #     }
  #   )
  #   checker.check # => [#<I18n::LocaleChecker::Issue ...>, ...]
  class LocaleChecker
    PLURAL_KEYS = %i[zero one two few many other].freeze
    # Explicit "0"/"1" count keys, see I18n::Backend::Pluralization.
    EXPLICIT_COUNT_KEYS = [:"0", :"1"].freeze
    PLURAL_BRANCH_KEYS = (PLURAL_KEYS + EXPLICIT_COUNT_KEYS).freeze

    LOADABLE_EXTENSIONS = %w[.yml .yaml .json].freeze
    SKIPPED_EXTENSIONS  = %w[.rb].freeze

    INTERPOLATION_UNION = Regexp.union(I18n::DEFAULT_INTERPOLATION_PATTERNS)

    # A traceable position of a translation: the file it was loaded from and
    # its flattened key, e.g. "config/locales/en.yml" + "inbox.messages".
    Location = Struct.new(:file, :key, keyword_init: true) do
      def to_s
        "#{file}:#{key}"
      end
    end

    # A single finding. +type+ is one of :missing_params, :extra_params,
    # :missing_branch, :extra_branch or :unresolvable_alias. +params+ holds
    # the differing placeholder names, +branch+ the plural branch concerned
    # (nil for plain entries). +location+ points at the checked locale's
    # entry, +reference_location+ at the reference locale's entry. When the
    # checked entry reaches its value through a Symbol alias, +alias_target+
    # and +alias_location+ identify the shared target.
    Issue = Struct.new(
      :key, :locale, :type, :params, :branch,
      :location, :reference_location, :alias_target, :alias_location,
      keyword_init: true
    )

    Entry = Struct.new(:value, :location) # :nodoc:

    # Result of following a chain of Symbol aliases. +entry+ is the final
    # non-Symbol entry, +alias_target+ the flattened key it was reached
    # through (nil when no alias was involved) and +error+ one of :missing
    # or :cycle when the chain could not be resolved.
    Resolution = Struct.new(:entry, :alias_target, :error) # :nodoc:

    attr_reader :root, :reference_locale, :skipped_files

    def initialize(root, reference_locale:, allowed_branch_differences: {})
      @root = root
      @reference_locale = reference_locale.to_sym
      @allowed_branch_differences = normalize_branch_differences(allowed_branch_differences)
      @translations = Hash.new { |hash, locale| hash[locale] = {} }
      @skipped_files = []
      load_directory
    end

    # Returns an Array of Issue describing every interpolation placeholder
    # mismatch against the reference locale.
    def check
      issues = []
      reference = @translations[@reference_locale]

      reference.keys.sort.each do |key|
        reference_resolution = resolve(@reference_locale, key)
        reference_shape = shape(reference_resolution.entry&.value)
        next unless reference_shape

        each_locale_entry(key) do |locale, entry|
          resolution = resolve(locale, key)

          if resolution.error
            issues << Issue.new(
              key: key, locale: locale, type: :unresolvable_alias,
              params: [], branch: nil,
              location: entry.location,
              reference_location: reference_resolution.entry&.location,
              alias_target: resolution.alias_target,
              alias_location: nil
            )
            next
          end

          locale_shape = shape(resolution.entry&.value)
          next unless locale_shape

          compare(key, locale, entry, reference_shape, reference_resolution, locale_shape, resolution, issues)
        end
      end

      issues
    end

    # Extracts interpolation placeholder names from a string using the same
    # patterns as the interpolator. Escaped percent signs ("%%") match the
    # first pattern and carry no capture group, so "%%{name}" yields nothing.
    def self.extract_placeholders(string)
      string.scan(INTERPOLATION_UNION).each_with_object([]) do |match, names|
        name = match.compact.first
        names << name.to_sym if name
      end.uniq
    end

    private

    def load_directory
      pattern = File.join(@root, "**", "*{#{LOADABLE_EXTENSIONS.join(',')},#{SKIPPED_EXTENSIONS.join(',')}}")
      Dir.glob(pattern).sort.each do |file|
        extension = File.extname(file)

        if SKIPPED_EXTENSIONS.include?(extension)
          # Ruby locale files would have to be evaluated; the checker only
          # reads directories and never executes user supplied code.
          @skipped_files << file
          next
        end

        data = extension == ".json" ? load_json(file) : load_yaml(file)
        next unless data.is_a?(Hash)

        data.each_pair do |locale, translations|
          flatten(locale.to_sym, translations, file, nil) if translations.is_a?(Hash)
        end
      end
    end

    def load_yaml(file)
      YAML.safe_load_file(file, permitted_classes: [Symbol], aliases: true, symbolize_names: true)
    end

    def load_json(file)
      JSON.parse(File.read(file), symbolize_names: true)
    end

    def flatten(locale, hash, file, prefix)
      hash.each_pair do |key, value|
        flat_key = prefix ? "#{prefix}.#{key}" : key.to_s

        if value.is_a?(Hash) && !plural_hash?(value)
          flatten(locale, value, file, flat_key)
        else
          @translations[locale][flat_key] = Entry.new(value, Location.new(file: file, key: flat_key))
        end
      end
    end

    def plural_hash?(hash)
      !hash.empty? && hash.keys.all? { |key| PLURAL_BRANCH_KEYS.include?(key.to_sym) }
    end

    # Follows Symbol aliases ("links") within one locale without ever calling
    # callable values. Returns a Resolution.
    def resolve(locale, flat_key)
      entry = @translations[locale][flat_key]
      return Resolution.new(nil, nil, :missing) unless entry

      seen = [flat_key]
      alias_target = nil

      while entry && entry.value.is_a?(Symbol)
        alias_target = entry.value.to_s
        return Resolution.new(nil, alias_target, :cycle) if seen.include?(alias_target)

        seen << alias_target
        entry = @translations[locale][alias_target]
        return Resolution.new(nil, alias_target, :missing) unless entry
      end

      Resolution.new(entry, alias_target, nil)
    end

    # Returns the comparable "shape" of a value: [:plural, {branch => names}]
    # for pluralization hashes, [:plain, names] for strings and arrays of
    # strings, or nil for values that cannot be inspected without executing
    # them (Procs, lambdas) or carry no placeholders (numbers, booleans).
    def shape(value)
      case value
      when Hash
        [:plural, value.each_with_object({}) do |(branch, text), branches|
          branches[branch.to_sym] = placeholders_of(text)
        end]
      else
        names = placeholders_of(value)
        names && [:plain, names]
      end
    end

    def placeholders_of(value)
      case value
      when String
        self.class.extract_placeholders(value)
      when Array
        value.grep(String).flat_map { |string| self.class.extract_placeholders(string) }.uniq
      end
    end

    def compare(key, locale, entry, reference_shape, reference_resolution, locale_shape, resolution, issues)
      reference_kind, reference_branches = reference_shape
      locale_kind, locale_branches = locale_shape

      if reference_kind == :plural && locale_kind == :plural
        compare_branches(key, locale, entry, reference_branches, locale_branches, reference_resolution, resolution, issues)
      else
        reference_params = reference_kind == :plural ? reference_branches.values.compact.flatten.uniq : reference_branches
        locale_params = locale_kind == :plural ? locale_branches.values.compact.flatten.uniq : locale_branches
        compare_params(key, locale, entry, nil, reference_params, locale_params, reference_resolution, resolution, issues)
      end
    end

    def compare_branches(key, locale, entry, reference_branches, locale_branches, reference_resolution, resolution, issues)
      (reference_branches.keys - locale_branches.keys).each do |branch|
        next if branch_difference_allowed?(locale, key, branch)

        issues << build_issue(key, locale, entry, :missing_branch, [], branch, reference_resolution, resolution)
      end

      (locale_branches.keys - reference_branches.keys).each do |branch|
        next if branch_difference_allowed?(locale, key, branch)

        issues << build_issue(key, locale, entry, :extra_branch, [], branch, reference_resolution, resolution)
      end

      (reference_branches.keys & locale_branches.keys).each do |branch|
        next unless reference_branches[branch] && locale_branches[branch]

        compare_params(
          key, locale, entry, branch,
          reference_branches[branch], locale_branches[branch],
          reference_resolution, resolution, issues
        )
      end
    end

    def compare_params(key, locale, entry, branch, reference_params, locale_params, reference_resolution, resolution, issues)
      missing = reference_params - locale_params
      extra = locale_params - reference_params

      issues << build_issue(key, locale, entry, :missing_params, missing, branch, reference_resolution, resolution) unless missing.empty?
      issues << build_issue(key, locale, entry, :extra_params, extra, branch, reference_resolution, resolution) unless extra.empty?
    end

    def build_issue(key, locale, entry, type, params, branch, reference_resolution, resolution)
      Issue.new(
        key: key,
        locale: locale,
        type: type,
        params: params,
        branch: branch,
        location: entry.location,
        reference_location: reference_resolution.entry&.location,
        alias_target: resolution.alias_target,
        alias_location: resolution.alias_target && resolution.entry&.location
      )
    end

    def branch_difference_allowed?(locale, key, branch)
      branches = @allowed_branch_differences[locale]
      return false unless branches

      Array(branches[:all]).include?(branch) || Array(branches[key]).include?(branch)
    end

    def normalize_branch_differences(config)
      config.each_with_object({}) do |(locale, value), normalized|
        normalized[locale.to_sym] =
          case value
          when Array
            { all: value.map(&:to_sym) }
          when Hash
            value.each_with_object({}) { |(key, branches), hash| hash[key.to_s] = branches.map(&:to_sym) }
          else
            raise ArgumentError, "allowed_branch_differences must map locales to Arrays or Hashes"
          end
      end
    end

    def each_locale_entry(key)
      @translations.each do |locale, entries|
        next if locale == @reference_locale

        entry = entries[key]
        yield locale, entry if entry
      end
    end
  end
end
