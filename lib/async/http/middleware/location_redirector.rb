# frozen_string_literal: true

# Released under the MIT License.
# Copyright, 2024-2026, by Samuel Williams.

require "protocol/http/middleware"
require "protocol/http/body/rewindable"
require "protocol/url/reference"

module Async
	module HTTP
		# @namespace
		module Middleware
			# A client wrapper which transparently handles redirects to a given maximum number of hops.
			#
			# The default implementation will only follow relative locations (i.e. those without a scheme) and will switch to GET if the original request was not a GET.
			#
			# Absolute locations are not followed by default and the redirect response is returned to the caller instead. Pass `follow_absolute_location: true` to also follow absolute locations whose scheme and authority match the request (i.e. same-origin locations). Cross-origin absolute locations are still returned to the caller, regardless of this option.
			#
			# Each call independently tracks the request targets it has already visited. Revisiting the same target (including the request method, so a POST switched to a GET counts as a different request) raises a {CircularRedirect} carrying the full, traceable path of targets.
			#
			# The best reference for these semantics is defined by the [Fetch specification](https://fetch.spec.whatwg.org/#http-redirect-fetch).
			#
			# | Redirect using GET                        | Permanent | Temporary |
			# |:-----------------------------------------:|:---------:|:---------:|
			# | Allowed                                   | 301       | 302       |
			# | Preserve original method                  | 308       | 307       |
			#
			# For the specific details of the redirect handling, see:
			# - <https://datatracker.ietf.org/doc/html/rfc7231#section-6-4-2> 301 Moved Permanently.
			# - <https://datatracker.ietf.org/doc/html/rfc7231#section-6-4-3> 302 Found.
			# - <https://datatracker.ietf.org/doc/html/rfc7538 308 Permanent Redirect.
			# - <https://datatracker.ietf.org/doc/html/rfc7231#section-6-4-7> 307 Temporary Redirect.
			#
			class LocationRedirector < ::Protocol::HTTP::Middleware
				# Raised when the maximum number of redirects has been exceeded.
				class TooManyRedirects < StandardError
				end

				# Raised when a redirect leads back to a request target which has already been visited during the same call.
				#
				# The {#path} attribute contains every target visited, in order, including the repeated target which closed the loop, so the redirect cycle can be traced.
				class CircularRedirect < StandardError
					# Initialize the circular redirect error.
					#
					# @parameter path [Array(String)] The visited request targets, in order, including the repeated target which closed the loop.
					def initialize(path)
						@path = path

						super("Circular redirect detected: #{path.join(" -> ")}")
					end

					# @attribute [Array(String)] The visited request targets, in order, including the repeated target which closed the loop.
					attr :path
				end

				# Header keys which should be deleted when changing a request from a POST to a GET as defined by <https://fetch.spec.whatwg.org/#request-body-header-name>.
				PROHIBITED_GET_HEADERS = [
					"content-encoding",
					"content-language",
					"content-location",
					"content-type",
				]

				# maximum_hops is the max number of redirects. Set to 0 to allow 1 request with no redirects.
				#
				# @parameter follow_absolute_location [Boolean] When true, absolute locations which share the same scheme and authority as the request (same-origin) are followed as well. Cross-origin absolute locations are never followed.
				def initialize(app, maximum_hops = 3, follow_absolute_location: false)
					super(app)

					@maximum_hops = maximum_hops
					@follow_absolute_location = follow_absolute_location
				end

				# The maximum number of hops which will limit the number of redirects until an error is thrown.
				attr :maximum_hops

				# Whether same-origin absolute locations are followed.
				# @returns [Boolean]
				attr :follow_absolute_location
				
				# Determine whether the redirect should switch the request method to GET.
				# @parameter request [Protocol::HTTP::Request] The original request.
				# @parameter response [Protocol::HTTP::Response] The redirect response.
				# @returns [Boolean] Whether the method should be changed to GET.
				def redirect_with_get?(request, response)
					# We only want to switch to GET if the request method is something other than get, e.g. POST.
					if request.method != GET
						# According to the RFC, we should only switch to GET if the response is a 301 or 302:
						return response.status == 301 || response.status == 302
					end
				end
				
				# Determine whether an absolute location shares the same scheme and authority (host and port) as the current request.
				#
				# @parameter request [Protocol::HTTP::Request] The current request.
				# @parameter uri [URI::Generic] The parsed absolute location.
				# @returns [Boolean] Whether the location is same-origin with the request.
				def same_origin?(request, uri)
					return false unless request.scheme && request.authority

					# Parse with the request's scheme so an implicit port resolves to the
					# default port for that scheme (e.g. 80 for http, 443 for https):
					origin = URI.parse("#{request.scheme}://#{request.authority}")

					return uri.scheme == origin.scheme &&
						uri.hostname == origin.hostname &&
						uri.port == origin.port
				end

				# Handle a redirect to a relative or (when enabled) same-origin absolute location.
				#
				# Relative locations are always followed. Absolute locations are only followed when {follow_absolute_location} is enabled and the location is same-origin with the request; any other absolute location (including all cross-origin locations) is left for the caller.
				#
				# @parameter request [Protocol::HTTP::Request] The original request, which you can modify if you want to handle the redirect.
				# @parameter location [String] The location to redirect to.
				# @returns [Boolean] True if the redirect was handled, false if it was not.
				def handle_redirect(request, location)
					uri = URI.parse(location)

					if uri.absolute?
						return false unless @follow_absolute_location && same_origin?(request, uri)

						# Keep the path (including query string) of the same-origin location:
						request.path = uri.request_uri
					else
						# Update the path of the request, preserving whatever query string the location targets:
						request.path = ::Protocol::URL::Reference[request.path] + location
					end

					# Follow the redirect:
					return true
				end
				
				# Build an identity for the current request target.
				#
				# The request method is part of the identity, so a request which was switched from another method (e.g. POST) to GET counts as a different request to the same path. The path includes the query string, so targets which only differ by query are distinct.
				#
				# @parameter request [Protocol::HTTP::Request] The current request.
				# @returns [String] An identity string for the request target.
				def request_identity(request)
					return "#{request.method} #{request.path}"
				end

				# Make a request, transparently following redirects up to {maximum_hops} times.
				# @parameter request [Protocol::HTTP::Request] The request to send.
				# @returns [Protocol::HTTP::Response] The final response.
				# @raises [CircularRedirect] If a redirect leads back to a request target already visited in this call.
				def call(request)
					# We don't want to follow redirects for HEAD requests:
					return super if request.head?

					body = ::Protocol::HTTP::Body::Rewindable.wrap(request)
					hops = 0

					# Track the targets visited by this call only, so concurrent calls never interfere with each other:
					visited = []

					while hops <= @maximum_hops
						identity = request_identity(request)

						if visited.include?(identity)
							raise CircularRedirect, visited + [identity]
						end

						visited << identity

						response = super(request)

						if response.redirection?
							hops += 1

							# Get the redirect location:
							unless location = response.headers["location"]
								return response
							end

							response.finish

							unless handle_redirect(request, location)
								return response
							end

							# Ensure the request (body) is finished and set to nil before we manipulate the request:
							request.finish

							if request.method == GET or response.preserve_method?
								# We (might) need to rewind the body so that it can be submitted again:
								body&.rewind
								request.body = body
							else
								# We are changing the method to GET:
								request.method = GET

								# We will no longer be submitting the body:
								body = nil

								# Remove any headers which are not allowed in a GET request:
								PROHIBITED_GET_HEADERS.each do |header|
									request.headers.delete(header)
								end
							end
						else
							return response
						end
					end

					raise TooManyRedirects, "Redirected #{hops} times, exceeded maximum!"
				end
			end
		end
	end
end
