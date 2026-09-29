# frozen_string_literal: true

module I18n
  # I18n::Explain implements the +explain+ query which collects, alongside a
  # regular translation, a step-by-step trace of how the translation was
  # resolved: which backends were asked, for which locales and keys, which
  # fallback locales and cascade levels were tried, which symbol aliases were
  # followed and - finally - which storage location produced the hit.
  #
  # The trace is collected during the translation itself. Backends record
  # their steps into a per-call Trace object that is passed through the
  # +explain+ option, so generating an explanation never re-queries a backend
  # and never runs interpolation callables an extra time. No global state is
  # written: each I18n.explain call returns its own Result.
  module Explain
    # A single recorded resolution step, e.g. a backend lookup, a fallback
    # locale being tried, a cascade level or a symbol alias being followed.
    class Step
      attr_reader :action, :backend, :locale, :key, :detail

      def initialize(action, backend:, locale:, key:, **detail)
        @action  = action
        @backend = backend
        @locale  = locale
        @key     = key
        @detail  = detail
      end

      # True when this step produced the translation that was returned.
      def hit?
        !!detail[:hit]
      end

      # Marks this step as the one that produced the translation.
      def hit!
        detail[:hit] = true
      end

      # The storage location that was consulted by this step (e.g. the
      # translations hash of a Simple backend), if the backend exposed it.
      def store
        detail[:store]
      end

      def to_s
        parts = [action.to_s, "backend=#{backend.inspect}", "locale=#{locale.inspect}", "key=#{key.inspect}"]
        detail.each do |name, value|
          next if name == :store
          parts << (name == :hit && value ? 'HIT' : "#{name}=#{value.inspect}")
        end
        parts << "store=#{store.class}" if store
        parts.join(' ')
      end
    end

    # Collects the steps of a single explain query. A fresh Trace is created
    # per I18n.explain call and handed through the +explain+ option, so no
    # global state is involved.
    class Trace
      attr_reader :steps

      def initialize
        @steps = []
      end

      def record(action, **attributes)
        step = Step.new(action, **attributes)
        @steps << step
        step
      end

      # The step that produced the returned translation, i.e. the one that
      # points at the storage location which was finally hit. When symbol
      # aliases were followed, this is the innermost lookup, i.e. the place
      # where the returned text is actually stored.
      def hit
        @steps.find { |step| step.hit? && step.store }
      end

      def hit?
        !hit.nil?
      end

      def to_a
        @steps.map(&:to_s)
      end
    end

    # The return value of I18n.explain. Keeps the translated value and its
    # explanation strictly separate.
    class Result
      attr_reader :translation, :trace

      def initialize(translation, trace)
        @translation = translation
        @trace       = trace
      end

      def hit?
        trace.hit?
      end
    end
  end
end
