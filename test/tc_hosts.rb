# --
# Copyright 2007 Nominet UK
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
# ++

require_relative 'spec_helper'
require 'tmpdir'
require 'fileutils'

# These tests never touch the real system hosts file: every Hosts instance is
# pointed at a unique temporary file inside an isolated temp directory.
class TestHosts < Minitest::Test
  def setup
    @dir = Dir.mktmpdir('dnsruby-hosts-spec')
    @path = File.join(@dir, 'hosts')
  end

  def teardown
    FileUtils.remove_entry(@dir) if @dir && File.directory?(@dir)
  end

  def write_hosts(content)
    File.write(@path, content)
  end

  def new_hosts(**options)
    Dnsruby::Hosts.new(@path, **options)
  end

  def test_parses_ipv4_ipv6_comments_and_blank_lines
    write_hosts(<<~HOSTS)
      # full line comment

      127.0.0.1   localhost localhost.localdomain
      2001:db8:0:0:0:0:0:1 v6full
      2001:db8::1 v6compressed # trailing comment
    HOSTS
    result = new_hosts.reload!

    assert_predicate result, :published?
    assert_equal 1, result.generation
    assert_equal 5, result.line_count
    assert_equal 4, result.entry_count
    hosts = new_hosts.tap(&:reload!)
    assert_equal '127.0.0.1', hosts.getaddress('localhost')
    assert_equal '2001:DB8::1', hosts.getaddress('v6full')
    assert_equal '2001:DB8::1', hosts.getaddress('v6compressed')
    assert_equal ['localhost', 'localhost.localdomain'], hosts.getnames('127.0.0.1')
    assert_empty result.line_errors
  end

  def test_invalid_address_and_missing_and_invalid_hostnames_are_reported
    write_hosts(<<~HOSTS)
      999.1.1.1 bad-address.example
      10.0.0.1
      10.0.0.2 -leading-dash.example
      10.0.0.3 good.example
    HOSTS
    result = new_hosts.reload!

    assert_predicate result, :published?
    reasons = result.line_errors.map(&:reason)
    assert_includes reasons, Dnsruby::Hosts::LINE_ERROR_INVALID_ADDRESS
    assert_includes reasons, Dnsruby::Hosts::LINE_ERROR_NO_HOSTNAMES
    assert_includes reasons, Dnsruby::Hosts::LINE_ERROR_INVALID_HOSTNAME
    lines = result.line_errors.map(&:line)
    assert_equal [1, 2, 3], lines
    hosts = new_hosts.tap(&:reload!)
    assert_equal ['10.0.0.3'], hosts.getaddresses('good.example')
    assert_empty hosts.getaddresses('bad-address.example')
  end

  def test_strict_mode_rejects_entire_reload_and_keeps_generation
    write_hosts("127.0.0.1 good.example\n")
    hosts = new_hosts(strict: true)
    first = hosts.reload!
    assert_predicate first, :published?
    assert_equal 1, hosts.generation

    write_hosts("not-an-ip still.example\n")
    result = hosts.reload!
    assert_equal Dnsruby::Hosts::STATUS_REJECTED_LINES, result.status
    assert_predicate result, :rejected?
    assert_equal 1, result.generation, 'rejected reload must not advance generation'
    assert_equal '127.0.0.1', hosts.getaddress('good.example'), 'old mapping must stay effective'
    assert_equal 1, result.line_errors.size
  end

  def test_half_broken_file_never_replaces_valid_mapping_in_lenient_mode
    write_hosts("127.0.0.1 stable.example\n")
    hosts = new_hosts
    hosts.reload!

    write_hosts("127.0.0.1 stable.example\nthis is broken\n")
    result = hosts.reload!
    assert_predicate result, :published?
    assert_equal 2, result.generation
    assert_equal 1, result.line_errors.size
    assert_equal ['127.0.0.1'], hosts.getaddresses('stable.example')
  end

  def test_lookups_normalize_case_for_names_and_addresses
    write_hosts("10.0.0.1 Host.Example ALIAS.Example\n")
    hosts = new_hosts.tap(&:reload!)

    assert_equal hosts.getaddresses('host.example'), hosts.getaddresses('HOST.EXAMPLE')
    assert_equal hosts.getaddresses('Host.Example'), hosts.getaddresses('hOsT.eXaMpLe')
    assert_equal 'host.example', hosts.getname('10.0.0.1')
    assert_equal 'host.example', Dnsruby::Hosts.normalize_name('HOST.EXAMPLE.')
  end

  def test_duplicate_name_keeps_last_declaration_first_with_conflict
    write_hosts(<<~HOSTS)
      10.0.0.1 host.example
      10.0.0.2 host.example
    HOSTS
    result = new_hosts.reload!
    hosts = new_hosts.tap(&:reload!)

    assert_equal ['10.0.0.2', '10.0.0.1'], hosts.getaddresses('host.example')
    conflict = result.conflicts.find { |c| c.kind == :name_rebound }
    refute_nil conflict
    assert_equal Dnsruby::Hosts::CONFLICT_NAME_REBOUND, conflict.strategy
    assert_equal 2, conflict.line
  end

  def test_identical_pair_is_kept_once_first_declaration_wins
    write_hosts(<<~HOSTS)
      10.0.0.1 host.example alias.example
      10.0.0.1 host.example alias.example
    HOSTS
    result = new_hosts.reload!
    hosts = new_hosts.tap(&:reload!)

    assert_equal ['10.0.0.1'], hosts.getaddresses('host.example')
    assert_equal ['host.example', 'alias.example'], hosts.getnames('10.0.0.1')
    kinds = result.conflicts.map(&:kind)
    assert_includes kinds, :duplicate_pair
    assert_equal Dnsruby::Hosts::CONFLICT_DUPLICATE_PAIR,
      result.conflicts.find { |c| c.kind == :duplicate_pair }.strategy
  end

  def test_address_with_multiple_names_accumulates_first_seen_order
    write_hosts(<<~HOSTS)
      10.0.0.1 first.example second.example
      10.0.0.1 third.example
    HOSTS
    result = new_hosts.reload!
    hosts = new_hosts.tap(&:reload!)

    assert_equal ['first.example', 'second.example', 'third.example'],
      hosts.getnames('10.0.0.1')
    conflict = result.conflicts.find { |c| c.kind == :address_renamed }
    refute_nil conflict
    assert_equal Dnsruby::Hosts::CONFLICT_ADDRESS_RENAMED, conflict.strategy
  end

  def test_case_folded_spelling_records_conflict
    write_hosts(<<~HOSTS)
      10.0.0.1 host.example
      10.0.0.2 HOST.example
    HOSTS
    result = new_hosts.reload!

    conflict = result.conflicts.find { |c| c.kind == :case_folded }
    refute_nil conflict
    assert_equal Dnsruby::Hosts::CONFLICT_CASE_FOLDED, conflict.strategy
    assert_equal 2, conflict.line
  end

  def test_entries_remember_source_line_role_and_raw_text
    write_hosts("10.0.0.1 canonical.example alias.example\n")
    hosts = new_hosts.tap(&:reload!)

    entries = hosts.entries_for_name('canonical.example')
    assert_equal 1, entries.size
    entry = entries.first
    assert_equal 1, entry.line
    assert_equal '10.0.0.1 canonical.example alias.example', entry.raw_line
    assert_predicate entry, :canonical?

    alias_entry = hosts.entries_for_name('alias.example').first
    assert_predicate alias_entry, :alias?
    assert_equal 1, alias_entry.line
    assert_equal '10.0.0.1', alias_entry.address

    addr_entries = hosts.entries_for_address('10.0.0.1')
    assert_equal ['canonical.example', 'alias.example'], addr_entries.map(&:name)
  end

  def test_published_snapshots_are_frozen_and_immutable
    write_hosts("10.0.0.1 host.example\n")
    hosts = new_hosts.tap(&:reload!)
    entry = hosts.entries_for_name('host.example').first

    assert entry.frozen?
    assert entry.raw_line.frozen?
    assert_raises(FrozenError) { entry.address = '10.0.0.9' }
  end

  def test_reload_diff_explains_cache_invalidation_scope
    write_hosts(<<~HOSTS)
      10.0.0.1 keep.example changed.example
      10.0.0.2 gone.example
    HOSTS
    hosts = new_hosts.tap(&:reload!)

    write_hosts(<<~HOSTS)
      10.0.0.1 keep.example renamed.example
      10.0.0.9 changed.example
      10.0.0.3 added.example
    HOSTS
    result = hosts.reload!

    assert_equal ['added.example', 'renamed.example'].sort, result.added_names.sort
    assert_equal ['gone.example'], result.removed_names
    assert_equal ['changed.example'], result.updated_names
    assert_includes result.removed_addresses, '10.0.0.2'
    assert_includes result.added_addresses, '10.0.0.3'
    assert_includes result.added_addresses, '10.0.0.9'
    # 10.0.0.1 changed from (keep, changed) to (keep, renamed), so the reverse
    # mapping is reported as updated for address-scoped cache invalidation.
    assert_includes result.updated_addresses, '10.0.0.1'
    assert_empty result.removed_names & result.added_names
  end

  def test_rejected_reload_reports_empty_diff_and_same_generation
    write_hosts("10.0.0.1 host.example\n")
    hosts = new_hosts(strict: true)
    hosts.reload!

    write_hosts("garbage\n")
    result = hosts.reload!

    assert_predicate result, :rejected?
    assert_equal 1, result.generation
    assert_empty result.added_names
    assert_empty result.removed_names
    assert_empty result.updated_names
    assert_empty result.added_addresses
    assert_empty result.removed_addresses
    assert_equal '10.0.0.1', hosts.getaddress('host.example')
  end

  def test_missing_file_is_reported_and_keeps_previous_generation
    write_hosts("10.0.0.1 host.example\n")
    hosts = new_hosts.tap(&:reload!)

    File.delete(@path)
    result = hosts.reload!
    assert_equal Dnsruby::Hosts::STATUS_FILE_UNAVAILABLE, result.status
    assert_predicate result, :rejected?
    assert_equal 1, result.generation
    assert_equal '10.0.0.1', hosts.getaddress('host.example')

    fresh = new_hosts.reload!
    assert_equal Dnsruby::Hosts::STATUS_FILE_UNAVAILABLE, fresh.status
    assert_equal 0, fresh.generation
    assert_empty new_hosts.tap(&:reload!).getaddresses('host.example')
  end

  def test_replaced_file_during_read_is_refused
    write_hosts("127.0.0.1 first.example\n")
    swapping_reader = lambda do |filename|
      before = Dnsruby::Hosts.file_signature(filename)
      content = File.read(filename)
      # Simulate the file being atomically replaced while the read is in
      # progress; the post-read signature therefore differs.
      File.write(filename, "10.1.1.1 swapped.example\n")
      after = Dnsruby::Hosts.file_signature(filename)
      raise Dnsruby::Hosts::HostsFileReplaced, 'changed' unless before == after
      content
    end

    hosts = Dnsruby::Hosts.new(@path, reader: swapping_reader)
    result = hosts.reload!

    assert_equal Dnsruby::Hosts::STATUS_REPLACED, result.status
    assert_predicate result, :rejected?
    assert_equal 0, result.generation
    assert_empty hosts.getaddresses('swapped.example')
    assert_empty hosts.getaddresses('first.example')
    refute_nil result.error
  end

  def test_default_reader_detects_real_replacement_around_read
    write_hosts("127.0.0.1 first.example\n")
    hosts = new_hosts
    result = hosts.reload!
    assert_predicate result, :published?

    # Rewrite with different content and ensure a normal reload picks up the
    # complete new generation atomically.
    write_hosts("10.2.2.2 second.example\n")
    second = hosts.reload!
    assert_predicate second, :published?
    assert_equal 2, second.generation
    assert_equal '10.2.2.2', hosts.getaddress('second.example')
    assert_empty hosts.getaddresses('first.example')
  end

  def test_each_address_reads_one_fixed_generation_even_if_reloaded
    write_hosts("1.1.1.1 shared.example\n")
    hosts = new_hosts.tap(&:reload!)

    seen_during_iteration = nil
    hosts.each_address('shared.example') do |address|
      seen_during_iteration = address
      write_hosts("9.9.9.9 shared.example\n")
      hosts.reload!
    end

    assert_equal '1.1.1.1', seen_during_iteration
    assert_equal '9.9.9.9', hosts.getaddress('shared.example')
  end

  def test_on_reload_hook_receives_every_result_including_rejections
    write_hosts("127.0.0.1 host.example\n")
    hosts = new_hosts
    received = []
    hosts.on_reload { |result| received << result }

    hosts.reload!
    write_hosts("broken\n")
    hosts.reload!(strict: true)

    assert_equal 2, received.size
    assert_predicate received.first, :published?
    assert_predicate received.last, :rejected?
    assert_equal 1, received.last.generation
  end

  def test_failing_hook_does_not_break_reload
    write_hosts("127.0.0.1 host.example\n")
    hosts = new_hosts
    hosts.on_reload { raise 'hook boom' }
    result = hosts.reload!
    assert_predicate result, :published?
    assert_equal '127.0.0.1', hosts.getaddress('host.example')
  end

  def test_lazy_initialization_loads_once_and_survives_missing_file
    path = File.join(@dir, 'never-created')
    hosts = Dnsruby::Hosts.new(path)
    assert_empty hosts.getaddresses('anything.example')
    assert_equal 0, hosts.generation
    assert_equal Dnsruby::Hosts::STATUS_FILE_UNAVAILABLE, hosts.last_load_result.status
  end

  def test_getaddress_and_getname_raise_resolv_error_when_absent
    write_hosts("127.0.0.1 host.example\n")
    hosts = new_hosts.tap(&:reload!)

    assert_raises(Dnsruby::ResolvError) { hosts.getaddress('missing.example') }
    assert_raises(Dnsruby::ResolvError) { hosts.getname('10.9.9.9') }
  end

  def test_name_normalization_rules_are_explicit
    normalize = Dnsruby::Hosts.method(:normalize_name)
    assert_equal 'a_b.example', normalize.call('a_b.example'), 'underscores are allowed in hosts names'
    assert_equal 'a-b.example', normalize.call('a-b.example')
    assert_equal 'a.example', normalize.call('A.EXAMPLE.')
    assert_equal 'host', normalize.call('host.')
    assert_equal '1host', normalize.call('1host')
    assert_nil normalize.call('x..y.example')
    assert_nil normalize.call('.example')
    assert_nil normalize.call('a-.example')
    assert_nil normalize.call('-a.example')
    assert_nil normalize.call('')
  end

  def test_address_normalization_accepts_v4_v6_and_rejects_garbage
    normalize = Dnsruby::Hosts.method(:normalize_address)
    assert_equal '10.0.0.1', normalize.call('10.0.0.1')
    assert_nil normalize.call('256.1.1.1')
    assert_nil normalize.call('1.2.3')
    assert_equal '::1', normalize.call('::1')
    assert_equal '2001:DB8::1', normalize.call('2001:db8:0:0:0:0:0:1')
    assert_nil normalize.call('fe80::1%eth0'), 'scoped IPv6 literals are not accepted'
    assert_nil normalize.call('abcd')
  end

  def test_concurrent_queries_serve_consistent_generation
    write_hosts("10.0.0.#{1}.example\n")
    hosts = new_hosts
    hosts.reload!

    errors = []
    threads = 4.times.map do |i|
      Thread.new do
        50.times do
          write_hosts("10.1.#{i}.1 rotating.example\n")
          hosts.reload!
          begin
            hosts.getaddresses('rotating.example').each do |address|
              raise 'mixed address' unless address.match?(/\A10\.\d+\.\d+\.\d+\z/)
            end
          rescue => e
            errors << e
          end
        end
      end
    end
    threads.each(&:join)
    assert_empty errors
  end

  def test_empty_file_publishes_an_empty_generation
    write_hosts("")
    result = new_hosts.reload!
    assert_predicate result, :published?
    assert_equal 1, result.generation
    assert_equal 0, result.entry_count
    assert_empty result.added_names
  end

  def test_underscore_and_trailing_dot_names_resolve_after_file_parse
    write_hosts(<<~HOSTS)
      10.0.0.1 under_score.example
      10.0.0.2 dotted.example.
    HOSTS
    hosts = new_hosts.tap(&:reload!)

    assert_equal '10.0.0.1', hosts.getaddress('under_score.example')
    assert_equal '10.0.0.2', hosts.getaddress('dotted.example')
    assert_equal '10.0.0.2', hosts.getaddress('DOTTED.example.')
    assert_equal ['under_score.example'], hosts.getnames('10.0.0.1')
  end
end
