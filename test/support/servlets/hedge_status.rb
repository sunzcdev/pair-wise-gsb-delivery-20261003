# frozen_string_literal: true

require_relative "test"

# Responds with a sequence of status codes, one per accepted request, in order.
# Useful for hedging tests, where two attempts race each other and the test needs
# deterministic control over which status each attempt gets.
#
#   server = HedgeStatusServer.new(statuses: [500, 200])
class HedgeStatusServer < TestServer
  class StatusApp < WEBrick::HTTPServlet::AbstractServlet
    def initialize(server, statuses, mutex, counter)
      super(server)
      @statuses = statuses
      @mutex = mutex
      @counter = counter
    end

    def do_GET(_req, res)
      idx = @mutex.synchronize do
        current = @counter[0]
        @counter[0] += 1
        current
      end

      res.status = @statuses[idx] || @statuses.last
      res["content-type"] = "text/plain"
      res.body = "response-#{idx}"
    end
  end

  attr_reader :request_count

  def initialize(statuses:, **options)
    super(options)
    @statuses = statuses
    @mutex = Mutex.new
    @counter = [0]
    @request_count = @counter
    mount("/", StatusApp, statuses, @mutex, @counter)
  end
end
