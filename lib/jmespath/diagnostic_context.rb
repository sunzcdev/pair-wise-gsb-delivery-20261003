# frozen_string_literal: true
module JMESPath
  # @api private
  #
  # Collects diagnostics while an expression is evaluated in diagnostic
  # mode. The active context is stored in a thread local so the node
  # classes can record entries without changing their `visit` signatures.
  class DiagnosticContext
    THREAD_KEY = :jmespath_diagnostic_context

    def self.active?
      !Thread.current[THREAD_KEY].nil?
    end

    def self.current
      Thread.current[THREAD_KEY]
    end

    # Activates a context for the duration of the block. Returns the
    # block result together with the populated context.
    def self.activate
      previous = Thread.current[THREAD_KEY]
      context = new
      Thread.current[THREAD_KEY] = context
      begin
        result = yield
      ensure
        Thread.current[THREAD_KEY] = previous
      end
      [result, context]
    end

    # Recursively replaces the MISSING sentinel with `nil` so diagnostic
    # reports only contain JSON serializable values.
    def self.sanitize(value)
      if value.equal?(MISSING)
        nil
      elsif value.respond_to?(:to_ary)
        value.to_ary.map { |element| sanitize(element) }
      elsif value.respond_to?(:to_hash)
        value.to_hash.each_with_object({}) do |(key, element), hash|
          hash[key] = sanitize(element)
        end
      else
        value
      end
    end

    def initialize
      @entries = []
    end

    # @return [Array<Hash>] JSON serializable diagnostic entries.
    attr_reader :entries

    def record(path, reason, detail = nil)
      entry = { 'path' => path, 'reason' => reason }
      entry['detail'] = detail if detail
      @entries << entry unless @entries.include?(entry)
      nil
    end
  end
end
