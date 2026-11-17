# --
# Copyright 2007 Nominet UK
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
# ++
module Dnsruby
  # Explicit bookkeeping for the 16-bit DNS message IDs multiplexed onto a
  # single TCP connection. RFC 7766 section 6.2.1 forbids two outstanding
  # transactions on one connection from sharing an ID, and RFC 1035 section
  # 4.1.1 only provides 65536 values, so reuse needs explicit reservation and
  # release boundaries rather than an overwritable table entry.
  #
  # A registry is owned by exactly one connection generation. Replacing the
  # connection (a rollover after EOF, retirement, a collision or exhaustion)
  # starts a new registry with a new generation, so a response arriving on the
  # old stream can never be handed to a query admitted on the new one. Every
  # reservation also records the question section, which is re-checked when a
  # response is matched because the ID alone is not unique.
  #
  # SelectThread owns these objects and only mutates them while holding its
  # state mutex; the registry itself therefore needs no internal lock.
  class TcpMessageRegistry
    # A 16-bit DNS header ID ranges from 0 to 65535.
    HEADER_ID_RANGE = (0..65_535).freeze
    CAPACITY = HEADER_ID_RANGE.size
    # Reservation outcomes the caller must distinguish. :id_collision means the
    # caller-chosen ID is already reserved on this generation; :saturated means
    # every 16-bit ID is reserved and the connection must be refused/rolled.
    ID_COLLISION = :id_collision
    SATURATED = :saturated

    Entry = Struct.new(:header_id, :client_query_id, :question_signature,
      :generation, :serial, :answered, :question_optional, keyword_init: true) do
      def answered?
        answered
      end
    end
    private_constant :Entry

    attr_reader :generation, :capacity

    def initialize(generation:, capacity: CAPACITY)
      unless generation.is_a?(Integer) && generation >= 1
        raise ArgumentError, 'connection generation must be a positive integer'
      end
      unless capacity.is_a?(Integer) && capacity >= 1 && capacity <= CAPACITY
        raise ArgumentError, "capacity must be between 1 and #{CAPACITY}"
      end
      @generation = generation
      @capacity = capacity
      @entries = {}
      # Monotonic reservation number within this connection generation. It
      # never reuses a value, even after the 16-bit header ID has wrapped.
      @next_serial = 0
    end

    # Reserve the caller's header ID before any byte is written. The question
    # signature and connection generation travel with the reservation so an
    # accepted response can be proven to belong to this exact transaction.
    # question_optional mirrors the legacy dispatch rule for raw messages and
    # dynamic updates, which match on ID alone (a raw packet keeps the caller's
    # untouched header; an UPDATE reply is validated as a transaction instead).
    # Returns the reservation serial, ID_COLLISION, or SATURATED. An existing
    # entry is never overwritten.
    def reserve(header_id, client_query_id, question_signature, question_optional: false)
      validate_header_id(header_id)
      return ID_COLLISION if @entries.key?(header_id)
      return SATURATED if @entries.size >= @capacity
      serial = (@next_serial += 1)
      @entries[header_id] = Entry.new(header_id: header_id,
        client_query_id: client_query_id,
        question_signature: question_signature,
        generation: @generation, serial: serial, answered: false,
        question_optional: question_optional)
      serial
    end

    # Release an ID at an explicit boundary: a completed answer, cancellation,
    # timeout or connection teardown. Only the owner recorded when the ID was
    # reserved can release it, so a stale late response cannot remove a newer
    # reservation. Returns the released entry, or nil when there is nothing to
    # release or the owner/generation no longer matches.
    def release(header_id, client_query_id, answered: false)
      entry = @entries[header_id]
      return nil unless entry && entry.client_query_id == client_query_id
      entry.answered = true if answered
      @entries.delete(header_id)
    end

    def reserved?(header_id)
      @entries.key?(header_id)
    end

    def size
      @entries.size
    end

    def empty?
      @entries.empty?
    end

    # Look up the live reservation for a response. The connection generation is
    # checked first: a frame decoded from a previous generation's stream has no
    # owner here. The question section is then compared so two transactions
    # that happen to reuse an ID cannot receive each other's answers. Raw and
    # UPDATE reservations skip the question comparison by contract.
    def match(header_id, response_questions, allow_questionless_error: false)
      entry = @entries[header_id]
      return nil unless entry && entry.generation == @generation
      return entry if entry.question_optional
      return entry if allow_questionless_error && response_questions.empty?
      return entry if entry.question_signature.nil? ||
        signatures_match?(entry.question_signature, response_questions)
      nil
    end

    def entry_for(header_id)
      @entries[header_id]
    end

    # Every reservation still owned by this connection generation. Used when
    # the stream is torn down so each owner is reclaimed exactly once.
    def each_entry
      return enum_for(:each_entry) unless block_given?
      @entries.each_value { |entry| yield entry }
    end

    # A stable, comparable snapshot of a query's question section. Question
    # objects already implement value equality; freeze the array so the
    # signature cannot drift while the transaction is outstanding.
    def self.question_signature(questions)
      return nil if questions.nil?
      questions.dup.freeze
    end

    private

    def validate_header_id(header_id)
      return if HEADER_ID_RANGE.include?(header_id)
      raise ArgumentError, "DNS header ID must be in 0..65535, got #{header_id.inspect}"
    end

    def signatures_match?(signature, response_questions)
      signature.length == response_questions.length &&
        signature.each_with_index.all? { |question, index| question == response_questions[index] }
    end
  end
end
