# frozen_string_literal: true

require_relative "sql_migration"

module ROM
  module SQL
    module Migration
      # SQL migrations have a very simple grammar:
      #
      # 1. Files are organized into two directions (up and down). Each
      #    direction may contain one or more `begin..end` sections; all
      #    sections for a direction execute sequentially in source order
      #    at apply time. Each section keeps its own `split=` setting,
      #    so a single direction may mix splitting strategies across
      #    sections.
      # 2. Directives are written as SQL line comments using the `-- @migrate`
      #    sigil. The leading `--` keeps directive lines valid SQL syntax (so
      #    editors, syntax highlighters, and linters treat them as comments);
      #    the `@migrate` sigil disambiguates them from author-written `--`
      #    comments. Plain `-- ...` comments without the `@migrate` sigil are
      #    treated as part of the SQL body and pass through verbatim.
      # 3. Optional settings may be provided in a pragma line before the up/down sections
      # 4. Pragma and begin lines accept `key=value` settings separated by spaces or tabs
      # 5. Each direction begins with a direction directive followed by one
      #    or more begin directives
      # 6. The begin directive may itself carry settings (e.g. statement splitting strategy)
      # 7. Each section concludes with an end directive
      # 8. Down migrations are optional, but they always must follow the up migration
      # 9. Empty sections (begin..end with no SQL body) are silently skipped
      # 10. A checkpoint directive may appear between sections to declare a
      #     verifiable step boundary (see "Resumable migrations")
      #
      # Recognized settings:
      #
      # * Pragma line: `transaction=true|false` controls whether the migration runs
      #   inside a transaction. Any other value raises `SQLParseError`. Unknown
      #   pragma keys are silently ignored.
      # * Pragma line: `env=true|false` enables environment-variable substitution
      #   in statement bodies (see "Variable substitution"). Any other value raises
      #   `SQLParseError`.
      # * Begin line: `split=semicolon|line` controls statement splitting for the
      #   section's body. Default is to send the entire body to the database in a
      #   single call. Any other value raises `SQLParseError`. Unknown begin keys
      #   are silently ignored.
      #
      # Resumable migrations:
      #
      # * A migration that must run outside a transaction (`transaction=false`)
      #   can mark verifiable step boundaries with a checkpoint directive placed
      #   between sections:
      #
      #       -- @migrate checkpoint verify="SELECT ..."
      #
      #   The `verify` query must return at least one row when the preceding
      #   step's effects are in place. On a re-run after a failure, recorded
      #   steps whose verification still holds are skipped and only the
      #   remaining steps execute.
      # * Progress is persisted in the `rom_sql_migration_checkpoints` table
      #   together with a SHA-256 checksum of the migration file. Editing the
      #   file after an interrupted run makes the stored checksum mismatch and
      #   raises `MigrationResumeError`; the migration must be repaired
      #   manually rather than resumed against a different script.
      # * A recorded step whose verification query no longer returns a row
      #   raises `MigrationCheckpointError`: the persisted progress does not
      #   match the target state.
      # * Checkpoints are only meaningful outside a transaction, so a
      #   checkpoint directive in a migration without `transaction=false`
      #   raises `SQLParseError`. Ordinary transactional migrations keep
      #   running statement-by-statement without any persisted state.
      # * The verification SQL is a double-quoted string. Backslash escapes
      #   `\"` and `\\` are honored inside it; no other directive may share
      #   the checkpoint line.
      #
      # Variable substitution:
      #
      # * With the `env=true` pragma, occurrences of `${NAME}` in statement bodies
      #   are replaced with `ENV["NAME"]` when the migration is applied. Names match
      #   `[A-Za-z_][A-Za-z0-9_]*`.
      # * Only the braced form `${NAME}` is substituted. Bare `$NAME`, `$$`-quoted
      #   bodies, and `$1` positional parameters are left untouched, so the feature
      #   is safe to use alongside PostgreSQL function bodies and prepared params.
      # * Substitution happens at apply time, so values reflect the environment when
      #   the migration runs. All of a direction's statements are resolved before
      #   any are executed, so a missing variable aborts before any side effects.
      # * A `${NAME}` with no corresponding `ENV` entry raises `SQLEnvError`.
      # * Values are inserted as raw SQL text (not quoted or escaped); the
      #   environment is trusted input.
      #
      # Splitting caveats:
      #
      # * `split=semicolon` splits on `;` followed by end of line. It does **not**
      #   track string literals or `$$`-quoted bodies; semicolons inside those will
      #   be mistaken for statement terminators. Use the default single-statement
      #   mode for migrations containing such content.
      # * `split=line` emits each non-empty line as a separate statement. Plain
      #   SQL comment lines (`-- ...` without the `@migrate` sigil) are passed
      #   through verbatim and will each be sent to the database as their own
      #   (no-op) statement.
      # * Adjacent settings on the same directive line are separated by one or
      #   more space or tab characters. Trailing whitespace after the last
      #   setting is allowed. Any other content on a directive line raises
      #   `SQLParseError`.
      # * Windows-style line endings (`\r\n`) are normalized to `\n` at read time.
      # * The directive prefix requires exactly one space between `--` and
      #   `@migrate` (i.e. `-- @migrate`). A bare `@migrate` at the start of a
      #   line (no `--` prefix) is treated as SQL content.
      # * A body line of the form `[whitespace]word=word` is lexed as a setting,
      #   not SQL. Immediately after a begin directive such a line is consumed as
      #   a (possibly ignored) section setting rather than emitted as a statement.
      #
      # ABNF Grammar (RFC 5234 core rules: ALPHA, DIGIT, SP, HTAB, WSP, LF):
      #
      #     ; The format is line-oriented. A blank line (LF alone) yields no
      #     ; tokens and may appear between any lines; blank lines are omitted
      #     ; below for clarity. The final line's terminating LF is optional
      #     ; (EOF may stand in for it).
      #     ;
      #     ; Disambiguation is by lexer precedence: each physical line is
      #     ; matched against the directive, pragma, then setting productions
      #     ; (in that order); a line matching none of them is a sql-line. ABNF
      #     ; cannot express that exclusion, so sql-line is descriptive.
      #
      #     input          = *pragma-line up-direction [down-direction]
      #
      #     up-direction   = up-line   1*sql-section
      #     down-direction = down-line 1*sql-section
      #     sql-section    = begin-line *sql-line end-line
      #                      [checkpoint-line]
      #
      #     ; Direction directives take no settings; only trailing whitespace
      #     ; may follow.
      #
      #     up-line         = "-- @migrate up"   trailer newline
      #     down-line       = "-- @migrate down" trailer newline
      #     end-line        = "-- @migrate end"  trailer newline
      #     checkpoint-line = "-- @migrate checkpoint" gap
      #                       "verify=" quoted-string trailer newline
      #
      #     ; begin and pragma lines may carry whitespace-separated settings.
      #
      #     begin-line  = "-- @migrate begin" *( gap setting ) trailer newline
      #     pragma-line = "-- @migrate"       *( gap setting ) trailer newline
      #
      #     setting        = word "=" word
      #     quoted-string  = DQUOTE *( unescaped / ESC escapee ) DQUOTE
      #     escapee        = DQUOTE / "\"
      #     word           = 1*word-char
      #     word-char      = ALPHA / DIGIT / "_"
      #     sql-line       = 1*line-char newline
      #
      #     ; Terminals
      #
      #     newline   = LF
      #     gap       = 1*WSP             ; whitespace separating settings
      #     trailer   = *WSP              ; optional trailing whitespace
      #     line-char = %x01-09 / %x0B-FF ; any byte except LF
      #
      # @api private
      class SQLParser
        NEWLINE = "\n"

        # Matches a trailing `;` (with optional trailing horizontal whitespace)
        # at the end of a statement buffer. `split=semicolon` treats an
        # end-of-line `;` as a statement terminator.
        SEMICOLON = /;[ \t]*\z/

        # Each TOKENS row is `[type, regex]`. Every regex must include a
        # `(?<value>...)` named capture identifying the substring that becomes
        # the token's value and position. The tokenizer reads
        # `match[:value]` for the value and `match.begin(:value)` for the
        # column; `lpos` advances by `match.end(0)`, which may exceed the
        # value's span when the regex consumes more (e.g. leading whitespace
        # as a separator for the `:setting` regex).
        #
        # IMPORTANT: order matters. The `:pragma` regex matches the prefix
        # of every more-specific directive (`:up`, `:down`, `:begin`,
        # `:end`). The tokenizer uses first-match-wins, so the specific
        # directives MUST appear before `:pragma`. When adding a new
        # `-- @migrate <verb>` directive, insert its row above the `:pragma`
        # row or it will be silently tokenized as a pragma.
        TOKENS = [
          [:up, /\A(?<value>-- @migrate up)\b/i],
          [:down, /\A(?<value>-- @migrate down)\b/i],
          [:begin, /\A(?<value>-- @migrate begin)\b/i],
          [:end, /\A(?<value>-- @migrate end)\b/i],
          [:checkpoint, /\A(?<value>-- @migrate checkpoint)\b/i],
          [:pragma, /\A(?<value>-- @migrate)\b/i],
          [:setting, /\A[ \t]+(?<value>\w+=\w+)\b/i],
        ]

        # Matches the required `verify="..."` pair following a checkpoint
        # directive. Backslash escapes (`\"`, `\\`) are honored so a
        # verification query may contain quotes.
        CHECKPOINT_VERIFY = /\A[ \t]+verify="(?<value>(?:\\.|[^"\\])*)"/m

        TRAILING_WHITESPACE = /\A[ \t]*(?:\n|\z)/

        class Token < String
          attr_reader :type, :line, :pos

          def initialize(value, type:, line:, pos:)
            super(value)

            @type = type
            @line = line
            @pos  = pos

            freeze
          end

          def value = to_s
          def inspect = "(#{type} #{super} @#{line_pos})"
          def end = pos + length
          def type?(name) = type == name
          def line_pos = "#{line}:#{pos}"
        end

        # Translate a `.sql` migration file into an `SQLMigration`.
        #
        # @param path [String, Pathname] absolute path to the `.sql` file
        # @return [SQLMigration]
        #
        # @api private
        def call(path)
          @path  = path
          tokens = File.open(path, "r") { |file| tokenize(file) }

          unless tokens.any? { |t| t.type != :line }
            raise SQLParseError, "No @migrate directives found in #{path}"
          end

          settings   = parse_pragmas(tokens)
          up_sections, up_checkpoints = parse_direction(:up, tokens)
          down_sections, down_checkpoints = parse_direction(:down, tokens)

          if (stray = tokens.first)
            raise SQLParseError,
                  "Unexpected content after final section in #{path}: " \
                  "#{stray.type} at #{location(path, stray.line, stray.pos)}"
          end

          validate_checkpoint_settings(settings, up_checkpoints + down_checkpoints)

          attributes = {
            filename: path,
            settings: settings,
            up: flatten(up_sections)
          }

          unless down_sections.empty?
            attributes[:down] = flatten(down_sections)
          end

          steps = build_steps(up_sections, up_checkpoints, down_sections, down_checkpoints)
          attributes[:steps] = steps unless steps.empty?

          SQLMigration.new(**attributes)
        end

        def tokenize(file)
          lno = 0
          tokens = []

          until file.eof?
            # Normalize line endings
            line = file.gets.sub(/\r\n\z/, NEWLINE)
            lno += 1
            lpos = 0

            while lpos < line.length
              remainder = line[lpos..]

              break if remainder == NEWLINE

              match_type, match_data = nil
              TOKENS.each do |type, regex|
                if (match = remainder.match(regex))
                  match_type = type
                  match_data = match
                  break
                end
              end

              if match_data
                tokens << Token.new(
                  match_data[:value],
                  type: match_type,
                  line: lno,
                  pos: lpos + match_data.begin(:value)
                )
                lpos += match_data.end(0)

                # A checkpoint directive carries an inline quoted
                # verification query; consume the rest of the line as a
                # single verify token rather than lexing it as settings.
                if match_type == :checkpoint
                  verify, consumed = parse_checkpoint_verify(remainder[lpos..], lno)
                  tokens << Token.new(verify, type: :verify, line: lno, pos: lpos)
                  lpos += consumed
                end
              elsif lpos.zero?
                # No directive matched at start of line: treat as a SQL line.
                tokens << Token.new(line.chomp(NEWLINE), type: :line, line: lno, pos: lpos)
                lpos = line.length
              elsif remainder.match?(TRAILING_WHITESPACE)
                # Trailing whitespace after the last setting on a directive
                # line is allowed.
                break
              else
                raise SQLParseError,
                      "Unrecognized content #{remainder.chomp.inspect} " \
                      "on directive line at #{location(@path, lno, lpos)}"
              end
            end
          end

          tokens
        end

        # Parse the `verify="..."` pair that must follow a checkpoint
        # directive, returning the unescaped query and the number of
        # characters consumed from the line remainder.
        #
        # @api private
        def parse_checkpoint_verify(remainder, lno)
          match = remainder.match(CHECKPOINT_VERIFY) or
            raise SQLParseError,
                  "Checkpoint directive requires a verify=\"...\" pair " \
                  "at #{location(@path, lno, 0)}"

          trailing = remainder[match.end(0)..]
          unless trailing.nil? || trailing.match?(TRAILING_WHITESPACE)
            raise SQLParseError,
                  "Unrecognized content #{trailing.chomp.inspect} after " \
                  "checkpoint verify at #{location(@path, lno, match.end(0))}"
          end

          [unescape(match[:value]), match.end(0)]
        end

        # Decode the backslash escapes accepted inside a quoted verify value.
        #
        # @api private
        def unescape(value)
          value.gsub(/\\(.)/, '\1')
        end

        def parse_pragmas(tokens)
          settings = {}

          while next?(:pragma, tokens)
            consume(:pragma, tokens)

            while next?(:setting, tokens)
              case parse_setting(tokens)
              in [:transaction, "true" | "false" => value]
                settings[:transaction] = value == "true"
              in [:env, "true" | "false" => value]
                settings[:env] = value == "true"
              in [(:transaction | :env) => key, value]
                raise SQLParseError,
                      %(#{key}= must be "true" or "false", got #{value.inspect} in #{@path})
              else
                # ignore unknown pragma keys
              end
            end
          end

          settings
        end

        def parse_settings(tokens)
          settings = {}

          while next?(:setting, tokens)
            key, value = parse_setting(tokens)
            settings[key] = value
          end

          settings
        end

        def parse_setting(tokens)
          key, value = consume(:setting, tokens).value.split("=", 2)
          [key.to_sym, value]
        end

        # Parse a full direction (`up` is required, `down` optional),
        # returning its sections and `[section_index, verify_token]`
        # checkpoint pairs in source order.
        #
        # @return [Array(Array<Array<Statement>>, Array<Array(Integer, Token)>)]
        #
        # @api private
        def parse_direction(name, tokens)
          return [[], []] if name == :down && !next?(:down, tokens)

          consume(name, tokens)

          sections = []
          checkpoints = []

          while next?(:begin, tokens)
            index = sections.length
            sections << parse_sql(tokens)

            if next?(:checkpoint, tokens)
              consume(:checkpoint, tokens)
              checkpoints << [index, consume(:verify, tokens)]
            end
          end

          [sections, checkpoints]
        end

        # @api private
        def validate_checkpoint_settings(settings, checkpoints)
          return if checkpoints.empty?

          if (_, verify = checkpoints.first) && verify.value.strip.empty?
            raise SQLParseError,
                  "Checkpoint verify query cannot be empty " \
                  "at #{verify.line_pos} in #{@path}"
          end

          return if settings[:transaction] == false

          _, first_verify = checkpoints.first

          raise SQLParseError,
                "Checkpoint directives require transaction=false so that " \
                "completed steps stay committed if a later step fails " \
                "(found at #{first_verify.line_pos} in #{@path})"
        end

        # Build per-direction `Step` lists from parsed sections. A
        # checkpoint following a section closes that section's step; a
        # section without one forms a step with no verification.
        #
        # @api private
        def build_steps(up_sections, up_checkpoints, down_sections, down_checkpoints)
          steps = {}

          if up_checkpoints.any?
            steps[:up] = steps_for(up_sections, up_checkpoints)
          end
          if down_checkpoints.any?
            steps[:down] = steps_for(down_sections, down_checkpoints)
          end

          steps
        end

        # @api private
        def steps_for(sections, checkpoint_pairs)
          checkpoints_by_index = checkpoint_pairs.to_h { |index, token|
            [index, Checkpoint.new(verify: token.value, line: token.line)]
          }

          sections.each_with_index.map do |statements, index|
            Step.new(
              index: index,
              statements: statements,
              checkpoint: checkpoints_by_index[index]
            )
          end
        end

        # Flatten section statement lists into the single array stored by
        # `SQLMigration#up`/`#down`, preserving source order.
        #
        # @api private
        def flatten(sections)
          sections.flatten
        end

        # Parse a single `begin..end` section into an array of `Statement`s,
        # applying the section's `split=` strategy.
        #
        # @api private
        def parse_sql(tokens)
          consume(:begin, tokens)
          settings = parse_settings(tokens)

          line_tokens = []

          while next?(:line, tokens)
            line_tokens << consume(:line, tokens)
          end

          consume(:end, tokens)

          return [] if line_tokens.empty?

          case settings.fetch(:split, :absent)
          when :absent
            [Statement.new(sql: join(line_tokens), line: line_tokens.first.line)]
          when "line"
            line_tokens
              .reject { |token| token.value.strip.empty? }
              .map { |token| Statement.new(sql: token.value, line: token.line) }
          when "semicolon"
            line_tokens
              .slice_after { |token| token.value.match?(SEMICOLON) }
              .filter_map { |group|
                sql = join(group).sub(SEMICOLON, "")
                Statement.new(sql: sql, line: group.first.line) unless sql.strip.empty?
              }
          else
            raise SQLParseError, %(Unknown split strategy #{settings[:split].inspect}: expected "semicolon" or "line" in #{@path})
          end
        end

        # Join line tokens back into a single SQL string.
        #
        # @api private
        def join(line_tokens)
          line_tokens.map(&:value).join(NEWLINE)
        end

        def next?(type, tokens)
          return false if tokens.empty?

          tokens.first.type?(type)
        end

        def consume(type, tokens)
          if next?(type, tokens)
            tokens.shift
          elsif tokens.empty?
            raise SQLParseError,
                  "Unexpected end of input in #{@path}, " \
                  "expected token of type #{type.inspect}"
          else
            token = tokens[0]
            raise SQLParseError, "Expected #{type}, got #{token.type} " \
                  "at #{location(@path, token.line, token.pos)}"
          end
        end

        def location(path, line, col) = "#{path}:#{line}:#{col}"
      end
    end
  end
end
