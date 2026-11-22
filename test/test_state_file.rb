# frozen_string_literal: true

require_relative "helper"
require_relative "helpers/tmp_path"

require 'puma/state_file'

class TestStateFile < PumaTest
  include TmpPath

  def test_load_empty_value_as_nil
    state_path = tmp_path('.state')
    File.write state_path, <<-STATE
---
pid: 123456
control_url:
control_auth_token:
running_from: "/path/to/app"
    STATE

    sf = Puma::StateFile.new
    sf.load(state_path)
    assert_equal 123456, sf.pid
    assert_equal '/path/to/app', sf.running_from
    assert_nil sf.control_url
    assert_nil sf.control_auth_token

  end

  def test_running_identity_round_trip
    state_path = tmp_path('.state')
    identity = 'a1b2c3d4e5f60718293a4b5c6d7e8f90'

    sf = Puma::StateFile.new
    sf.pid = 1234
    sf.control_url = 'tcp://127.0.0.1:9293'
    sf.control_auth_token = 'tok'
    sf.running_from = '/path/to/app'
    sf.running_identity = identity
    sf.save state_path

    contents = File.read state_path
    assert_match(/\Arunning_identity: #{identity}\z/, contents[/^running_identity:.*$/])

    loaded = Puma::StateFile.new
    loaded.load state_path

    assert_equal identity, loaded.running_identity
    assert_equal 'tcp://127.0.0.1:9293', loaded.control_url
    assert_equal 'tok', loaded.control_auth_token
  end

  # A state file written by an older Puma has no `running_identity` key.
  # It must still load cleanly and expose a nil identity, which pumactl
  # treats as "verification unavailable" rather than as an error.
  def test_legacy_state_file_without_running_identity
    state_path = tmp_path('.state')
    File.write state_path, <<-STATE
---
pid: 123456
control_url: tcp://127.0.0.1:9293
control_auth_token: tok
running_from: "/path/to/app"
    STATE

    sf = Puma::StateFile.new
    sf.load(state_path)

    assert_nil sf.running_identity
    assert_equal 123456, sf.pid
    assert_equal 'tcp://127.0.0.1:9293', sf.control_url
  end

  def test_empty_running_identity_value_is_nil
    state_path = tmp_path('.state')
    File.write state_path, <<-STATE
---
pid: 123456
running_identity:
    STATE

    sf = Puma::StateFile.new
    sf.load(state_path)

    assert_nil sf.running_identity
  end

  def test_save_skips_blank_running_identity
    state_path = tmp_path('.state')

    sf = Puma::StateFile.new
    sf.pid = 1234
    sf.running_identity = '   '
    sf.save state_path

    refute_match(/running_identity/, File.read(state_path))
  end
end
