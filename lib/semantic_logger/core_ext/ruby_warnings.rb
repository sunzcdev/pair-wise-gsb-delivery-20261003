module SemanticLogger
  module CoreExt
    # Routes Ruby's own warnings, the ones Ruby passes to `Warning.warn` with a category, into
    # Semantic Logger as ordinary `:warn` entries, so that the JSON log can tell a native Ruby
    # warning apart from an application's own `logger.warn`.
    #
    # Ruby warns with a category for two kinds of warning (see CATEGORIES):
    # - `:deprecated`, such as a deprecated Ruby feature or gem API, while `Warning[:deprecated]`
    #   is on.
    # - `:experimental`, such as an experimental language feature, while `Warning[:experimental]`
    #   is on.
    # Both are logged at `:warn`, the level of an application warning, with the category in
    # `Log#warning_category`. A warning without a category, and one with another category, is not
    # touched: the next `Warning.warn` in the chain handles it exactly as before.
    #
    # The warning text Ruby writes starts with the file and line that raised it, such as
    # "app/models/order.rb:42: warning: ...", which Ruby adds before calling `Warning.warn`. That
    # location is parsed into the entry's call site (see Log#call_site), and the entry's message is
    # the warning text without it, so the location reaches every formatter as structured data.
    #
    # Enabling this module only switches its flag on after prepending it to `Warning`; it never
    # changes `Warning[:deprecated]` or `Warning[:experimental]`, so which warnings Ruby raises
    # stays whatever the process set. A warning the process switched off never reaches
    # `Warning.warn`, so it is never logged. While disabled, the module passes every warning on
    # unchanged, as if it were not there.
    #
    # Prepended to `Warning` only when first enabled (see .enabled=), so that loading Semantic
    # Logger changes nothing until an application asks for it, and a `Warning.warn` the application
    # defines before or afterwards keeps seeing every warning this module does not log.
    module RubyWarnings
      # The categories of Ruby warning this module logs, the only categories Ruby itself warns
      # with, see `Warning.[]`. A warning with another category, or none, is passed on unchanged.
      CATEGORIES = %i[deprecated experimental].freeze

      # The name of the logger that logs the captured warnings, so that their entries have one
      # source name, separate from any class the application logs under.
      LOGGER_NAME = "Ruby Warning".freeze

      # The level at which a captured warning is logged: the level of an application's own warning.
      LEVEL = :warn

      # A native warning as Ruby passes it to `Warning.warn`: "path:line: warning: text\n". The
      # path is everything up to the last ":line:", since a Windows path such as "C:/app/a.rb:12"
      # holds a colon too. The text keeps the trailing line break Ruby adds until #parse chomps it.
      LOCATED_WARNING = /\A(.+):(\d{1,10}): warning: (.*)\z/m

      @enabled = false

      class << self
        # Whether Ruby's categorized warnings are logged as Semantic Logger entries. Enabling it
        # prepends this module to `Warning` once and switches the interception on; disabling it
        # switches the interception off without removing the module, so that every warning is again
        # passed on unchanged. Enabling it twice changes nothing.
        def enabled=(enabled)
          if enabled
            # Leading :: is required: inside CoreExt the bare constant Warning resolves to
            # SemanticLogger::CoreExt::Warning, the module that drops the json gem's warning.
            singleton = ::Warning.singleton_class
            singleton.prepend(self) unless singleton.ancestors.include?(self)
            @enabled = true
          else
            @enabled = false
          end
        end

        # Whether Ruby's categorized warnings are currently routed into Semantic Logger.
        def enabled?
          @enabled == true
        end

        # The Semantic Logger that receives a captured warning. Built lazily, like any logger.
        def logger
          @logger ||= SemanticLogger[LOGGER_NAME]
        end

        # Logs one captured warning as an ordinary `:warn` entry, carrying its category and the
        # location Ruby named in its text. Goes through the logger's own #should_log?, so the
        # global default level and any filter on this logger apply exactly as they do to an
        # application's `logger.warn`; each appender then applies its own level and filter.
        # Returns nil, as `Warning.warn` does.
        def log_warning(message, category)
          text                = message.is_a?(String) ? message : message.to_s
          call_site, stripped = parse(text)

          log                  = SemanticLogger::Log.new(LOGGER_NAME, LEVEL)
          log.assign(message: stripped)
          log.call_site        = call_site
          log.warning_category = category
          logger.log(log) if logger.should_log?(log)
          nil
        end

        private

        # Returns [CallSite|nil, String] the location the warning names, parsed as a call site, and
        # the warning text without its "path:line: warning: " prefix and trailing line break. A
        # message that does not start with that prefix, such as one built by hand, keeps its text,
        # chomped of a trailing line break, and has no call site.
        def parse(text)
          match = LOCATED_WARNING.match(text)
          return [nil, text.chomp("\n")] unless match

          call_site = CallSite.parse("#{match[1]}:#{match[2]}")
          [call_site, match[3].chomp("\n")]
        end
      end

      # Called by Ruby for each warning. Logs a categorized warning Ruby itself raised while
      # enabled, and passes every other warning on unchanged.
      #
      # The category is passed on only when every #warn further down the chain accepts one, as
      # Ruby itself does. One that takes only the message, which an application may define, gets
      # the message alone (see CoreExt::Warning). The whole chain is walked, since another module,
      # such as that one, can sit between this module and it.
      def warn(message, *, **options)
        if SemanticLogger::CoreExt::RubyWarnings.enabled? &&
           SemanticLogger::CoreExt::RubyWarnings::CATEGORIES.include?(options[:category])
          SemanticLogger::CoreExt::RubyWarnings.log_warning(message, options[:category])
        elsif message_only_chain?
          super(message)
        else
          super
        end
      end

      private

      # Whether a #warn further down the method chain takes only the message, so the category
      # keyword must not be passed to it. Ruby's own Warning.warn and Kernel#warn accept keywords,
      # so the chain ends on a method that does.
      def message_only_chain?
        method = method(:warn).super_method
        while method
          return true if method.arity == 1

          method = method.super_method
        end
        false
      end
    end
  end
end
