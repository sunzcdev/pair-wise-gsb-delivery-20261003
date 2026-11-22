require_relative "../spec_helper"

describe "typecast_params combined conversion for gateway forwarding rules" do
  before do
    forwarded = @forwarded = []

    app(:bare) do
      plugin :typecast_params_sized_integers

      plugin :typecast_params do
        # Explicit combined conversion for one gateway forwarding rule group.
        # Uses the existing sized integer bit-width checks on the nested
        # fields (uint16 for the ports, uint32 for the subnet number), then
        # compares the start/end values of the same port range.  Any missing
        # field, out-of-range value, or inverted range is reported against
        # the nested field, so the error keys locate the original array item.
        def forwarding_rule!(start_key = 'start_port', end_key = 'end_port', subnet_key = 'subnet')
          start_port, end_port = uint16!([start_key, end_key])
          subnet = uint32!(subnet_key)

          # Only compare when both fields converted to integers; otherwise
          # the bit-width/missing checks above already recorded the error.
          if start_port.is_a?(Integer) && end_port.is_a?(Integer) && start_port > end_port
            handle_error(start_key, :invalid_value,
              "start port #{start_port} is greater than end port #{end_port} for parameter #{param_name(start_key)}")
          end

          {start_key=>start_port, end_key=>end_port, subnet_key=>subnet}
        end
      end

      plugin :error_handler do |e|
        if e.is_a?(Roda::RodaPlugins::TypecastParams::Error)
          response.status = 400
          e.param_names.join(', ')
        else
          raise e
        end
      end

      route do |r|
        r.post 'forwarding' do
          # Convert all rule groups up front.  If any group fails, this
          # raises before the normal forwarding branch below is reached.
          rules = typecast_params['rules'].convert_each! do |rule|
            rule.forwarding_rule!
          end

          # Normal forwarding branch: only executed when every group
          # converted successfully.
          rules.each{|rule| forwarded << rule}
          rules.map{|rule| "#{rule['start_port']}-#{rule['end_port']}/#{rule['subnet']}"}.join(' ')
        end

        r.get 'port' do
          # Single-value conversion interface is unchanged.
          typecast_params.uint16!('port').to_s
        end
      end
    end
  end

  def post_rules(body)
    req('REQUEST_METHOD'=>'POST', 'PATH_INFO'=>'/forwarding',
        'CONTENT_TYPE'=>'application/x-www-form-urlencoded',
        'rack.input'=>rack_input(body))
  end

  it "converts a complete set of rule groups and runs the forwarding branch" do
    status, _, body = post_rules(
      'rules[0][start_port]=80&rules[0][end_port]=8080&rules[0][subnet]=3232235521' \
      '&rules[1][start_port]=9000&rules[1][end_port]=9001&rules[1][subnet]=2886729729')

    status.must_equal 200
    body.join.must_equal '80-8080/3232235521 9000-9001/2886729729'

    # Verify the values actually passed down to the route's forwarding branch
    @forwarded.must_equal [
      {'start_port'=>80, 'end_port'=>8080, 'subnet'=>3232235521},
      {'start_port'=>9000, 'end_port'=>9001, 'subnet'=>2886729729}
    ]
    @forwarded.each do |rule|
      rule.values.each{|v| v.must_be_kind_of Integer}
    end
  end

  it "rejects a set with multiple errors, locating the original array items, without running the forwarding branch" do
    status, _, body = post_rules(
      'rules[0][start_port]=80&rules[0][subnet]=3232235521' \
      '&rules[1][start_port]=70000&rules[1][end_port]=8080&rules[1][subnet]=4294967296' \
      '&rules[2][start_port]=9000&rules[2][end_port]=8000&rules[2][subnet]=2886729729')

    status.must_equal 400
    names = body.join.split(', ')
    # missing field in group 0, out-of-range port and subnet in group 1,
    # inverted range in group 2, all located to the original array item
    names.must_include 'rules[0][end_port]'
    names.must_include 'rules[1][start_port]'
    names.must_include 'rules[1][subnet]'
    names.must_include 'rules[2][start_port]'
    names.length.must_equal 4

    # The normal forwarding branch must not have executed
    @forwarded.must_equal []
  end

  it "returns 400 located at rules when the rules parameter itself is missing" do
    status, _, body = post_rules('')

    status.must_equal 400
    body.join.must_equal 'rules'
    @forwarded.must_equal []
  end

  it "keeps the single-value conversion interface working" do
    body('/port', 'QUERY_STRING'=>'port=8080').must_equal '8080'

    status, _, body = req('/port', 'QUERY_STRING'=>'port=70000')
    status.must_equal 400
    body.join.must_equal 'port'
  end
end
