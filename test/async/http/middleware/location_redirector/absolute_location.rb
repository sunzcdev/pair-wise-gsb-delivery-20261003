# frozen_string_literal: true

# Released under the MIT License.
# Copyright, 2026, by Samuel Williams.

require "async/http/middleware/location_redirector"
require "async/http/mock"
require "async/http/client"

require "sus/fixtures/async/reactor_context"

describe Async::HTTP::Middleware::LocationRedirector do
	include Sus::Fixtures::Async::ReactorContext

	# A local (in-process) stand-in server which records every request it sees and
	# releases the bodies of every redirect response it hands out.
	class MockApplication
		ORIGIN = "http://localhost"

		def initialize
			@requests = []
			@released = []
		end

		attr :requests
		attr :released

		def call(request)
			@requests << [request.method, request.path]

			case [request.method, request.path]
			# POST starts at the same address it will later reach as a GET:
			in ["POST", "/start"]
				redirect(302, "/start")
			# Two segments which bounce between a relative and a same-origin absolute location:
			in ["GET", "/start"]
				redirect(302, "/relative")
			in ["GET", "/relative"]
				redirect(301, "#{ORIGIN}/absolute")
			in ["GET", "/absolute"]
				redirect(302, "/relative")
			# Parallel short chain which only changes the query, sharing the opening target:
			in ["GET", "/shared"]
				redirect(302, "/shared?variant=one")
			in ["GET", "/shared?variant=one"]
				redirect(302, "/final?variant=one")
			in ["GET", "/final?variant=one"]
				success("final-one")
			in ["GET", "/shared?variant=two"]
				redirect(302, "/final?variant=two")
			in ["GET", "/final?variant=two"]
				success("final-two")
			# Targets used to verify default and cross-origin handling:
			in ["GET", "/default-absolute"]
				redirect(302, "#{ORIGIN}/elsewhere")
			in ["GET", "/cross-origin"]
				redirect(302, "http://example.com/elsewhere")
			in ["GET", "/elsewhere"]
				success("elsewhere")
			# Relative redirects which report the method that finally reached them:
			in [_, "/301-check"]
				redirect(301, "/method-target")
			in [_, "/302-check"]
				redirect(302, "/method-target")
			in [_, "/307-check"]
				redirect(307, "/method-target")
			in [_, "/308-check"]
				redirect(308, "/method-target")
			in [_, "/method-target"]
				success(request.method)
			# An infinite relative loop, used to exercise the hop limit:
			in [_, "/loop-a"]
				redirect(302, "/loop-b")
			in [_, "/loop-b"]
				redirect(302, "/loop-a")
			else
				success("unexpected")
			end
		end

		private

		def redirect(status, location)
			response = Protocol::HTTP::Response[status, {"location" => location}, []]
			response.finish
			@released << [status, location]
			return response
		end

		def success(body)
			return Protocol::HTTP::Response[200, {}, [body]]
		end
	end

	let(:endpoint) {Async::HTTP::Mock::Endpoint.new}
	let(:application) {MockApplication.new}
	let(:server_task) {@server_task = Async do
		endpoint.run do |request|
			application.call(request)
		end
	end}
	let(:client) {
		server_task
		@client = Async::HTTP::Client.new(endpoint)
	}
	let(:redirector) {subject.new(client, 10, follow_absolute_location: true)}

	after do
		@client&.close
		@server_task&.stop
	end

	it "follows relative and same-origin absolute locations, detecting the second GET" do
		error = nil

		begin
			redirector.post("/start")
		rescue subject::CircularRedirect => exception
			error = exception
		end

		expect(error).not.to be_nil

		# The traceable path contains every visited target, including the repeated
		# target which closed the loop:
		expect(error.path).to be == [
			"POST /start",
			"GET /start",
			"GET /relative",
			"GET /absolute",
			"GET /relative",
		]

		expect(error.message).to be =~ /circular redirect/i
		# The path loops back to the first GET /relative:
		expect(error.message).to be =~ /GET \/absolute -> GET \/relative/
		expect(error.path.last).to be == error.path[2]

		# Four redirect responses were processed along the way and each one was
		# finished/released by the redirector (the stand-in finishes them itself too):
		expect(application.released).to be == [
			[302, "/start"],
			[302, "/relative"],
			[301, "http://localhost/absolute"],
			[302, "/relative"],
		]
	end

	it "treats the POST and later GET of the same address as different requests" do
		expect do
			redirector.post("/start")
		end.to raise_exception(subject::CircularRedirect)

		# POST /start is visited once, then GET /start is a fresh, different request.
		# The loop is raised before the repeated GET /relative is ever sent:
		expect(application.requests).to be == [
			["POST", "/start"],
			["GET", "/start"],
			["GET", "/relative"],
			["GET", "/absolute"],
		]
	end

	with "absolute location handling" do
		it "does not follow absolute locations by default" do
			default_redirector = subject.new(client, 10)

			response = default_redirector.get("/default-absolute")

			expect(response).to be(:redirection?)
			expect(response.headers["location"]).to be == "#{MockApplication::ORIGIN}/elsewhere"
		ensure
			response&.finish
		end

		it "returns cross-origin absolute locations to the caller even when enabled" do
			response = redirector.get("/cross-origin")

			expect(response).to be(:redirection?)
			expect(response.headers["location"]).to be == "http://example.com/elsewhere"

			# The cross-origin target was never requested:
			expect(application.requests).to be == [["GET", "/cross-origin"]]
		ensure
			response&.finish
		end
	end

	with "parallel calls" do
		it "keeps hops and visited targets independent per call" do
			# Run two chains concurrently. They share the opening target but diverge
			# via their query strings:
			results = {}

			first = Async do
				results[:one] = redirector.get("/shared")
			end

			second = Async do
				results[:two] = redirector.get("/shared?variant=two")
			end

			first.wait
			second.wait

			expect(results[:one]).to be(:success?)
			expect(results[:one].read).to be == "final-one"

			expect(results[:two]).to be(:success?)
			expect(results[:two].read).to be == "final-two"

			# Both chains walked their own targets, including the shared opening:
			expect(application.requests).to be(:include?, ["GET", "/shared"])
			expect(application.requests).to be(:include?, ["GET", "/shared?variant=one"])
			expect(application.requests).to be(:include?, ["GET", "/shared?variant=two"])
			expect(application.requests).to be(:include?, ["GET", "/final?variant=one"])
			expect(application.requests).to be(:include?, ["GET", "/final?variant=two"])

			# The shared opening target was only requested once, and neither call
			# mistook the other chain's query variants for a loop:
			expect(application.requests.count(["GET", "/shared"])).to be == 1
		ensure
			results.each_value(&:close)
		end
	end

	with "relative redirect semantics" do
		it "switches POST to GET on 301 and 302" do
			expect(redirector.post("/301-check").read).to be == "GET"
			expect(redirector.post("/302-check").read).to be == "GET"
		end

		it "preserves the request method on 307 and 308" do
			expect(redirector.post("/307-check").read).to be == "POST"
			expect(redirector.post("/308-check").read).to be == "POST"
		end

		it "does not treat a single relative hop as a loop" do
			expect(redirector.get("/302-check")).to be(:success?)
		end
	end

	with "hop limit" do
		it "raises TooManyRedirects once the maximum number of hops is exhausted" do
			# One hop reaches /loop-b; the next hop would exceed the limit before
			# the two-step loop has a chance to close:
			strict_redirector = subject.new(client, 1, follow_absolute_location: true)

			expect do
				strict_redirector.get("/loop-a")
			end.to raise_exception(subject::TooManyRedirects, message: be =~ /maximum/)
		end

		it "raises CircularRedirect for a two-step loop before the hop limit" do
			# A large hop limit does not mask an eventually repeating target:
			strict_redirector = subject.new(client, 10)

			expect do
				strict_redirector.get("/loop-a")
			end.to raise_exception(subject::CircularRedirect) do |error|
				expect(error.path).to be == ["GET /loop-a", "GET /loop-b", "GET /loop-a"]
			end
		end
	end
end
