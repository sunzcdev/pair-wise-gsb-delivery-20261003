require_relative '../helper'

require 'tmpdir'
require 'json'
require 'fluent/command/time_explain'

class TestFluentCommandTimeExplain < Test::Unit::TestCase
  CONFIG = <<~CONF
    <source>
      @type forward
      blocking_timeout 2s
    </source>
    <match inline.**>
      @type stdout
      flush_interval 30s
    </match>
    <match nested.**>
      @type stdout
      <buffer>
        flush_interval 10s
        retry_wait 5
      </buffer>
    </match>
  CONF

  BAD_CONFIG = <<~CONF
    <match bad.**>
      @type stdout
      <buffer>
        flush_interval 10x
      </buffer>
    </match>
  CONF

  def write_config(content)
    dir = Dir.mktmpdir
    path = File.join(dir, 'fluent.conf')
    File.write(path, content)
    [dir, path]
  end

  test 'prints raw value, unit, seconds and section for inline and nested buffer params' do
    _dir, path = write_config(CONFIG)
    output = capture_stdout do
      Fluent::Command::TimeExplain.new([path]).call
    end

    assert_match(%r{match inline\.\*\* +flush_interval: raw="30s" +unit=s +seconds=30 +source=explicit}, output)
    assert_match(%r{match nested\.\*\* > buffer +flush_interval: raw="10s" +unit=s +seconds=10 +source=explicit}, output)
    # bare-number historical spelling is called out as having no unit
    assert_match(/retry_wait: raw="5" +unit=\(no unit, seconds\) +seconds=5\.0/, output)
    # defaults are reported too
    assert_match(/timekey_wait.*source=default/, output)
  end

  test 'emits JSON when requested' do
    _dir, path = write_config(CONFIG)
    output = capture_stdout do
      Fluent::Command::TimeExplain.new(['--format=json', path]).call
    end

    data = JSON.parse(output)
    inline = data.find do |entry|
      entry.dig('source', 'section_path') == 'match inline.**' && entry['source']['name'] == 'flush_interval'
    end
    assert_equal '30s', inline['raw_value']
    assert_equal 's', inline['unit']
    assert_equal 30, inline['seconds']
  end

  test 'exits with a non-zero status and locates the invalid time unit' do
    _dir, path = write_config(BAD_CONFIG)
    output = capture_stdout do
      assert_raise(SystemExit) do
        Fluent::Command::TimeExplain.new([path]).call
      end
    end

    assert_match(%r{match bad\.\*\* > buffer}, output)
    assert_match(/flush_interval: "10x" +INVALID/, output)
    assert_match(/invalid time value/, output)
  end

  test 'JSON output marks invalid entries with an error field' do
    _dir, path = write_config(BAD_CONFIG)
    output = capture_stdout do
      assert_raise(SystemExit) do
        Fluent::Command::TimeExplain.new(['-f', 'json', path]).call
      end
    end

    data = JSON.parse(output)
    invalid = data.find { |entry| entry.key?('error') }
    assert_equal '10x', invalid['raw_value']
    assert_equal 'match bad.** > buffer', invalid.dig('source', 'section_path')
  end

  YAML_CONFIG = <<~YAML
    config:
      - match:
          $tag: app.**
          $type: stdout
          flush_interval: 30s
          buffer:
            retry_wait: 5
  YAML

  test 'explains YAML configuration files' do
    dir = Dir.mktmpdir
    path = File.join(dir, 'fluent.yaml')
    File.write(path, YAML_CONFIG)
    output = capture_stdout do
      Fluent::Command::TimeExplain.new([path]).call
    end

    assert_match(/match app\.\*\* +flush_interval: raw="30s"/, output)
    assert_match(%r{match app\.\*\* > buffer +retry_wait: raw="5"}, output)
  end

  test 'rejects unknown output formats' do
    _dir, path = write_config(CONFIG)
    assert_raise(SystemExit) do
      capture_stdout do
        Fluent::Command::TimeExplain.new(['--format=xml', path]).call
      end
    end
  end

  test 'requires a config path' do
    assert_raise(SystemExit) do
      capture_stdout do
        Fluent::Command::TimeExplain.new([]).call
      end
    end
  end

  test 'exits cleanly when the config file does not exist' do
    capture_output do
      error = assert_raise(SystemExit) do
        Fluent::Command::TimeExplain.new([File.join(Dir.mktmpdir, 'missing.conf')]).call
      end
      assert_equal 1, error.status
    end
  end

  test 'exits cleanly with a readable message for an unknown plugin' do
    dir = Dir.mktmpdir
    path = File.join(dir, 'unknown.conf')
    File.write(path, <<~CONF)
      <match x.**>
        @type nonexistent_plugin_for_test
      </match>
    CONF

    _, err = capture_output do
      error = assert_raise(SystemExit) do
        Fluent::Command::TimeExplain.new([path]).call
      end
      assert_equal 1, error.status
    end
    assert_match(/Unknown output plugin/, err)
  end
end
