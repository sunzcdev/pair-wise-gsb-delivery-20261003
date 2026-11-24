require 'spec_helper'
require 'socket'
require 'net/http'
require 'yaml'

RSpec.describe 'Preserving the recorded body end semantics', with_monkey_patches: :webmock do
  # A minimal HTTP server that delimits each response body in a different
  # way, so that VCR records the semantics of how each body ends.
  class BodyEndServer
    CONNECTION_CLOSE_BODY = 'close-delimited body'
    NORMAL_BODY           = 'hello world'

    attr_reader :port

    def initialize
      @server = TCPServer.new('127.0.0.1', 0)
      @port   = @server.addr[1]
      @thread = Thread.new do
        loop { Thread.new(@server.accept) { |socket| serve(socket) } }
      end
    end

    def shutdown
      @thread.kill
      @server.close
    end

  private

    def serve(socket)
      request_line = socket.gets or return socket.close
      path = request_line.split(' ')[1]
      while (line = socket.gets) && line != "\r\n"; end # consume the request headers

      case path
      when '/connection-close'
        # No Content-Length: the body ends when the connection closes.
        respond(socket, "Content-Type: text/plain\r\nConnection: close\r\n", CONNECTION_CLOSE_BODY)
      when '/zero'
        respond(socket, "Content-Length: 0\r\nConnection: close\r\n", '')
      when '/head'
        # A HEAD response declares the length of the representation it
        # describes, but does not carry a body of its own.
        respond(socket, "Content-Length: 42\r\nConnection: close\r\n", '')
      when '/normal'
        respond(socket, "Content-Length: #{NORMAL_BODY.bytesize}\r\nConnection: close\r\n", NORMAL_BODY)
      else
        respond(socket, "Content-Length: 0\r\nConnection: close\r\n", '', '404 Not Found')
      end
    end

    def respond(socket, headers, body, status = '200 OK')
      socket.write "HTTP/1.1 #{status}\r\n#{headers}\r\n#{body}"
      socket.close
    end
  end

  before(:all)  { @server = BodyEndServer.new }
  after(:all)   { @server.shutdown }
  let(:server)  { @server }

  before(:each) do
    VCR.configure { |c| c.hook_into :webmock }
  end

  def get(path)
    Net::HTTP.start('127.0.0.1', server.port) { |http| http.get(path) }
  end

  def head(path)
    Net::HTTP.start('127.0.0.1', server.port) { |http| http.head(path) }
  end

  def cassette_file(name)
    File.join(VCR.configuration.cassette_library_dir, "#{name}.yml")
  end

  it 'records how each response body ends and preserves those semantics on playback' do
    # Record the four responses through the local adapter.
    VCR.use_cassette('body_end', preserve_body_end: true) do
      expect(get('/connection-close').body).to eq(BodyEndServer::CONNECTION_CLOSE_BODY)
      expect(get('/zero').body).to eq('')
      expect(head('/head')['Content-Length']).to eq('42')
      expect(get('/normal').body).to eq(BodyEndServer::NORMAL_BODY)
    end

    # The recorded semantics must survive being saved and reopened.
    recorded = YAML.load_file(cassette_file('body_end'))['http_interactions']
    body_ends = recorded.map { |i| i['response'].fetch('adapter_metadata')['vcr_body_end'] }
    expect(body_ends).to eq(%w[ connection_close content_length no_body content_length ])

    longer_close_body = 'a longer close-delimited body'
    longer_normal_body = 'a much longer recorded body'

    VCR.configure do |c|
      c.before_playback do |interaction|
        case interaction.request.uri
        when %r{/connection-close}
          interaction.response.body = longer_close_body
          # Simulate the HTTP adapter adding a temporary Content-Length.
          interaction.response.headers['Content-Length'] = ['999']
        when %r{/normal}
          interaction.response.body = longer_normal_body
        end
      end
    end

    VCR.use_cassette('body_end', preserve_body_end: true) do
      # The body that ended with the connection closing is played back
      # without a Content-Length, and its (modified) bytes are returned
      # in full.
      close_response = get('/connection-close')
      expect(close_response['Content-Length']).to be_nil
      expect(close_response.body).to eq(longer_close_body)

      # The zero length GET keeps its original representation.
      zero_response = get('/zero')
      expect(zero_response['Content-Length']).to eq('0')
      expect(zero_response.body).to eq('')

      # The HEAD response keeps the declared representation length.
      head_response = head('/head')
      expect(head_response['Content-Length']).to eq('42')
      expect(head_response.body.to_s).to eq('')

      # The normal GET gets its Content-Length updated to match the
      # modified body.
      normal_response = get('/normal')
      expect(normal_response.body).to eq(longer_normal_body)
      expect(normal_response['Content-Length']).to eq(longer_normal_body.bytesize.to_s)
    end
  end
end
