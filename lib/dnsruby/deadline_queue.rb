module Dnsruby
  # Indexed minimum heap. Updating or cancelling a deadline removes its old
  # entry immediately, so storage never exceeds the number of live owners.
  # Callers provide synchronization alongside their corresponding query tables.
  class DeadlineQueue # :nodoc: all
    def initialize
      @heap = []
      @positions = {}
    end

    def first
      @heap.first
    end

    def first_deadline
      @heap.first&.last
    end

    def []=(key, deadline)
      index = @positions[key]
      if index
        @heap[index][1] = deadline
        move_down(move_up(index))
      else
        @positions[key] = @heap.length
        @heap << [key, deadline]
        move_up(@heap.length - 1)
      end
      deadline
    end

    def delete(key)
      index = @positions.delete(key)
      return unless index
      removed = @heap[index]
      last = @heap.pop
      if index < @heap.length
        @heap[index] = last
        @positions[last[0]] = index
        move_down(move_up(index))
      end
      removed[1]
    end

    private

    def swap(left, right)
      @heap[left], @heap[right] = @heap[right], @heap[left]
      @positions[@heap[left][0]] = left
      @positions[@heap[right][0]] = right
    end

    def move_up(index)
      while index > 0
        parent = (index - 1) / 2
        break if @heap[parent][1] <= @heap[index][1]
        swap(parent, index)
        index = parent
      end
      index
    end

    def move_down(index)
      loop do
        child = index * 2 + 1
        break if child >= @heap.length
        right = child + 1
        child = right if right < @heap.length && @heap[right][1] < @heap[child][1]
        break if @heap[index][1] <= @heap[child][1]
        swap(index, child)
        index = child
      end
    end
  end
end
