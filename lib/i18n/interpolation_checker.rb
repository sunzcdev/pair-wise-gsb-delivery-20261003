# frozen_string_literal: true

require 'yaml'
require 'json'

module I18n
  # Checks a directory of locale data files for inconsistent interpolation
  # parameters across locales, using one locale as the reference. This is
  # meant to catch mistakes like +%{user_name}+ in the English source but
  # +%{username}+ in a translation before the data is released.
  #
  # The checker understands plural branches (hashes made up of plural
  # categories such as +one+/+other+), escaped percent signs (+%%+) and
  # Symbol aliases (entries whose value is a Symbol pointing at another key
  # of the same locale). It only reads YAML and JSON files - Ruby locale
  # files are ignored and callable values are never called - so it is safe
  # to run against untrusted directories.
  #
  #   checker = I18n::InterpolationChecker.new(
  #     reference_locale: :en,
  #     allowed_branch_differences: {
  #       'inbox.messages' => { one: { missing: %i[count] } }
  #     }
  #   )
  #   issues = checker.check_dir('config/locales')
  #
  # The result is a list of InterpolationChecker::Issue objects describing
  # the entry, the locale, the parameters that are missing or extra compared
  # to the reference locale, and where the involved translations can be
  # found. For entries resolved through a Symbol alias the issue also points
  # at the location of the shared alias target.
  class InterpolationChecker
    # Plural categories as defined by CLDR, plus the explicit "0"/"1" keys.
    PLURAL_BRANCHES = %i[zero one two few many other 0 1].freeze

    # The same patterns the runtime interpolator uses, so that e.g.
    # <tt>"%%{name}"</tt> is an escaped literal and not a placeholder.
    INTERPOLATION_PATTERN = Regexp.union(
      /%%/,
      /%\{([\w|]+)\}/,                            # matches placeholders like "%{foo} or %{foo|word}"
      /%<(\w+)>([^\d]*?\d*\.?\d*[bBdiouxXeEfgGcps])/  # matches placeholders like "%<foo>.d"
    )

    LOCALE_FILE_PATTERN = '**/*.{yml,yaml,json}'
    private_constant :LOCALE_FILE_PATTERN

    # A structured description of one inconsistency found by the checker.
    class Issue
      attr_reader :kind, :key, :locale, :reference_locale, :branch,
                  :missing, :extra, :source, :reference_source,
                  :alias_of, :alias_source

      def initialize(kind: :params, key:, locale:, reference_locale:, branch: nil,
                     missing: [], extra: [], source: nil, reference_source: nil,
                     alias_of: nil, alias_source: nil)
        @kind = kind
        @key = key
        @locale = locale
        @reference_locale = reference_locale
        @branch = branch
        @missing = missing
        @extra = extra
        @source = source
        @reference_source = reference_source
        @alias_of = alias_of
        @alias_source = alias_source
      end

      def to_h
        {
          kind: kind, key: key, locale: locale, reference_locale: reference_locale,
          branch: branch, missing: missing, extra: extra,
          source: source, reference_source: reference_source,
          alias_of: alias_of, alias_source: alias_source
        }
      end

      def ==(other)
        other.is_a?(Issue) && to_h == other.to_h
      end
      alias eql? ==

      def hash
        to_h.hash
      end

      def to_s
        location = branch ? "#{key} (#{branch})" : key
        details =
          if kind == :unresolved_alias
            "unresolved alias #{alias_of.inspect}"
          else
            parts = []
            parts << "missing: #{missing.map(&:inspect).join(', ')}" unless missing.empty?
            parts << "extra: #{extra.map(&:inspect).join(', ')}" unless extra.empty?
            parts.join('; ')
          end
        "#{locale}: #{location} - #{details} (#{source})"
      end
      alias inspect to_s
    end

    # A leaf translation holding a plain string.
    class StringEntry < Struct.new(:params, :source)
    end

    # A leaf translation aliasing another key of the same locale.
    class AliasEntry < Struct.new(:target, :source)
    end

    # A translation made up of plural branches.
    class PluralEntry < Struct.new(:branches, :source)
    end

    # The flattened entries of one locale, with alias resolution.
    class LocaleData
      attr_reader :entries

      def initialize
        @entries = {}
      end

      # Returns a hash of plural branch (or +nil+ for plain strings) to the
      # list of interpolation parameters for the given dotted key, following
      # Symbol aliases. Returns +nil+ when the key is unknown or its alias
      # chain cannot be resolved.
      def branches_for(key, seen = [])
        entry = entries[key]
        return nil unless entry

        case entry
        when AliasEntry
          return nil if seen.include?(entry.target)

          branches_for(entry.target, seen + [key])
        when StringEntry
          { nil => entry.params }
        when PluralEntry
          entry.branches.each_with_object({}) do |(branch, value), result|
            result[branch] = branch_params(value, seen + [key])
          end
        end
      end

      # When the entry stored under +key+ is a Symbol alias, returns the
      # resolved target key and the entry it points to, following alias
      # chains. Returns +nil+ when the key is not an alias or the chain
      # cannot be resolved.
      def alias_resolution(key)
        entry = entries[key]
        return nil unless entry.is_a?(AliasEntry)

        seen = [key]
        while entry.is_a?(AliasEntry)
          return nil if seen.include?(entry.target)

          seen << entry.target
          target = entries[entry.target]
          return nil unless target
          return [entry.target, target] unless target.is_a?(AliasEntry)

          entry = target
        end
      end

      private

      def branch_params(value, seen)
        case value
        when StringEntry
          value.params
        when AliasEntry
          return [] if seen.include?(value.target)

          (branches_for(value.target, seen) || {}).values.flatten.uniq
        else
          []
        end
      end
    end

    attr_reader :reference_locale

    def initialize(reference_locale:, allowed_branch_differences: {})
      @reference_locale = reference_locale.to_sym
      @allowed_branch_differences = normalize_allowed(allowed_branch_differences)
    end

    # Reads every YAML/JSON locale file under +dir+ (recursively) and
    # returns a sorted list of Issue objects. Never evaluates Ruby files
    # and never calls callable values found in the data.
    def check_dir(dir)
      locales = load_locales(dir)
      unless locales.key?(reference_locale)
        raise ArgumentError, "reference locale #{reference_locale.inspect} not found in #{dir}"
      end

      issues = []
      reference = locales[reference_locale]

      reference.entries.each_key do |key|
        reference_branches = reference.branches_for(key)
        next unless reference_branches

        (locales.keys - [reference_locale]).each do |locale|
          data = locales[locale]
          next unless data.entries.key?(key)

          branches = data.branches_for(key)
          if branches.nil?
            issues << unresolved_alias_issue(key, locale, data)
            next
          end

          issues.concat(compare(key, locale, reference_branches, branches, reference, data))
        end
      end

      issues.sort_by { |issue| [issue.key, issue.locale.to_s, issue.branch.to_s] }
    end
    alias check check_dir

    private

    def load_locales(dir)
      locales = Hash.new { |hash, locale| hash[locale] = LocaleData.new }

      Dir.glob(File.join(dir, LOCALE_FILE_PATTERN)).sort.each do |file|
        data = load_file(file)
        next unless data.is_a?(Hash)

        data.each do |locale, translations|
          next unless translations.is_a?(Hash)

          flatten(Utils.deep_symbolize_keys(translations), nil, file, locales[locale.to_sym].entries)
        end
      end

      locales
    end

    def load_file(file)
      case File.extname(file)
      when '.json'
        JSON.parse(File.read(file))
      else
        YAML.safe_load(File.read(file), permitted_classes: [Symbol], aliases: true)
      end
    end

    def flatten(hash, prefix, file, entries)
      hash.each do |key, value|
        dotted = prefix ? "#{prefix}.#{key}" : key.to_s

        if value.is_a?(Hash) && !plural_hash?(value)
          flatten(value, dotted, file, entries)
        else
          entry = build_entry(value, dotted, file)
          entries[dotted] = entry if entry
        end
      end
    end

    def build_entry(value, dotted, file)
      case value
      when String
        StringEntry.new(extract_params(value), source(file, dotted))
      when Symbol
        AliasEntry.new(value.to_s, source(file, dotted))
      when Hash
        branches = value.each_with_object({}) do |(branch, branch_value), result|
          entry = build_entry(branch_value, "#{dotted}.#{branch}", file)
          result[branch.to_sym] = entry if entry
        end
        PluralEntry.new(branches, source(file, dotted))
      end
      # Numbers, booleans, arrays, callables and other values carry no
      # interpolation parameters and are never called - they are skipped.
    end

    def plural_hash?(hash)
      !hash.empty? && hash.keys.all? { |key| PLURAL_BRANCHES.include?(key) }
    end

    def extract_params(string)
      params = []
      string.gsub(INTERPOLATION_PATTERN) do |match|
        params << ($1 || $2 || match.tr('%{}', '')).to_sym unless match == '%%'
        match
      end
      params.uniq
    end

    def compare(key, locale, reference_branches, branches, reference, data)
      if reference_branches.keys == [nil] && branches.keys != [nil]
        branches = { nil => branches.values.flatten.uniq }
      elsif branches.keys == [nil] && reference_branches.keys != [nil]
        reference_branches = { nil => reference_branches.values.flatten.uniq }
      end

      (reference_branches.keys & branches.keys).filter_map do |branch|
        missing = reference_branches[branch] - branches[branch]
        extra = branches[branch] - reference_branches[branch]
        missing, extra = apply_allowed(key, branch, missing, extra)
        next if missing.empty? && extra.empty?

        alias_of, alias_source = alias_info(key, data)
        _, reference_target = reference.alias_resolution(key)

        Issue.new(
          key: key,
          locale: locale,
          reference_locale: reference_locale,
          branch: branch,
          missing: missing,
          extra: extra,
          source: source_of(data, key),
          reference_source: reference_target ? reference_target.source : source_of(reference, key),
          alias_of: alias_of,
          alias_source: alias_source
        )
      end
    end

    def unresolved_alias_issue(key, locale, data)
      entry = data.entries[key]
      Issue.new(
        kind: :unresolved_alias,
        key: key,
        locale: locale,
        reference_locale: reference_locale,
        source: entry.source,
        alias_of: entry.is_a?(AliasEntry) ? entry.target : nil
      )
    end

    def alias_info(key, data)
      resolution = data.alias_resolution(key)
      resolution ? [resolution[0], resolution[1].source] : [nil, nil]
    end

    def source_of(data, key)
      entry = data.entries[key]
      entry && entry.source
    end

    def source(file, dotted)
      "#{file}:#{dotted}"
    end

    def normalize_allowed(allowed)
      allowed.each_with_object({}) do |(key, branches), result|
        result[key.to_s] = branches.each_with_object({}) do |(branch, rule), normalized|
          rule = { missing: rule, extra: rule } if rule.is_a?(Array)
          normalized[branch.to_sym] = {
            missing: Array(rule[:missing] || rule['missing']).map(&:to_sym),
            extra: Array(rule[:extra] || rule['extra']).map(&:to_sym)
          }
        end
      end
    end

    def apply_allowed(key, branch, missing, extra)
      rules = @allowed_branch_differences[key]
      return [missing, extra] unless rules

      [rules[:any], rules[branch]].compact.each do |rule|
        missing -= rule[:missing]
        extra -= rule[:extra]
      end
      [missing, extra]
    end
  end
end
