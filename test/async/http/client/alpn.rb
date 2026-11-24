# frozen_string_literal: true

# Released under the MIT License.
# Copyright, 2026, by Samuel Williams.

require "async/http/server"
require "async/http/client"
require "async/http/endpoint"

require "sus/fixtures/async"

require "openssl"

describe Async::HTTP::Client do
	include Sus::Fixtures::Async::ReactorContext

	let(:key) {OpenSSL::PKey::RSA.new(2048)}

	let(:certificate) do
		certificate = OpenSSL::X509::Certificate.new
		certificate.version = 2
		certificate.serial = 1
		certificate.subject = OpenSSL::X509::Name.parse("/CN=localhost")
		certificate.issuer = certificate.subject
		certificate.public_key = key.public_key
		certificate.not_before = Time.now
		certificate.not_after = Time.now + 3600

		extension_factory = OpenSSL::X509::ExtensionFactory.new
		extension_factory.subject_certificate = certificate
		extension_factory.issuer_certificate = certificate

		certificate.add_extension(extension_factory.create_extension("basicConstraints", "CA:FALSE", true))
		certificate.add_extension(extension_factory.create_extension("subjectAltName", "DNS:localhost", false))
		certificate.add_extension(extension_factory.create_extension("keyUsage", "digitalSignature,keyEncipherment", true))
		certificate.add_extension(extension_factory.create_extension("extendedKeyUsage", "serverAuth", false))

		certificate.sign(key, OpenSSL::Digest::SHA256.new)

		certificate
	end

	# The protocol the server selects during ALPN negotiation:
	let(:alpn_selection) {"http/1.1"}

	let(:server_context) do
		OpenSSL::SSL::SSLContext.new.tap do |context|
			context.cert = certificate
			context.key = key

			context.alpn_select_cb = lambda do |protocols|
				alpn_selection
			end
		end
	end

	# The range of protocols the client offers during ALPN negotiation, in order of preference:
	let(:alpn_protocols) {["h2", "http/1.1"]}

	# The range of protocols the client is willing to use, in order of preference:
	let(:protocol) {Async::HTTP::Protocol::HTTPS.for(alpn_protocols)}

	let(:certificate_store) do
		OpenSSL::X509::Store.new.tap do |store|
			store.add_cert(certificate)
		end
	end

	let(:client_context) do
		OpenSSL::SSL::SSLContext.new.tap do |context|
			context.cert_store = certificate_store
			context.alpn_protocols = alpn_protocols
			context.verify_mode = OpenSSL::SSL::VERIFY_PEER
		end
	end

	# The options used to construct the client endpoint:
	let(:client_endpoint_options) do
		{ssl_context: client_context, protocol: protocol}
	end

	let(:app) {::Protocol::HTTP::Middleware::HelloWorld}

	def before
		super

		server_endpoint = Async::HTTP::Endpoint.parse("https://localhost:0", ssl_context: server_context)
		@bound_endpoint = server_endpoint.bound

		@server = Async::HTTP::Server.new(app, @bound_endpoint, protocol: Async::HTTP::Protocol::HTTPS, scheme: "https")
		@server_task = @server.run

		address = @bound_endpoint.sockets.first.to_io.local_address

		client_endpoint = Async::HTTP::Endpoint.parse("https://localhost:#{address.ip_port}", **client_endpoint_options)

		@client = Async::HTTP::Client.new(client_endpoint, retries: 3)
	end

	def after(error = nil)
		::Async::Task.current.with_timeout(1) do
			@client&.close

			if @server_task
				@server_task.stop

				begin
					@server_task.wait_all
				rescue OpenSSL::SSL::SSLError
					# TLS handshake failures (e.g. client certificate verification failure) surface in the server's connection tasks. They are an expected outcome of these tests.
				end
			end

			@bound_endpoint&.close
		end

		super
	end

	with "a server that negotiates http/1.1" do
		let(:alpn_selection) {"http/1.1"}

		it "uses the negotiated protocol" do
			response = @client.get("/")

			expect(response).to be(:success?)
			expect(response.read).to be == "Hello World!"
			expect(response.version).to be == "HTTP/1.1"
			expect(response.connection).to be_a(Async::HTTP::Protocol::HTTP1::Connection)
			expect(response.connection.version).to be == "HTTP/1.1"
		end

		it "reuses the pooled connection with its original negotiated protocol" do
			first_response = @client.get("/")
			first_response.read

			second_response = @client.get("/")
			second_response.read

			expect(second_response.connection).to be == first_response.connection
			expect(second_response.version).to be == "HTTP/1.1"
		end
	end

	with "a server that negotiates h2" do
		let(:alpn_selection) {"h2"}

		it "uses the negotiated protocol" do
			response = @client.get("/")

			expect(response).to be(:success?)
			expect(response.read).to be == "Hello World!"
			expect(response.version).to be == "HTTP/2"
			expect(response.connection).to be_a(Async::HTTP::Protocol::HTTP2::Client)
			expect(response.connection).to be(:http2?)
		end

		it "reuses the pooled connection with its original negotiated protocol" do
			first_response = @client.get("/")
			first_response.read

			second_response = @client.get("/")
			second_response.read

			expect(second_response.connection).to be == first_response.connection
			expect(second_response.version).to be == "HTTP/2"
		end
	end

	with "a client restricted to a specific range of protocols" do
		# The client is only willing to use HTTP/1.1, so it only offers "http/1.1" during ALPN negotiation:
		let(:alpn_protocols) {["http/1.1"]}

		# No explicit SSL context: the endpoint derives its ALPN protocols from the configured protocol negotiator:
		let(:client_endpoint_options) do
			{protocol: protocol}
		end

		it "offers only the configured protocols and uses the negotiated result" do
			response = @client.get("/")

			expect(response).to be(:success?)
			expect(response.read).to be == "Hello World!"
			expect(response.version).to be == "HTTP/1.1"
			expect(response.connection).to be_a(Async::HTTP::Protocol::HTTP1::Connection)
		end
	end

	with "a client that does not trust the server certificate" do
		# An empty certificate store, so the server's self-signed certificate is not trusted:
		let(:certificate_store) {OpenSSL::X509::Store.new}

		it "propagates the TLS failure instead of falling back to another protocol" do
			expect do
				@client.get("/")
			end.to raise_exception(OpenSSL::SSL::SSLError, message: be =~ /certificate verify failed/)
		end
	end
end