Added the ``generation`` property and ``wait_generation()`` method to ``Condition``,
allowing tasks to wait for the next revision of shared state without losing
notifications that occur before the wait starts
