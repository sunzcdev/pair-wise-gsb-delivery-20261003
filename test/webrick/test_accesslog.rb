# frozen_string_literal: false
require "test/unit"
require "stringio"
require "webrick"

class TestWEBrickAccessLog < Test::Unit::TestCase
  def setup
    @config = WEBrick::Config::HTTP.dup.update(
      :Logger => WEBrick::Log.new(File::NULL)
    )
  end

  def new_response
    WEBrick::HTTPResponse.new(@config)
  end

  def sent_response(&block)
    res = new_response
    res.body = "ok"
    block.call(res) if block
    res.send_header(StringIO.new)
    res
  end

  def params_for(res, view = nil)
    {
      "o" => res,
      "W" => view,
      "i" => nil,
      "p" => 80,
      "s" => WEBrick::AccessLog.response_status(res),
    }
  end

  def test_view_build_from_array
    view = WEBrick::AccessLog::ResponseHeaderView.build(%w[X-Cache Set-Cookie])
    assert_equal %w[x-cache set-cookie], view.fields
    assert_empty view.masked
  end

  def test_view_build_from_hash_normalizes_keys
    view = WEBrick::AccessLog::ResponseHeaderView.build(
      "Fields" => %w[X-Cache], "Masked" => %w[x-cache], "Mask" => "###")
    assert_equal %w[x-cache], view.fields
    assert_equal %w[x-cache], view.masked
    assert_equal "###", view.mask
  end

  def test_view_build_from_nil
    assert_nil WEBrick::AccessLog::ResponseHeaderView.build(nil)
  end

  def test_repeated_header_values_keep_wire_order
    res = sent_response {|r|
      r.header["X-Cache"] = "hit"
      r.header["x-cache"] = "miss"
    }
    view = WEBrick::AccessLog::ResponseHeaderView.build(%w[x-cache])
    assert_equal %w[hit miss], view.values(res, "X-Cache")
    assert_equal "hit, miss", view.value(res, "x-cache")
  end

  def test_separator_boundary_is_quoted_when_a_value_contains_it
    res = sent_response {|r| r["X-Cache"] = "hit, miss" }
    view = WEBrick::AccessLog::ResponseHeaderView.build(%w[x-cache])
    assert_equal '"hit, miss"', view.value(res, "x-cache")
  end

  def test_custom_separator
    res = sent_response {|r|
      r.header["X-Cache"] = "hit"
      r.header["x-cache"] = "miss"
    }
    view = WEBrick::AccessLog::ResponseHeaderView.build(
      :fields => %w[x-cache], :separator => " | ")
    assert_equal "hit | miss", view.value(res, "x-cache")
  end

  def test_masked_value_is_replaced_per_occurrence
    res = sent_response {|r|
      r.cookies << WEBrick::Cookie.new("a", "1")
      r.cookies << WEBrick::Cookie.new("b", "2")
    }
    view = WEBrick::AccessLog::ResponseHeaderView.build(
      :fields => %w[set-cookie], :masked => %w[set-cookie], :mask => "***")
    assert_equal "***, ***", view.value(res, "set-cookie")
    # The wire still carries the real values.
    assert_equal 2, res.sent_header.count {|name, _| name == "Set-Cookie" }
  end

  def test_unselected_field_is_not_reported
    res = sent_response {|r| r["x-cache"] = "hit"; r["x-other"] = "secret" }
    view = WEBrick::AccessLog::ResponseHeaderView.build(%w[x-cache])
    assert_nil view.selected_value(res, "x-other")
    assert_equal "hit", view.selected_value(res, "x-cache")
  end

  def test_header_hash_fallback_before_sending
    res = new_response
    res["x-cache"] = "hit"
    view = WEBrick::AccessLog::ResponseHeaderView.build(%w[x-cache])
    assert_equal :unsent, res.header_write_state
    assert_equal "hit", view.value(res, "x-cache")
  end

  def test_to_log_string_lists_selected_fields
    res = sent_response {|r|
      r["x-cache"] = "hit"
      r["cache-control"] = "max-age=0"
    }
    view = WEBrick::AccessLog::ResponseHeaderView.build(
      %w[x-cache cache-control missing])
    assert_equal "x-cache=hit cache-control=max-age=0 missing=-",
                 view.to_log_string(res)
  end

  def test_format_reads_header_snapshot_for_legacy_specifier
    res = sent_response {|r|
      r.header["X-Cache"] = "hit"
      r.header["x-cache"] = "miss"
    }
    out = WEBrick::AccessLog.format("%{X-Cache}o", params_for(res))
    assert_equal "hit, miss", out
  end

  def test_format_masks_through_legacy_specifier
    res = sent_response {|r| r.cookies << WEBrick::Cookie.new("sid", "abc") }
    view = WEBrick::AccessLog::ResponseHeaderView.build(
      :fields => %w[set-cookie], :masked => %w[set-cookie], :mask => "***")
    out = WEBrick::AccessLog.format("%{set-cookie}o", params_for(res, view))
    assert_equal "***", out
    refute_match(/sid=abc/, out)
  end

  def test_format_W_emits_selected_fields
    res = sent_response {|r|
      r.header["X-Cache"] = "hit"
      r.header["x-cache"] = "miss"
      r.cookies << WEBrick::Cookie.new("sid", "abc")
    }
    view = WEBrick::AccessLog::ResponseHeaderView.build(
      :fields => %w[x-cache set-cookie], :masked => %w[set-cookie], :mask => "***")
    out = WEBrick::AccessLog.format("%W", params_for(res, view))
    assert_equal "x-cache=hit, miss set-cookie=***", out
  end

  def test_format_W_is_empty_without_view
    res = sent_response {|r| r["x-cache"] = "hit" }
    assert_equal "", WEBrick::AccessLog.format("%W", params_for(res))
  end

  def test_format_write_state
    res = sent_response {|r| r["x-cache"] = "hit" }
    out = WEBrick::AccessLog.format(
      "%x %{bytes}x %{status}x", params_for(res))
    assert_equal "complete #{res.header_bytes_written} 200", out
  end

  def test_format_write_state_before_sending
    res = new_response
    out = WEBrick::AccessLog.format(
      "%x %{bytes}x %{status}x", params_for(res))
    assert_equal "unsent 0 -", out
  end

  def test_format_write_state_partial
    res = new_response
    res.body = "x"
    socket = Object.new
    def socket.write(_data) raise Errno::EPIPE end
    res.send_response(socket)

    out = WEBrick::AccessLog.format(
      "%s %x %{bytes}x %{status}x", params_for(res))
    # Nothing of the header reached the wire, so no status is reported as
    # delivered; the state and byte count tell the whole story.
    assert_equal "- partial 0 -", out
  end

  def test_format_write_state_when_only_the_status_line_was_written
    res = new_response
    res.body = "x"
    socket = Object.new
    def socket.write(_data) 17 end # "HTTP/1.1 200 OK\r\n"
    res.send_response(socket)

    out = WEBrick::AccessLog.format(
      "%x %{bytes}x %{size}x %{status}x", params_for(res))
    assert_equal "partial 17 #{res.sent_header_size} 200", out
    refute_equal res.sent_header_size, res.header_bytes_written
  end

  def test_partial_write_still_reports_the_prepared_header
    res = new_response
    res["x-cache"] = "hit"
    res.body = "ok"
    # A transport that accepts only the status line.
    socket = Object.new
    def socket.write(_data) 17 end
    res.send_response(socket)

    view = WEBrick::AccessLog::ResponseHeaderView.build(%w[x-cache])
    out = WEBrick::AccessLog.format("%W %x", params_for(res, view))
    # The field shows the value that was handed to the transport; %x says the
    # transport did not confirm the whole header, so the log does not claim a
    # complete response header was sent.
    assert_equal "x-cache=hit partial", out
    assert_equal %w[hit], res.sent_header.select {|n, _| n == "X-Cache" }.map(&:last)
  end

  def test_format_header_size_before_sending
    res = new_response
    assert_equal "0", WEBrick::AccessLog.format("%{size}x", params_for(res))
  end

  def test_status_prefers_written_status
    res = sent_response {|r| r.status = 201 }
    # The header was written as 201, so the log reports 201.
    assert_equal "201", WEBrick::AccessLog.format("%s", params_for(res))
  end

  def test_setup_params_builds_view_from_config
    config = @config.merge(
      :AccessLogResponseHeaders => { :fields => %w[x-cache] })
    req = Object.new
    def req.peeraddr; ["AF_INET", 1, "127.0.0.1", "127.0.0.1"]; end
    def req.request_method; "GET"; end
    def req.attributes; {}; end
    def req.port; 80; end
    def req.query_string; nil; end
    def req.request_line; "GET / HTTP/1.1\r\n"; end
    def req.request_time; Time.now; end
    def req.user; nil; end
    def req.unparsed_uri; "/"; end

    res = new_response
    params = WEBrick::AccessLog.setup_params(config, req, res)
    assert_instance_of WEBrick::AccessLog::ResponseHeaderView, params["W"]
    assert_same params["W"], res.response_header_view
  end
end