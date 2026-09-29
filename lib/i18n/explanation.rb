# frozen_string_literal: true

module I18n
  # An Explanation describes how a single translate call resolved its key.
  #
  # While the backends walk their stores they record one step per action
  # (see I18n.explain): every backend that was asked, every fallback locale
  # that was tried, every cascade scope that was probed, every symbol alias
  # that was followed and every actual storage lookup. The last successful
  # lookup step (see #hit) points at the exact storage location the returned
  # translation was read from.
  #
  # Steps are recorded as a by-product of the translation lookup itself, so
  # collecting an explanation never triggers additional backend queries and
  # never evaluates interpolation callables a second time. Each I18n.explain
  # call uses its own Explanation instance - no global state is written.
  class Explanation
    attr_reader :key, :locale, :steps

    def initialize(key = nil, locale = nil)
      @key = key
      @locale = locale
      @steps = []
    end

    # Records a step. Called by backend implementations while they resolve
    # the translation. Returns the recorded step so callers can still amend
    # it (e.g. flag a lookup as found once the store has answered).
    def record(type, attributes = {})
      step = attributes.merge(:type => type)
      @steps << step
      step
    end

    # The step describing where the returned translation was actually read
    # from: the last successful storage lookup. nil when nothing was found.
    def hit
      @steps.reverse_each.find { |step| step[:type] == :lookup && step[:found] }
    end

    def hit?
      !hit.nil?
    end

    def to_s
      lines = steps.map { |step| describe(step) }
      lines << "hit: #{hit ? describe(hit) : 'none (translation missing)'}"
      lines.join("\n")
    end

    private

    def describe(step)
      details = step.reject { |name, _| name == :type }.map do |name, value|
        value = value.inspect if value.is_a?(Symbol) || value.is_a?(Array)
        "#{name}=#{value}"
      end
      "#{step[:type]}(#{details.join(', ')})"
    end
  end
end
