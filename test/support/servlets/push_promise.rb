# frozen_string_literal: true

require_relative "test"

class PushPromiseServer < TestHTTP2Server
  using HTTPX::URIExtensions

  def initialize(num_promises: 1, promises: nil, **kw)
    super(**kw)
    @promises = promises || num_promises.times.map { |i| { ":path" => "/#{i}" } }
  end

  private

  def handle_request(conn, stream)
    return super if conn.remote_settings.settings_enable_push.zero?

    response = "PARENT"
    stream.headers({
                     ":status" => "200",
                     "content-length" => response.bytesize.to_s,
                     "content-type" => "text/plain",
                   }, end_stream: false)

    authority = URI(origin).authority
    push_streams = []
    @promises.each_with_index do |extra, i|
      head = { ":method" => "GET",
               ":scheme" => "https",
               ":authority" => authority }.merge(extra)

      stream.promise(head) do |push|
        child_response = "CHILD #{i}"
        push.headers({
                       ":status" => "200",
                       "content-type" => "text/plain",
                       "content-length" => child_response.bytesize.to_s,
                       "x-http2-push" => "1",
                     })
        push_streams << [push, child_response]
      end
    end

    stream.data(response)

    push_streams.each do |push, payload|
      push.data(payload)
    end
  end
end
