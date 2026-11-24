# frozen_string_literal: true

# Released under the MIT License.
# Copyright, 2018-2024, by Samuel Williams.

require "protocol/http/body/writable"
require "async/queue"
require_relative "mirror"

# @namespace
module Async
	# @namespace
	module HTTP
		# @namespace
		module Body
			# Read interception used by {Writable#mirror}.
			module WritableMirroring
				# Attach (or return) the bounded diagnostic mirror for this body.
				#
				# @parameter mirror [Mirror | Integer | Nil] An existing mirror, a chunk
				# 	limit, or nil to create a default bounded mirror.
				# @returns [Mirror]
				def mirror(mirror = nil, **options, &block)
					if mirror.is_a?(Mirror)
						@mirror = mirror
					elsif mirror.is_a?(Hash)
						options = mirror.merge(options)
						@mirror ||= Mirror.new(**options)
					elsif mirror || !options.empty? || @mirror.nil?
						options[:max_chunks] ||= mirror if mirror.is_a?(Integer)
						@mirror ||= Mirror.new(**options)
					end

					@mirror.observe(&block) if block_given?
					@mirror
				end

				# All mirrors attached to this body.  The current public API supports
				# one mirror; this array keeps pipe cleanup code uniform.
				def mirrors
					@mirror ? [@mirror] : []
				end

				def read
					chunk = super

					if @mirror
						if chunk.nil?
							@mirror.close_write
						else
							@mirror.capture(chunk)
						end
					end

					chunk
				rescue => error
					@mirror&.close_write(error)
					raise
				end

				# Deliberately do not finish the mirror in #close_write: chunks may still
				# be queued for the primary reader.  #read closes the mirror only once
				# the primary has drained those chunks and reaches EOF.

				def close(error = nil)
					# Publish EOF/error to the mirror without clearing its independent,
					# already captured queue.
					@mirror&.close_write(error)
					super
				end
			end

			unless ::Protocol::HTTP::Body::Writable.include?(WritableMirroring)
				::Protocol::HTTP::Body::Writable.prepend(WritableMirroring)
			end

			Writable = ::Protocol::HTTP::Body::Writable
		end
	end
end
