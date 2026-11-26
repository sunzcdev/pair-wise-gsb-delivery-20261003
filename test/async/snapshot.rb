# frozen_string_literal: true

# Released under the MIT License.
# Copyright, 2026, by Samuel Williams.

require "async/snapshot"
require "async/notification"

require "sus/fixtures/async"

describe Async::Snapshot do
	let(:root) {Async::Node.new(annotation: "root")}
	let(:branch_a) {Async::Node.new(root, annotation: "branch-a")}
	let(:branch_b) {Async::Node.new(root, annotation: "branch-b")}
	let(:leaf_a1) {Async::Node.new(branch_a, annotation: "a1")}
	let(:leaf_a2) {Async::Node.new(branch_a, annotation: "a2")}
	let(:leaf_b1) {Async::Node.new(branch_b, annotation: "b1")}
	let(:leaf_b2) {Async::Node.new(branch_b, annotation: "b2")}
	
	def build_tree
		leaf_a1
		leaf_a2
		leaf_b1
		leaf_b2
	end
	
	it "captures membership and relationships at sampling time" do
		build_tree
		
		snapshot = root.snapshot
		
		expect(snapshot.size).to be == 7
		expect(snapshot.remaining).to be == 7
		
		entries = snapshot.to_a
		
		expect(entries.map(&:node)).to be == [root, branch_a, leaf_a1, leaf_a2, branch_b, leaf_b1, leaf_b2]
		expect(entries.map(&:parent)).to be == [nil, root, branch_a, branch_a, root, branch_b, branch_b]
		expect(entries.map(&:level)).to be == [0, 1, 2, 2, 1, 2, 2]
	end
	
	it "delivers entries in batches" do
		build_tree
		
		snapshot = root.snapshot
		
		batch = snapshot.next_batch(3)
		expect(batch.map(&:node)).to be == [root, branch_a, leaf_a1]
		expect(snapshot.remaining).to be == 4
		
		batches = snapshot.each_batch(3).to_a
		expect(batches.map{|batch| batch.map(&:node)}).to be == [[leaf_a2, branch_b, leaf_b1], [leaf_b2]]
		
		expect(snapshot).to be(:empty?)
		expect(snapshot.next_batch).to be_nil
	end
	
	it "rejects non-positive batch sizes" do
		build_tree
		
		snapshot = root.snapshot
		
		expect{snapshot.next_batch(0)}.to raise_exception(ArgumentError, message: be =~ /at least 1/)
	end
	
	it "does not include nodes attached after sampling" do
		build_tree
		
		snapshot = root.snapshot
		
		extra = Async::Node.new(branch_b, annotation: "extra")
		
		expect(snapshot.to_a.map(&:node)).not.to be(:include?, extra)
	end
	
	it "retains members that are removed or moved after sampling" do
		build_tree
		
		snapshot = root.snapshot
		
		# Detach a member and move another one to a different branch:
		leaf_a1.consume
		leaf_b1.parent = branch_a
		
		# Both still appear exactly once, with the relationships captured at sampling time:
		entries = snapshot.to_a
		expect(entries.map(&:node)).to be == [root, branch_a, leaf_a1, leaf_a2, branch_b, leaf_b1, leaf_b2]
		
		a1 = entries[2]
		expect(a1.parent).to be == branch_a
		expect(a1).to be(:finished?)
		expect(a1).to be(:moved?)
		
		b1 = entries[5]
		expect(b1.parent).to be == branch_b
		expect(b1).to be(:moved?)
		expect(b1.node.parent).to be == branch_a
		
		# Untouched members are neither finished nor moved:
		expect(entries[1]).not.to be(:finished?)
		expect(entries[1]).not.to be(:moved?)
	end
	
	it "evaluates finished? and moved? when the entry is observed" do
		build_tree
		
		snapshot = root.snapshot
		entry = snapshot.next_batch(2).last
		
		expect(entry.node).to be == branch_a
		expect(entry).not.to be(:moved?)
		
		# The state is evaluated against the live node, not captured eagerly:
		branch_a.parent = nil
		expect(entry).to be(:moved?)
	end
	
	it "can be closed, releasing its read state, without affecting other observers" do
		build_tree
		
		first = root.snapshot
		second = root.snapshot
		
		first.next_batch(2)
		second.next_batch(3)
		
		first.close
		
		expect(first).to be(:closed?)
		expect(first.size).to be == 0
		expect(first.next_batch).to be_nil
		
		# The other observer is unaffected:
		expect(second).not.to be(:closed?)
		expect(second.remaining).to be == 4
		expect(second.next_batch(4).size).to be == 4
	end
	
	it "closes the snapshot when the block exits, even if it exits early" do
		build_tree
		
		snapshot = nil

		result = Async::Snapshot.open(root) do |opened|
			snapshot = opened
			opened.next_batch(1)
			break :early
		end
		
		expect(result).to be == :early
		expect(snapshot).to be(:closed?)
	end
	
	it "does not affect the live tree" do
		build_tree
		
		root.snapshot.to_a
		
		expect(root.children.size).to be == 2
		expect(root.traverse.to_a.size).to be == 7
	end
	
	with "a live task tree" do
		include Sus::Fixtures::Async::ReactorContext
		
		it "samples a two-level task tree in batches while the tree keeps changing" do
			gate = Async::Notification.new
			finish_a1 = Async::Notification.new

			# Build a two-level task tree:
			parent = reactor.async(annotation: "parent") {gate.wait}
			branch_a = parent.async(annotation: "branch-a") {gate.wait}
			branch_b = parent.async(annotation: "branch-b") {gate.wait}
			a1 = branch_a.async(annotation: "a1") {finish_a1.wait}
			a2 = branch_a.async(annotation: "a2") {gate.wait}
			b1 = branch_b.async(annotation: "b1") {gate.wait}
			b2 = branch_b.async(annotation: "b2") {gate.wait}

			# Take a snapshot of the two-level tree:
			snapshot = parent.snapshot
			expect(snapshot.size).to be == 7
			
			# A second observer starts reading the same tree:
			other = parent.snapshot
			
			# Read the first small batch:
			batch = snapshot.next_batch(2)
			expect(batch.map(&:node)).to be == [parent, branch_a]
			expect(batch.map(&:finished?)).to be == [false, false]
			
			# After the first batch is delivered, let an already listed child finish:
			finish_a1.signal

			# Move a still-unlisted child to an already visited branch:
			b1.parent = branch_a

			# Add a new child to an unvisited branch:
			new_child = branch_b.async(annotation: "b3") {gate.wait}

			# The caller may wait between batches, allowing the reactor to run:
			sleep(0.01)
			expect(a1.status).to be == :completed

			# The second observer leaves early, releasing its read state:
			expect(other.next_batch(1).map(&:node)).to be == [parent]
			other.close
			expect(other).to be(:closed?)
			expect(other.next_batch).to be_nil

			# Continue consuming the first snapshot in batches:
			entries = []
			snapshot.each_batch(2) do |batch|
				entries.concat(batch)

				# Waiting between batches is allowed:
				Async::Task.current.yield
			end

			# Unlisted original members still appear exactly once, with the relationships captured at sampling time:
			expect(entries.map(&:node)).to be == [a1, a2, branch_b, b1, b2]

			a1_entry, a2_entry, branch_b_entry, b1_entry, b2_entry = entries

			# The finished child is recognized as finished:
			expect(a1_entry).to be(:finished?)
			expect(a1_entry.node.status).to be == :completed

			# The moved child is reported with its parent at sampling time, and can be observed to have migrated:
			expect(b1_entry.parent).to be == branch_b
			expect(b1_entry).to be(:moved?)
			expect(b1_entry.node.parent).to be == branch_a

			# Untouched members are neither finished nor moved:
			expect(a2_entry).not.to be(:finished?)
			expect(a2_entry).not.to be(:moved?)
			expect(branch_b_entry).not.to be(:moved?)

			# The new child did not enter this round:
			seen = batch + entries
			expect(seen.map(&:node)).not.to be(:include?, new_child)

			# ... but appears in the next diagnosis, in its new position:
			next_snapshot = parent.snapshot
			next_entries = next_snapshot.to_a
			expect(next_entries.map(&:node)).to be == [parent, branch_a, a2, b1, branch_b, b2, new_child]

			# The live traversal entry point remains available and reflects the current tree:
			expect(parent.traverse.to_a.map(&:first)).to be == [parent, branch_a, a2, b1, branch_b, b2, new_child]
			
			# The scheduler is unaffected and the tree keeps running:
			probe = parent.async {:result}
			expect(probe.wait).to be == :result
		ensure
			parent&.stop
		end
	end
end
