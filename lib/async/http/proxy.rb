# frozen_string_literal: true

# Released under the MIT License.
# Copyright, 2019-2026, by Samuel Williams.

require_relative "client"
require_relative "endpoint"

require_relative "body/pipe"

module Async
	module HTTP
		# Wraps a client, address and headers required to initiate a connectio to a remote host using the CONNECT verb.
		# Behaves like a TCP endpoint for the purposes of connecting to a remote host.
		class Proxy
			# Raised when a CONNECT tunnel through a proxy cannot be established.
			class ConnectFailure < StandardError
				# Initialize the failure with the unsuccessful response.
				# @parameter response [Protocol::HTTP::Response] The failed response from the proxy.
				def initialize(response)
					super "Failed to connect: #{response.status}"
					@response = response
				end
				
				attr :response
			end
			
			# Extends {Async::HTTP::Client} with proxy capabilities.
			module Client
				# Create a proxy instance for the given endpoint.
				# @parameter endpoint [Endpoint] The target endpoint to tunnel to.
				# @parameter headers [Hash | Nil] Optional headers to send with the CONNECT request.
				# @returns [Proxy] A proxy instance for establishing tunnels.
				def proxy(endpoint, headers = nil, mirror: nil)
					Proxy.new(self, endpoint.authority(false), headers, mirror: mirror)
				end
				
				# Create a client that will proxy requests through the current client.
				def proxied_client(endpoint, headers = nil, mirror: nil)
					proxy = self.proxy(endpoint, headers, mirror: mirror)
					
					return self.class.new(proxy.wrap_endpoint(endpoint))
				end
				
				# Create an endpoint that connects via this proxy.
				# @parameter endpoint [Endpoint] The target endpoint.
				# @parameter headers [Hash | Nil] Optional headers for the CONNECT request.
				# @returns [Endpoint] An endpoint that tunnels through the proxy.
				def proxied_endpoint(endpoint, headers = nil, mirror: nil)
					proxy = self.proxy(endpoint, headers, mirror: mirror)
					
					return proxy.wrap_endpoint(endpoint)
				end
			end
			
			# Prepare and endpoint which can establish a TCP connection to the remote system.
			# @param client [Async::HTTP::Client] the client which will be used as a proxy server.
			# @param host [String] the hostname or address to connect to.
			# @param port [String] the port number to connect to.
			# @param headers [Array] an optional list of headers to use when establishing the connection.
			# @see IO::Endpoint#tcp
			def self.tcp(client, host, port, headers = nil, configuration = nil, mirror: nil)
				mirror ||= configuration
				self.new(client, "#{host}:#{port}", headers, mirror: mirror)
			end
			
			# Construct a endpoint that will use the given client as a proxy for HTTP requests.
			# @param client [Async::HTTP::Client] the client which will be used as a proxy server.
			# @param endpoint [Async::HTTP::Endpoint] the endpoint to connect to.
			# @param headers [Array] an optional list of headers to use when establishing the connection.
			def self.endpoint(client, endpoint, headers = nil, mirror: nil)
				proxy = self.new(client, endpoint.authority(false), headers, mirror: mirror)
				
				return proxy.endpoint(endpoint.url)
			end
			
			# @param client [Async::HTTP::Client] the client which will be used as a proxy server.
			# @param address [String] the address to connect to.
			# @param headers [Array] an optional list of headers to use when establishing the connection.
			# @param mirror [Body::Mirror | Integer | Hash | Boolean | Nil] an optional
			# 	default bounded mirror for forwarded response chunks.
			def initialize(client, address, headers = nil, configuration = nil, mirror: nil, **options)
				@client = client
				@address = address
				@headers = ::Protocol::HTTP::Headers[headers].freeze
				mirror ||= configuration
				mirror ||= options unless options.empty?
				@mirror = coerce_mirror(mirror == true ? {} : mirror)
				@mirror_options = options
			end

			attr :client
			attr :mirror
			
			# Close the underlying client connection.
			def close
				@client.close
			end
			
			# Establish a TCP connection to the specified host.
			#
			# @parameter mirror [Body::Mirror | Integer | Hash | Boolean | Nil] Override
			# 	the proxy's default bounded mirror for this tunnel.
			# @yields [IO, Body::Mirror | Nil] The connected IO and optional mirror.
			# @return [Socket] a connected bi-directional socket.
			def connect(configuration = nil, mirror: :__use_proxy_default__, &block)
				input = Body::Writable.new
				mirror = configuration || mirror
				mirror = coerce_mirror(mirror == :__use_proxy_default__ ? @mirror : mirror)

				response = @client.connect(authority: @address, headers: @headers, body: input)

				if response.success?
					pipe = Body::Pipe.new(response.body, input, mirror: mirror)
					io = pipe.to_io

					if mirror
						io.define_singleton_method(:mirror) { mirror }
						io.define_singleton_method(:pipe) { pipe }
					end

					if block_given?
						begin
							yield io, mirror
						ensure
							pipe.close
						end
					else
						io
					end
				else
					# This ensures we don't leave a response dangling:
					input.close
					response.close

					raise ConnectFailure, response
				end
			end

			# Establish a tunnel and return both its IO and bounded mirror.
			#
			# @return [Array(IO, Body::Mirror)]
			def connect_mirrored(limit: Body::Mirror::DEFAULT_MAX_CHUNKS, **options)
				mirror = Body::Mirror.new(max_chunks: limit, **options)
				io = connect(mirror: mirror)

				[io, mirror]
			end
			
			# @return [Async::HTTP::Endpoint] an endpoint that connects via the specified proxy.
			def wrap_endpoint(endpoint)
				Endpoint.new(endpoint.url, self, **endpoint.options)
			end

			private

			def coerce_mirror(mirror)
				case mirror
				when Body::Mirror
					mirror
				when Integer
					Body::Mirror.new(max_chunks: mirror)
				when Hash
					Body::Mirror.new(**mirror)
				when true
					Body::Mirror.new
				else
					nil
				end
			end
		end
		
		Client.prepend(Proxy::Client)
	end
end
