#!/usr/bin/env ruby
# frozen_string_literal: true

# Deterministic, local claim-coverage comparison for two Solid Queue configs.
#
# Setup writes a small SQLite fixture. The diagnostic scan then opens one
# query-only transaction and evaluates both configs through the real
# SolidQueue::QueueSelector. A second connection commits a late ready row while
# the candidate scan is mid-flight to prove that arrivals are not misreported
# as configuration differences.

require "bundler/setup"
require "optparse"
require "fileutils"
require "json"
require "logger"
require "securerandom"
require "time"

require "rails"
require "active_record"
require "active_job"
require "active_support/core_ext/string"
require "solid_queue"
require "zeitwerk"

ROOT = File.expand_path("..", __dir__)
SCHEMA = File.join(ROOT, "lib/generators/solid_queue/install/templates/db/queue_schema.rb")
MODEL_DIR = File.join(ROOT, "app/models/solid_queue")

RUNNING_CONFIG = %w[billing mail-* mail-transaction* legacy-* ops-*].freeze
CANDIDATE_CONFIG = %w[billing mail-transaction* mail-* ops-*].freeze

INITIAL_QUEUES = {
  "billing" => 1,
  "mail-transactional" => 1,
  "legacy-etl" => 1
}.freeze
PAUSED_QUEUES = %w[billing].freeze
LATE_QUEUE = "ops-late"

def load_solid_queue_models!
  loader = Zeitwerk::Loader.new
  loader.push_dir(MODEL_DIR, namespace: SolidQueue)
  loader.setup
end

class QueueCoverageReport
  STATUS_CLAIMABLE = "CLAIMABLE"
  STATUS_PAUSED = "CONFIGURED_BUT_PAUSED"
  STATUS_ABSENT = "CONFIGURATION_ABSENT"

  attr_reader :options, :events, :generated_at

  def initialize(options)
    @options = options
    @events = []
    @generated_at = Time.now.utc.iso8601(3)
  end

  def fixture?
    options[:fixture]
  end

  def database_path
    fixture? ? options[:fixture_db] : options[:existing_db]
  end

  def call
    if fixture?
      prepare_fixture_database!
      load_solid_queue_models!
      seed!
      run_shared_snapshot!(race: true)
      read_post_snapshot_state!
      verify_expectations!
    else
      connect_existing_database!
      load_solid_queue_models!
      run_shared_snapshot!(race: false)
      read_post_snapshot_state!
      @self_checks = nil
    end
    self
  end

  def self_checks = @self_checks

  def as_json
    {
      generated_at: generated_at,
      mode: fixture? ? "deterministic_fixture" : "existing_sqlite_read_only",
      database: database_path,
      reproduction_command: "bundle exec ruby bin/queue_coverage_report.rb --write reports/queue-coverage-report.md --json reports/queue-coverage-report.json",
      running_config: options[:running],
      candidate_config: options[:candidate],
      shared_snapshot: {
        ready_execution_counts: @snapshot_counts,
        paused_queues: @snapshot_paused,
        running: @running,
        candidate: @candidate
      },
      post_snapshot_read: @post_snapshot,
      self_checks: @self_checks,
      events: events
    }
  end

  def markdown
    ReportMarkdown.new(self).render
  end

  def snapshot_counts = @snapshot_counts
  def snapshot_paused = @snapshot_paused
  def running_report = @running
  def candidate_report = @candidate
  def post_snapshot = @post_snapshot

  private
    def prepare_fixture_database!
      db = database_path
      FileUtils.mkdir_p(File.dirname(db))
      [db, "#{db}-wal", "#{db}-shm"].each { |path| FileUtils.rm_f(path) }

      connect_sqlite!(db)
      ActiveRecord::Schema.verbose = false
      load SCHEMA

      connection = ActiveRecord::Base.connection
      connection.execute("PRAGMA journal_mode=WAL")
      connection.execute("PRAGMA foreign_keys=ON")
      connection.execute("PRAGMA busy_timeout=10000")
    end

    def connect_existing_database!
      db = database_path
      raise ArgumentError, "SQLite database does not exist: #{db}" unless File.file?(db)

      connect_sqlite!(db)
      connection = ActiveRecord::Base.connection
      connection.execute("PRAGMA busy_timeout=10000")
    end

    def connect_sqlite!(db)
      ActiveRecord::Base.establish_connection(
        adapter: "sqlite3",
        database: db,
        timeout: 10_000
      )
    end

    def seed!
      now = Time.now.utc
      INITIAL_QUEUES.each do |queue_name, count|
        count.times do |index|
          SolidQueue::Job.create!(
            queue_name: queue_name,
            class_name: "CoverageProbeJob",
            arguments: [{ index: index }].to_json,
            priority: 0,
            active_job_id: SecureRandom.uuid,
            created_at: now,
            updated_at: now
          )
        end
      end

      PAUSED_QUEUES.each do |queue_name|
        SolidQueue::Pause.create!(queue_name: queue_name, created_at: now)
      end
    end

    def record(event, **details)
      events << {
        seq: events.length + 1,
        at: Time.now.utc.iso8601(3),
        monotonic: Process.clock_gettime(Process::CLOCK_MONOTONIC),
        event: event
      }.merge(details)
    end

    def run_shared_snapshot!(race:)
      connection = SolidQueue::Record.connection
      previous_query_only = connection.select_value("PRAGMA query_only").to_i

      SolidQueue::Record.uncached do
        # ActiveRecord's normal SQLite transaction uses BEGIN IMMEDIATE, which
        # would make the demo writer lock. A raw DEFERRED transaction takes its
        # snapshot on the first SELECT and allows the second connection to write
        # in WAL mode. Existing-DB reports use the same read-only snapshot.
        connection.execute("PRAGMA query_only=ON")
        connection.execute("BEGIN DEFERRED TRANSACTION")
        record("shared_snapshot_begin", mode: "DEFERRED", read_only: true)

        @snapshot_counts = SolidQueue::ReadyExecution.group(:queue_name).count.sort.to_h
        @snapshot_paused = SolidQueue::Pause.pluck(:queue_name).sort
        record(
          "shared_view_read",
          ready_queues: @snapshot_counts.keys,
          paused_queues: @snapshot_paused
        )

        queue_names = @snapshot_counts.keys
        candidate_hook = race ? -> { insert_late_arrival! } : nil
        @candidate = scan("candidate", options[:candidate], queue_names, &candidate_hook)
        @running = scan("running", options[:running], queue_names)

        record("both_configs_scanned_in_shared_view")
        connection.execute("COMMIT")
      end

      record("shared_snapshot_released")
    rescue StandardError => error
      begin
        connection&.execute("ROLLBACK")
      rescue StandardError
        nil
      end
      raise error
    ensure
      begin
        connection.execute("PRAGMA query_only=#{previous_query_only == 1 ? "ON" : "OFF"}")
      rescue StandardError => e
        record("restore_query_only_failed", error: "#{e.class}: #{e.message}")
      end
    end

    def insert_late_arrival!
      record(
        "candidate_scan_midpoint",
        action: "insert ReadyExecution through a second SQLite connection",
        queue: LATE_QUEUE
      )

      result = Thread.new do
        ActiveRecord::Base.connection_pool.with_connection do |writer|
          writer.execute("PRAGMA busy_timeout=10000")
          now = Time.now.utc
          job = SolidQueue::Job.create!(
            queue_name: LATE_QUEUE,
            class_name: "CoverageProbeJob",
            arguments: [{ late: true }].to_json,
            priority: 9,
            active_job_id: SecureRandom.uuid,
            created_at: now,
            updated_at: now
          )
          {
            job_id: job.id,
            ready_execution_count: SolidQueue::ReadyExecution.where(job_id: job.id).count
          }
        end
      end.value

      @late_insert = result.merge(queue: LATE_QUEUE)
      record("late_arrival_committed_on_second_connection", **result, queue: LATE_QUEUE)
    end

    def scan(label, config, queue_names, &mid_scan)
      selector = SolidQueue::QueueSelector.new(config, SolidQueue::ReadyExecution)

      # These are the real private selection collections used by claim/count.
      # Calling them here also executes the real exact/prefix SQL.
      eligible_order = selector.send(:eligible_queues).map(&:to_s)
      claim_order = selector.send(:queue_names).map(&:to_s)
      record(
        "selector_core_read_complete",
        scan: label,
        eligible_order: eligible_order,
        claim_order: claim_order
      )

      mid_scan&.call

      view = queue_names.to_set
      entries = config.each_with_index.map do |pattern, index|
        pattern_selector = SolidQueue::QueueSelector.new([pattern], SolidQueue::ReadyExecution)
        matched_queues = pattern_selector
          .send(:eligible_queues)
          .map(&:to_s)
          .select { |queue_name| view.include?(queue_name) }

        {
          index: index + 1,
          pattern: pattern,
          match_type: match_type(pattern),
          matched_queues: matched_queues
        }
      end

      eligible_in_view = eligible_order.select { |queue_name| view.include?(queue_name) }
      claim_in_view = claim_order.select { |queue_name| view.include?(queue_name) }
      record("selector_match_attribution_complete", scan: label, entry_count: entries.length)

      queues = queue_names.map do |queue_name|
        summarize_queue(queue_name, entries, eligible_in_view, claim_in_view)
      end

      {
        label: label,
        config: config,
        entries: entries,
        eligible_order: eligible_in_view,
        claim_order: claim_in_view,
        queues: queues
      }
    end

    def summarize_queue(queue_name, entries, eligible_order, claim_order)
      matches = entries.select { |entry| entry[:matched_queues].include?(queue_name) }
      winning_match = matches.min_by { |entry| entry[:index] }
      shadowed_matches = matches.reject { |entry| entry[:index] == winning_match[:index] }

      covered = eligible_order.include?(queue_name)
      paused = snapshot_paused.include?(queue_name)
      claimable = claim_order.include?(queue_name)

      status =
        if claimable
          STATUS_CLAIMABLE
        elsif covered && paused
          STATUS_PAUSED
        elsif !covered
          STATUS_ABSENT
        else
          "UNEXPECTED_NOT_CLAIMABLE"
        end

      {
        queue_name: queue_name,
        status: status,
        covered: covered,
        paused: paused,
        claimable: claimable,
        eligible_position: eligible_order.index(queue_name)&.+(1),
        claim_position: claim_order.index(queue_name)&.+(1),
        winning_match: winning_match && "#{winning_match[:match_type]} #{winning_match[:pattern]} (##{winning_match[:index]})",
        all_matches: matches.map { |entry| "##{entry[:index]} #{entry[:match_type]} `#{entry[:pattern]}`" },
        shadowed_matches: shadowed_matches.map { |entry| "##{entry[:index]} #{entry[:match_type]} `#{entry[:pattern]}`" }
      }
    end

    def match_type(pattern)
      return "全量通配(all_wildcard)" if pattern == "*"
      return "前缀(prefix)" if pattern.end_with?("*")
      return "精确名(exact)" unless pattern.include?("*")

      "SQL模式(sql_pattern)"
    end

    def read_post_snapshot_state!
      counts = SolidQueue::ReadyExecution.group(:queue_name).count.sort.to_h
      paused = SolidQueue::Pause.pluck(:queue_name).sort

      late_matches = if counts.key?(LATE_QUEUE)
        {
          candidate: live_matching_patterns(options[:candidate], LATE_QUEUE),
          running: live_matching_patterns(options[:running], LATE_QUEUE)
        }
      else
        {}
      end

      @post_snapshot = {
        ready_execution_counts: counts,
        paused_queues: paused,
        claimed_execution_rows: SolidQueue::ClaimedExecution.count,
        late_queue: LATE_QUEUE,
        late_queue_ready_count: counts[LATE_QUEUE].to_i,
        late_queue_was_in_shared_snapshot: @snapshot_counts.key?(LATE_QUEUE),
        late_queue_live_matches: late_matches,
        interpretation: "This read is after the shared snapshot ended. It is audit evidence only; it is not mixed into the candidate-vs-running diff."
      }
    end

    def live_matching_patterns(config, queue_name)
      config.each_with_index.filter_map do |pattern, index|
        selector = SolidQueue::QueueSelector.new([pattern], SolidQueue::ReadyExecution)
        matched = selector.send(:eligible_queues).map(&:to_s).include?(queue_name)
        "##{index + 1} #{match_type(pattern)} `#{pattern}`" if matched
      end
    end

    def verify_expectations!
      candidate = @candidate[:queues].index_by { |queue| queue[:queue_name] }
      running = @running[:queues].index_by { |queue| queue[:queue_name] }

      checks = [
        ["snapshot contains exactly three fixture queues", @snapshot_counts.keys.sort == INITIAL_QUEUES.keys.sort],
        ["billing is exact-matched in candidate", candidate["billing"][:winning_match]&.include?("精确名(exact) billing")],
        ["billing is configured but paused", candidate["billing"][:status] == STATUS_PAUSED],
        ["mail-transactional matches two candidate prefixes", candidate["mail-transactional"][:all_matches].size == 2],
        ["mail-transactional is claimable in candidate", candidate["mail-transactional"][:status] == STATUS_CLAIMABLE],
        ["legacy-etl is absent from candidate", candidate["legacy-etl"][:status] == STATUS_ABSENT],
        ["legacy-etl is claimable under running config", running["legacy-etl"][:status] == STATUS_CLAIMABLE],
        ["late arrival is excluded from shared snapshot", @post_snapshot[:late_queue_was_in_shared_snapshot] == false],
        ["late ops-* match is invisible inside snapshot", @candidate[:entries].find { |e| e[:pattern] == "ops-*" }[:matched_queues].empty?],
        ["late arrival is visible after snapshot", @post_snapshot[:late_queue_ready_count] == 1],
        ["report did not claim executions", @post_snapshot[:claimed_execution_rows] == 0]
      ]

      failed = checks.reject { |_name, passed| passed }.map(&:first)
      raise "Coverage report self-check failed: #{failed.join("; ")}" if failed.any?

      @self_checks = checks.map { |name, passed| { name: name, passed: passed } }
    end
end

class ReportMarkdown
  def initialize(report)
    @report = report
  end

  def render
    [
      "# Solid Queue 领取覆盖对照报告",
      "",
      "生成时间：#{@report.generated_at}  ",
      "模式：`#{@report.fixture? ? "deterministic_fixture" : "existing_sqlite_read_only"}`  ",
      "SQLite：`#{@report.database_path}`  ",
      "复跑命令：",
      "",
      "```powershell",
      reproduction_command,
      "```",
      "",
      "报告扫描在单个 `SolidQueue::Record.uncached` + 数据库事务中完成，并对该连接设置 `PRAGMA query_only=ON`；没有调用 `ReadyExecution.claim`，也没有启动 worker。",
      "",
      summary_section,
      storage_section,
      config_section("3. 候选配置（准备发布）", @report.candidate_report),
      config_section("4. 正在运行的旧配置", @report.running_report),
      diff_section,
      order_vs_pause_section,
      (@report.fixture? ? race_section : nil),
      (@report.self_checks ? self_check_section : nil),
      reproduction_section
    ].flatten.join("\n") + "\n"
  end

  private
    def reproduction_command
      if @report.fixture?
        [
          "$env:TARGET_DB='sqlite'",
          "bundle exec ruby bin/queue_coverage_report.rb --write reports/queue-coverage-report.md --json reports/queue-coverage-report.json"
        ].join("\n")
      else
        "bundle exec ruby bin/queue_coverage_report.rb --db <solid-queue.sqlite3> --running '<old patterns>' --candidate '<new patterns>'"
      end
    end

    def summary_section
      candidate = @report.candidate_report[:queues]
      running_by_name = @report.running_report[:queues].index_by { |queue| queue[:queue_name] }
      claimable = candidate.select { |q| q[:status] == QueueCoverageReport::STATUS_CLAIMABLE }
      paused = candidate.select { |q| q[:status] == QueueCoverageReport::STATUS_PAUSED }
      absent = candidate.select { |q| q[:status] == QueueCoverageReport::STATUS_ABSENT }
      lost_claimable = candidate.select do |q|
        running_by_name[q[:queue_name]]&.[](:claimable) && !q[:claimable]
      end

      lines = [
        "## 1. 结论摘要（候选配置）",
        "",
        "- 能领取：#{queue_list(claimable)}",
        "- 已配置但暂停：#{queue_list(paused)}",
        "- 配置缺席：#{queue_list(absent)}",
        "- 相对旧配置丢失的可领取队列：#{lost_claimable.map { |q| "`#{q[:queue_name]}`" }.join("、").then { |s| s.empty? ? "无" : s }}"
      ]

      if @report.fixture?
        lines += [
          "- 场景核对：`legacy-etl` 由旧前缀 `legacy-*` 命中，候选删除该声明，因此属于配置缺席，不是暂停。",
          "- 暂停排除：`billing` 虽被精确名 `billing` 命中，但 `solid_queue_pauses` 有暂停行，真实 `QueueSelector#queue_names` 会减去它。",
          "- 顺序去重：`mail-transactional` 同时命中两条前缀；只保留一个领取队列，首选声明由配置顺序决定。"
        ]
      end

      lines << ""
      lines
    end

    def queue_list(rows)
      return "无" if rows.empty?

      rows.map { |row| "`#{row[:queue_name]}`（#{row[:winning_match] || "无命中"}）" }.join("；")
    end

    def storage_section
      rows = @report.snapshot_counts.map do |queue_name, count|
        paused = @report.snapshot_paused.include?(queue_name) ? "是" : "否"
        "| `#{queue_name}` | #{count} | #{paused} | 是 |"
      end

      [
        "## 2. 两套配置共用的 ReadyExecution / Pause 视图",
        "",
        "| 队列 | ReadyExecution 行数 | 暂停 | 参与本次对照 |",
        "| --- | ---: | :---: | :---: |",
        *rows,
        ""
      ]
    end

    def config_section(title, scan)
      rows = scan[:queues].map do |queue|
        matches = queue[:all_matches].join("<br>")
        [
          "| `#{queue[:queue_name]}` | #{status_label(queue[:status])} | #{matches} | #{queue[:eligible_position] || "—"} | #{queue[:claim_position] || "—"} |"
        ].join
      end

      [
        "## #{title}",
        "",
        "配置顺序：#{scan[:config].each_with_index.map { |pattern, i| "##{i + 1} `#{pattern}`" }.join(" → ")}",
        "",
        "真实选择后的覆盖顺序：`#{scan[:eligible_order].join("`, `")}`  ",
        "减去暂停后的领取顺序：`#{scan[:claim_order].join("`, `")}`",
        "",
        "| 队列 | 状态 | 真实匹配（精确名 / 前缀） | 覆盖顺序位置 | 实际领取位置 |",
        "| --- | --- | --- | :---: | :---: |",
        *rows,
        ""
      ]
    end

    def diff_section
      candidate_by_name = @report.candidate_report[:queues].index_by { |q| q[:queue_name] }
      rows = @report.running_report[:queues].map do |old|
        new = candidate_by_name[old[:queue_name]]
        coverage_change = old[:covered] == new[:covered] ? "不变" : (new[:covered] ? "新增覆盖" : "覆盖丢失")
        claim_change = old[:claimable] == new[:claimable] ? "不变" : (new[:claimable] ? "变为可领取" : "可领取丢失")
        reason = diff_reason(old, new)

        "| `#{old[:queue_name]}` | #{status_label(old[:status])} | #{status_label(new[:status])} | #{coverage_change} | #{claim_change} | #{reason} |"
      end

      [
        "## 5. 旧配置 vs 候选配置（同一快照）",
        "",
        "| 队列 | 旧状态 | 候选状态 | 覆盖差异 | 领取差异 | 判定来源 |",
        "| --- | --- | --- | --- | --- | --- |",
        *rows,
        ""
      ]
    end

    def diff_reason(old, new)
      if old[:queue_name] == "legacy-etl"
        "旧配置的 `legacy-*` 是真实前缀匹配；候选列表删除该声明，因此属于**配置缺席**，不是暂停。"
      elsif old[:queue_name] == "billing"
        "两版都有精确名 `billing`；不能领取的唯一原因是 Pause 行，不是配置顺序或配置缺失。"
      elsif old[:queue_name] == "mail-transactional"
        old_winner = old[:winning_match]
        new_winner = new[:winning_match]
        "两版都实际覆盖且可领取；顺序首选从 `#{old_winner}` 变为 `#{new_winner}`，重复前缀只影响该队列的归属/顺序，不产生重复领取。"
      else
        "两套配置在同一视图中的真实选择结果。"
      end
    end

    def order_vs_pause_section
      candidate_by_name = @report.candidate_report[:queues].index_by { |q| q[:queue_name] }
      duplicate_rows = @report.running_report[:queues].select do |old|
        new = candidate_by_name[old[:queue_name]]
        old[:all_matches].size > 1 || new[:all_matches].size > 1
      end

      lines = [
        "## 6. 配置顺序与暂停排除是两件事",
        "",
        "### 顺序决定重复匹配归属于哪条声明",
        ""
      ]

      if duplicate_rows.empty?
        lines << "本快照中没有队列同时命中多条配置声明。"
      else
        duplicate_rows.each do |old|
          current = candidate_by_name[old[:queue_name]]
          lines << "- `#{old[:queue_name]}`"
          lines << "  - 旧配置首选：**#{old[:winning_match] || "无"}**；其余命中：#{shadow_text(old)}"
          lines << "  - 候选首选：**#{current[:winning_match] || "无"}**；其余命中：#{shadow_text(current)}"
        end
        lines << "- `SolidQueue::QueueSelector` 会按原始配置顺序重排并去重；同一队列不会因为命中多条声明而被领取多次。"
      end

      paused_queues = @report.candidate_report[:queues].select { |queue| queue[:paused] }
      lines += [
        "",
        "### 暂停决定已覆盖队列是否退出领取列表",
        "",
        "快照 Pause：`#{@report.snapshot_paused.join("`, `")}`",
        ""
      ]

      if paused_queues.empty?
        lines << "本快照没有暂停队列。"
      else
        paused_queues.each do |queue|
          coverage = queue[:covered] ? "被候选配置命中（#{queue[:winning_match]}）" : "未被候选配置命中"
          lines << "- `#{queue[:queue_name]}`：#{coverage}；候选状态 `#{queue[:status]}`。"
        end
        lines << "- 对已覆盖且暂停的队列，仍可在 `eligible_order` 中审计到，但真实 `queue_names` / `scoped_relations` 会将其排除。"
      end

      lines << ""
      lines
    end

    def shadow_text(queue)
      return "无" if queue[:shadowed_matches].empty?

      queue[:shadowed_matches].join("、")
    end

    def race_section
      post = @report.post_snapshot
      event_rows = @report.events.map do |event|
        details = event.reject { |key, _| %i[seq at monotonic event].include?(key) }
        "| #{event[:seq]} | `#{event[:event]}` | `#{event[:at]}` | `#{escape_table_cell(details.inspect)}` |"
      end

      [
        "## 7. 候选扫描期间的新任务（并发对照）",
        "",
        "- 新任务队列：`#{post[:late_queue]}`",
        "- 第二连接提交后的实时 ReadyExecution 行数：#{post[:late_queue_ready_count]}",
        "- 是否进入本次共享快照：`#{post[:late_queue_was_in_shared_snapshot]}`",
        "- 快照结束后实时匹配：候选 #{format_late_matches(post.dig(:late_queue_live_matches, :candidate))}；旧配置 #{format_late_matches(post.dig(:late_queue_live_matches, :running))}",
        "- `solid_queue_claimed_executions` 行数：#{post[:claimed_execution_rows]}（报告不领取任务）。",
        "- 结论：新到达只作为快照后的新鲜度证据；候选/旧版差异仍只来自同一快照中的两套配置。重跑报告会在新的快照中一致地评估它。",
        "",
        "| # | 事件 | UTC 时间 | 细节 |",
        "| ---: | --- | --- | --- |",
        *event_rows,
        ""
      ]
    end

    def format_late_matches(matches)
      return "无" if matches.nil? || matches.empty?

      matches.join("、")
    end

    def status_label(status)
      chinese = {
        QueueCoverageReport::STATUS_CLAIMABLE => "能领取",
        QueueCoverageReport::STATUS_PAUSED => "已配置但暂停",
        QueueCoverageReport::STATUS_ABSENT => "配置缺席"
      }.fetch(status, "异常")

      "`#{status}`<br>#{chinese}"
    end

    def escape_table_cell(text)
      text.gsub("|", "\\|")
    end

    def self_check_section
      rows = @report.self_checks.map do |check|
        "| #{check[:passed] ? "PASS" : "FAIL"} | #{escape_table_cell(check[:name])} |"
      end

      [
        "## 8. 复跑自检",
        "",
        "| 结果 | 断言 |",
        "| --- | --- |",
        *rows,
        ""
      ]
    end

    def reproduction_section
      if @report.fixture?
        [
          "## 9. 人工复跑",
          "",
          "```powershell",
          "# 在仓库根目录；当前 GSB Windows 环境已通过 BUNDLE_GEMFILE 指向 .gsb-env/Gemfile",
          "$env:TARGET_DB='sqlite'",
          "bundle exec ruby bin/queue_coverage_report.rb `",
          "  --write reports/queue-coverage-report.md `",
          "  --json reports/queue-coverage-report.json",
          "```",
          "",
          "检查点：",
          "",
          "1. SQLite 初始只有 `billing`、`mail-transactional`、`legacy-etl` 三条 ReadyExecution 队列。",
          "2. `billing` 有对应 Pause 行。",
          "3. `mail-transactional` 同时命中 `mail-*` 与 `mail-transaction*`。",
          "4. `legacy-etl` 只被旧配置的 `legacy-*` 命中。",
          "5. 候选扫描中点，第二连接向 `ops-late` 插入 Job + ReadyExecution。",
          "6. 报告事务内两套配置看到相同的三张初始队列；事务后的实时读取才能看到 `ops-late`。",
          ""
        ]
      else
        [
          "## 9. 人工复跑真实 SQLite 视图",
          "",
          "```powershell",
          "bundle exec ruby bin/queue_coverage_report.rb `",
          "  --db storage/production.sqlite3 `",
          "  --running 'default,mail-*,legacy-*' `",
          "  --candidate 'default,mail-transaction*,mail-*' `",
          "  --write queue-coverage.md --json queue-coverage.json",
          "```",
          "",
          "该模式不会建表、播种或插入任务；扫描连接使用 `PRAGMA query_only=ON` 和 DEFERRED 只读快照。",
          ""
        ]
      end
    end
end

options = {
  fixture: true,
  fixture_db: File.join(ROOT, "tmp", "queue-coverage.sqlite3"),
  existing_db: nil,
  running: RUNNING_CONFIG,
  candidate: CANDIDATE_CONFIG,
  write: nil,
  json: nil
}

parser = OptionParser.new do |opts|
  opts.banner = "Usage: ruby bin/queue_coverage_report.rb [options]"
  opts.on("--fixture-db PATH", "Reset and use the deterministic SQLite fixture at PATH") { |path| options[:fixture_db] = File.expand_path(path, ROOT) }
  opts.on("--db PATH", "--sqlite PATH", "Inspect an existing Solid Queue SQLite DB read-only") do |path|
    options[:fixture] = false
    options[:existing_db] = File.expand_path(path, ROOT)
  end
  opts.on("--running a,b,c", Array, "Comma-separated running worker queues") { |values| options[:running] = values.flat_map { |v| v.split(",") }.map(&:strip).reject(&:empty?) }
  opts.on("--candidate a,b,c", Array, "Comma-separated candidate worker queues") { |values| options[:candidate] = values.flat_map { |v| v.split(",") }.map(&:strip).reject(&:empty?) }
  opts.on("--write PATH", "Write Markdown report here") { |path| options[:write] = File.expand_path(path, ROOT) }
  opts.on("--json PATH", "Write JSON report here") { |path| options[:json] = File.expand_path(path, ROOT) }
end
parser.parse!(ARGV)

report = QueueCoverageReport.new(options).call
markdown = report.markdown
puts markdown

if options[:write]
  FileUtils.mkdir_p(File.dirname(options[:write]))
  File.write(options[:write], markdown)
  warn "Wrote Markdown report to #{options[:write]}"
end

if options[:json]
  FileUtils.mkdir_p(File.dirname(options[:json]))
  File.write(options[:json], JSON.pretty_generate(report.as_json) + "\n")
  warn "Wrote JSON report to #{options[:json]}"
end
