# frozen_string_literal: true

# Released under the MIT License.
# Copyright, 2026, by Samuel Williams.

require_relative "node"

module Async
	# A point-in-time sampling of the live task tree, designed for diagnostic code.
	#
	# When a snapshot is taken, the membership of the tree rooted at the given node is captured, along with the parent-child relationships at that time. The captured entries are then delivered to the caller in batches, so that large reports can be generated incrementally, allowing the caller to wait between batches, e.g. to avoid blocking the event loop.
	#
	# The live task tree continues to operate while a snapshot is being consumed:
	#
	# - Nodes that are attached after the snapshot is taken do not enter this round of sampling. They will be visible in the next snapshot.
	# - Nodes that finish or are moved to a different parent do not disappear from this round: they are still delivered exactly once, with the relationships captured at sampling time. When the visitor actually observes an entry, it can use {Entry#finished?} and {Entry#moved?} to recognize the current state of the underlying node.
	#
	# Snapshots are independent of each other and of the live tree: an observer may stop reading at any time and {close} its snapshot, releasing its read state without affecting other snapshots, the task tree, or the scheduler. The live {Node#traverse} traversal remains available and unaffected.
	class Snapshot
		include Enumerable
		
		# A single entry in a snapshot, capturing a node and its parent-child relationship at sampling time.
		class Entry
			# @parameter node [Node] The sampled node.
			# @parameter parent [Node | Nil] The parent of the node at sampling time.
			# @parameter level [Integer] The depth of the node relative to the root of the snapshot.
			def initialize(node, parent, level)
				@node = node
				@parent = parent
				@level = level
			end
			
			# @attribute [Node] The sampled node.
			attr :node
			
			# @attribute [Node | Nil] The parent of the node at sampling time.
			attr :parent
			
			# @attribute [Integer] The depth of the node relative to the root of the snapshot.
			attr :level
			
			# Whether the node has finished. Evaluated against the live node, so it reflects the state of the node when the entry is observed, rather than when the snapshot was taken.
			#
			# @returns [Boolean]
			def finished?
				@node.finished?
			end
			
			# Whether the node has moved to a different parent (or been detached from the tree) since the snapshot was taken. Evaluated against the live node, so it reflects the state of the node when the entry is observed, rather than when the snapshot was taken.
			#
			# @returns [Boolean]
			def moved?
				!@node.parent.equal?(@parent)
			end
			
			# @returns [String] A short description of the entry.
			def to_s
				state = String.new
				state << " finished" if finished?
				state << " moved" if moved?
				
				"#<#{self.class.name} #{@node} level=#{@level}#{state}>"
			end
			
			# @returns [String] A short description of the entry.
			alias inspect to_s
		end
		
		# Take a snapshot of the tree rooted at the given node, yield it to the block, and ensure it is closed when the block exits, even if the block exits early. Closing the snapshot releases its read state.
		#
		# @parameter node [Node] The root of the tree to sample.
		# @yields {|snapshot| ...} The snapshot.
		# @returns [Object] The result of the block.
		def self.open(node)
			snapshot = self.new(node)
			
			begin
				return yield(snapshot)
			ensure
				snapshot.close
			end
		end
		
		# Take a snapshot of the tree rooted at the given node.
		#
		# @parameter node [Node] The root of the tree to sample.
		def initialize(node)
			@entries = []
			@offset = 0
			@closed = false
			
			capture(node, nil, 0)
		end
		
		# @returns [Integer] The number of entries captured by this snapshot.
		def size
			@entries.size
		end
		
		# @returns [Integer] The number of entries remaining to be read.
		def remaining
			@entries.size - @offset
		end
		
		# @returns [Boolean] Whether all entries have been read.
		def empty?
			@offset == @entries.size
		end
		
		# @returns [Boolean] Whether the snapshot has been closed.
		def closed?
			@closed
		end
		
		# Close the snapshot, releasing its read state. Any entries that have not been read yet are discarded. Closing a snapshot does not affect the live task tree, other snapshots, or the scheduler.
		def close
			@entries = []
			@offset = 0
			@closed = true
			
			return nil
		end
		
		# Read the next batch of entries.
		#
		# @parameter count [Integer | Nil] The maximum number of entries to read. If nil, reads all remaining entries.
		# @raises [ArgumentError] If the count is less than 1.
		# @returns [Array(Entry) | Nil] The next batch of entries, or nil if there are no more entries.
		def next_batch(count = nil)
			return nil if empty?
			
			if count.nil?
				count = remaining
			elsif count < 1
				raise ArgumentError, "Count must be at least 1!"
			end
			
			batch = @entries[@offset, count]
			@offset += batch.size
			
			return batch
		end
		
		# Enumerate the remaining entries in batches. The caller may wait between batches, e.g. to avoid blocking the event loop; the live task tree continues to operate in the meantime.
		#
		# @parameter count [Integer | Nil] The maximum number of entries per batch. If nil, all remaining entries are delivered in a single batch.
		# @yields {|batch| ...} Each batch of entries.
		# @returns [Enumerator] If no block is given.
		def each_batch(count = nil)
			return enum_for(:each_batch, count) unless block_given?
			
			while batch = next_batch(count)
				yield batch
			end
			
			return self
		end
		
		# Enumerate the remaining entries, one at a time.
		#
		# @yields {|entry| ...} Each remaining entry.
		# @returns [Enumerator] If no block is given.
		def each
			return enum_for(:each) unless block_given?
			
			until empty?
				yield @entries[@offset]
				
				@offset += 1
			end
			
			return self
		end
		
		private
		
		# Capture the node and its descendants, recording the parent-child relationships at sampling time.
		def capture(node, parent, level)
			@entries << Entry.new(node, parent, level)
			
			node.children&.each do |child|
				capture(child, node, level + 1)
			end
		end
	end
	
	class Node
		# Take a point-in-time snapshot of the task tree rooted at this node, suitable for batched diagnostic reporting. See {Snapshot} for details.
		#
		# @returns [Snapshot] A snapshot of the tree rooted at this node.
		def snapshot
			Snapshot.new(self)
		end
	end
end
