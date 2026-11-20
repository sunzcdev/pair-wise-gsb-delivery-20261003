# frozen-string-literal: true

require 'time'

#
class Roda
  module RodaPlugins
    # The caching plugin adds methods related to HTTP caching.
    #
    # For proper caching, you should use either the +last_modified+ or
    # +etag+ request methods.  
    #
    #   r.get 'albums', Integer do |album_id|
    #     @album = Album[album_id]
    #     r.last_modified @album.updated_at
    #     view('album')
    #   end
    #
    #   # or
    #
    #   r.get 'albums', Integer do |album_id|
    #     @album = Album[album_id]
    #     r.etag @album.sha1
    #     view('album')
    #   end
    #
    # Both +last_modified+ or +etag+ will immediately halt processing
    # if there have been no modifications since the last time the
    # client requested the resource, assuming the client uses the
    # appropriate HTTP 1.1 request headers.
    #
    # If the last modified time and entity tag for a resource are not
    # available at the same time (for example, if they are stored on
    # different objects), you can use the +preconditions+ request method
    # after collecting both values.  It sets the appropriate response
    # headers and checks all request preconditions at once, instead of
    # checking each precondition as soon as the related value is available:
    #
    #   r.get 'albums', Integer do |album_id|
    #     album = Album[album_id]
    #     stats = Stats.for_album(album)
    #     r.preconditions etag: stats.sha1, last_modified: album.updated_at
    #     view('album')
    #   end
    #
    # This is useful for resources where +last_modified+ or +etag+ would
    # halt with a 304 or 412 response before all validators were known.
    # Unlike +last_modified+ and +etag+, +preconditions+ uses the
    # precondition evaluation order specified by RFC 9110, section 13.2.2,
    # so the result doesn't depend on the order the client sends the
    # request headers in, and modification requests (such as PUT and POST)
    # never receive a 304 response for a matching If-None-Match header.
    #
    # This plugin also includes the +cache_control+ and +expires+
    # response methods.  The +cache_control+ method sets the
    # Cache-Control header using the given hash:
    #
    #   response.cache_control public: true, max_age: 60
    #   # Cache-Control: public, max-age=60
    # 
    # The +expires+ method is similar, but in addition
    # to setting the HTTP 1.1 Cache-Control header, it also sets
    # the HTTP 1.0 Expires header:
    #
    #   response.expires 60, public: true
    #   # Cache-Control: public, max-age=60
    #   # Expires: Mon, 29 Sep 2014 21:25:47 GMT
    #
    # The implementation was originally taken from Sinatra,
    # which is also released under the MIT License:
    #
    # Copyright (c) 2007, 2008, 2009 Blake Mizerany
    # Copyright (c) 2010, 2011, 2012, 2013, 2014 Konstantin Haase
    # 
    # Permission is hereby granted, free of charge, to any person
    # obtaining a copy of this software and associated documentation
    # files (the "Software"), to deal in the Software without
    # restriction, including without limitation the rights to use,
    # copy, modify, merge, publish, distribute, sublicense, and/or sell
    # copies of the Software, and to permit persons to whom the
    # Software is furnished to do so, subject to the following
    # conditions:
    # 
    # The above copyright notice and this permission notice shall be
    # included in all copies or substantial portions of the Software.
    # 
    # THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND,
    # EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES
    # OF MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND
    # NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR COPYRIGHT
    # HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY,
    # WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING
    # FROM, OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR
    # OTHER DEALINGS IN THE SOFTWARE.
    module Caching
      module RequestMethods
        # Set the last modified time of the resource using the Last-Modified header.
        # The +time+ argument should be a Time instance.
        #
        # If the current request includes an If-Unmodified-Since header that is
        # before than the time specified, immediately returns a response
        # with a 412 status.
        #
        # If the current request does not include an If-None-Match header and
        # includes an If-Modified-Since header that is equal or later than the
        # time specified, immediately returns a response with a 304 status.
        def last_modified(time)
          return unless time
          res = response
          e = env
          res[RodaResponseHeaders::LAST_MODIFIED] = time.httpdate
          status = res.status

          if (!status || (status >= 200 && status < 300) || status == 412) && (ius = time_from_header(e['HTTP_IF_UNMODIFIED_SINCE'])) && ius < time.to_i
            res.status = 412
            halt
          end

          if !e['HTTP_IF_NONE_MATCH'] && (!status || status == 200) && (ims = time_from_header(e['HTTP_IF_MODIFIED_SINCE'])) && ims >= time.to_i
            res.status = 304
            halt
          end
        end

        # Set the response entity tag using the +ETag+ header.
        #
        # The +value+ argument is an identifier that uniquely
        # identifies the current version of the resource.
        # Keyword Arguments:
        # +weak+ :: Use a weak cache validator (a strong cache validator is the default)
        # +new_resource+ :: Whether this etag should match an etag of * (true for POST, false otherwise)
        #
        # When the current request includes an <tt>If-None-Match</tt> header with a
        # matching etag, immediately returns a response with a 304 or 412 status,
        # depending on the request method.
        #
        # When the current request includes an <tt>If-Match</tt> header with an etag that
        # doesn't match, or where the +weak+ argument was used, immediately returns a
        # response with a 412 status.
        def etag(value, opts=(no_positional_opts = true; OPTS), weak: opts[:weak], new_resource: opts.fetch(:new_resource){post?}, **kw)
          RodaPlugins.check_keywords(__callee__, no_positional_opts, opts, kw)
          
          res = response
          e = env
          res[RodaResponseHeaders::ETAG] = etag = "#{'W/' if weak}\"#{value}\""
          status = res.status

          if (!status || (status >= 200 && status < 300) || status == 304)
            if etag_matches?(e['HTTP_IF_NONE_MATCH'], etag, new_resource, weak: true)
              res.status = (request_method =~ /\AGET|HEAD|OPTIONS|TRACE\z/i ? 304 : 412)
              halt
            end

            if ifm = e['HTTP_IF_MATCH']
              if weak || !etag_matches?(ifm, etag, new_resource)
                res.status = 412
                halt
              end
            end
          end
        end

        # Set the response validators and check all request preconditions at
        # once.  This is designed for cases where the last modified time and
        # entity tag are not available at the same time, such as when they are
        # stored on different objects.  Instead of checking preconditions as
        # soon as each value is known (as +last_modified+ and +etag+ do), it
        # waits until the caller has collected every value it wants to use.
        #
        # Keyword Arguments:
        # +etag+ :: The current entity tag for the resource, or nil to skip setting ETag and checking etag-based preconditions
        # +last_modified+ :: The last modified time (a Time instance) for the resource, or nil to skip setting Last-Modified and checking time-based preconditions
        # +weak+ :: Use a weak cache validator for the etag (a strong cache validator is the default)
        # +new_resource+ :: Whether this etag should match an etag of * (true for POST, false otherwise)
        #
        #   r.preconditions etag: @stats.sha1, last_modified: @album.updated_at
        #
        # If no precondition fails, this returns nil and processing continues.
        # If a precondition fails, this immediately halts processing with the
        # appropriate status:
        #
        # 304 :: For GET and HEAD requests, when If-None-Match matches the current etag (using the weak comparison required for If-None-Match) or, if there is no If-None-Match header, when If-Modified-Since is not earlier than the last modified time.
        # 412 :: When If-Match does not match the current etag (using the strong comparison required for If-Match, so a weak etag only satisfies <tt>If-Match: *</tt> when the resource exists), when If-Unmodified-Since is earlier than the last modified time (and there is no If-Match header), or when If-None-Match matches for a request method other than GET or HEAD.
        #
        # The preconditions are evaluated in the order specified by RFC 9110,
        # section 13.2.2, so the result does not depend on the order the client
        # sends the request headers in.  If-Match takes precedence over
        # If-Unmodified-Since, and If-None-Match takes precedence over
        # If-Modified-Since.  Preconditions that require a validator that was
        # not provided are skipped.  Preconditions are only checked if the
        # response status has not been set, or has been set to a 2xx or 304
        # status.  This makes it safe to call this method before performing
        # any side effects for the request, since preconditions are checked
        # before the caller continues processing.
        def preconditions(opts=(no_positional_opts = true; OPTS), etag: opts[:etag], last_modified: opts[:last_modified], weak: opts[:weak], new_resource: opts.fetch(:new_resource){post?}, **kw)
          RodaPlugins.check_keywords(__callee__, no_positional_opts, opts, kw)

          res = response
          e = env
          etag = "#{'W/' if weak}\"#{etag}\"" if etag
          res[RodaResponseHeaders::ETAG] = etag if etag
          res[RodaResponseHeaders::LAST_MODIFIED] = last_modified.httpdate if last_modified

          status = res.status
          return unless !status || (status >= 200 && status < 300) || status == 304

          # If-Match is checked first.  If it is present, If-Unmodified-Since
          # must be ignored (RFC 9110, section 13.2.2).  If-Match uses the
          # strong comparison, so a weak etag only matches if the header is *.
          if ifm = e['HTTP_IF_MATCH']
            if etag && !etag_matches?(ifm, etag, new_resource, strong: true)
              res.status = 412
              halt
            end
          elsif last_modified && (ius = time_from_header(e['HTTP_IF_UNMODIFIED_SINCE'])) && ius < last_modified.to_i
            res.status = 412
            halt
          end

          # If-None-Match is checked next.  If it is present, If-Modified-Since
          # must be ignored (RFC 9110, section 13.2.2).  If-None-Match uses the
          # weak comparison, so a strong form of a weak etag also matches.
          if inm = e['HTTP_IF_NONE_MATCH']
            if etag && etag_matches?(inm, etag, new_resource, weak: true)
              res.status = get_or_head_request? ? 304 : 412
              halt
            end
          elsif last_modified && get_or_head_request? && (ims = time_from_header(e['HTTP_IF_MODIFIED_SINCE'])) && ims >= last_modified.to_i
            res.status = 304
            halt
          end
        end

        private

        # Whether the request method is GET or HEAD, the only methods for which
        # a failed If-None-Match or If-Modified-Since precondition results in a
        # 304 response instead of a 412 response.
        def get_or_head_request?
          request_method = @env['REQUEST_METHOD'].to_s.upcase
          request_method == 'GET' || request_method == 'HEAD'
        end

        # Helper method checking if a ETag value list includes the current ETag.
        # If +weak+ is true, uses the weak comparison that <tt>If-None-Match</tt> requires
        # (RFC 9110 section 13.1.2), which ignores a W/ prefix on both sides.
        # If +strong+ is true, uses the strong comparison that <tt>If-Match</tt> requires
        # (RFC 9110 section 13.1.1), which never matches a weak ETag.
        def etag_matches?(list, etag, new_resource, weak: false, strong: false)
          return unless list
          return !new_resource if list == '*'
          return false if strong && etag.start_with?('W/')
          list = list.to_s.split(/\s*,\s*/)
          if weak
            etag = etag.sub(/\AW\//, '')
            list.any? do |tag|
              tag.sub!(/\AW\//, '')
              tag == etag
            end
          else
            list.include?(etag)
          end
        end

        # Helper method parsing a time value from an HTTP header, returning the
        # time as an integer.
        def time_from_header(t)
          Time.httpdate(t).to_i if t
        rescue ArgumentError
          nil
        end
      end

      module ResponseMethods
        # Specify response freshness policy for using the <tt>Cache-Control</tt> header.
        # Options can can any non-value directives (:public, :private, :no_cache,
        # :no_store, :must_revalidate, :proxy_revalidate), with true as the value.
        # Options can also contain value directives (:max_age, :s_maxage).
        #
        #   response.cache_control public: true, max_age: 60
        #   # => Cache-Control: public, max-age=60
        #
        # See RFC 2616 / 14.9 for more on standard cache control directives:
        # http://tools.ietf.org/html/rfc2616#section-14.9.1
        def cache_control(opts)
          values = []
          opts.each do |k, v|
            next unless v
            k = k.to_s.tr('_', '-')
            values << (v == true ? k : "#{k}=#{v}")
          end

          @headers[RodaResponseHeaders::CACHE_CONTROL] = values.join(', ') unless values.empty?
        end

        # Set Cache-Control header with the max_age given.  max_age should
        # be an integer number of seconds that the current request should be
        # cached for.  Also sets the Expires header, useful if you have
        # HTTP 1.0 clients (Cache-Control is an HTTP 1.1 header).
        def expires(max_age, opts=OPTS)
          cache_control(opts.merge(:max_age=>max_age))
          @headers[RodaResponseHeaders::EXPIRES] = (Time.now + max_age).httpdate
        end

        # Remove Content-Type and Content-Length for 304 responses.
        def finish
          a = super
          if a[0] == 304
            h = a[1]
            h.delete(RodaResponseHeaders::CONTENT_TYPE)
            h.delete(RodaResponseHeaders::CONTENT_LENGTH)
          end
          a
        end
      end
    end

    register_plugin(:caching, Caching)
  end
end
