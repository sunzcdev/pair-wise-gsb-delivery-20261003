require_relative "../spec_helper"

describe "plain_hash_response_headers plugin" do 
  it "uses plain hashes for response headers" do
    app(:plain_hash_response_headers) do |r|
      r.get 'up' do
        response.headers['UP'] = 'U'
      end

      response.headers['down'] = 'd'
    end

    if Rack.release >= '3' && ENV['LINT']
      proc{req('/up')}.must_raise Rack::Lint::LintError
    else
      header('up', '/up').must_be_nil
      header('UP', '/up').must_equal 'U'
    end
    header('down').must_equal 'd'
    header('DOWN').must_be_nil
    req[1].must_be_instance_of Hash
  end
end
