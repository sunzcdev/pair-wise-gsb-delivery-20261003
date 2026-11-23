Added bounded checkpoints to ``BufferedByteReceiveStream`` so that speculative reads
of an optional prefix can be committed or rolled back to the same stream position,
with explicit states for budget overruns (``CheckpointBudgetExceeded``), early EOF and
cancellation
