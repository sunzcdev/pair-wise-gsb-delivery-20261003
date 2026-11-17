
PyBreaker
=========

PyBreaker is a Python implementation of the Circuit Breaker pattern, described
in Michael T. Nygard's book `Release It!`_.

In Nygard's words, *"circuit breakers exists to allow one subsystem to fail
without destroying the entire system. This is done by wrapping dangerous
operations (typically integration points) with a component that can circumvent
calls when the system is not healthy"*.


Features
--------

* Configurable list of excluded exceptions (e.g. business exceptions)
* Configurable failure threshold and reset timeout
* Support for several event listeners per circuit breaker
* Can guard generator functions
* Functions and properties for easy monitoring and management
* Thread-safe
* Optional redis backing
* Optional support for asynchronous Tornado calls


Requirements
------------

* `Python`_ 3.10+


Installation
------------

Run the following command line to download the latest stable version of
PyBreaker from `PyPI`_::

    $ pip install pybreaker

If you are a `Git`_ user, you might want to install the current development
version in editable mode::

    $ git clone git://github.com/danielfm/pybreaker.git
    $ cd pybreaker
    $ # run tests (on windows omit ./)
    $ ./pw test
    $ pip install -e .


Usage
-----

The first step is to create an instance of ``CircuitBreaker`` for each
integration point you want to protect against:

.. code:: python

    import pybreaker

    # Used in database integration points
    db_breaker = pybreaker.CircuitBreaker(fail_max=5, reset_timeout=60)


``CircuitBreaker`` instances should live globally inside the application scope,
e.g., live across requests.

You can also configure a success threshold to require multiple successful
requests before closing the circuit breaker:

.. code:: python

    # Require 3 successful requests before closing
    db_breaker = pybreaker.CircuitBreaker(fail_max=5, reset_timeout=60, success_threshold=3)

.. note::

  Integration points to external services (i.e. databases, queues, etc) are
  more likely to fail, so make sure to always use timeouts when accessing such
  services if there's support at the API level.

If you'd like to use the Redis backing, initialize the ``CircuitBreaker`` with
a ``CircuitRedisStorage``:

.. code:: python

    import pybreaker
    import redis

    redis = redis.StrictRedis()
    db_breaker = pybreaker.CircuitBreaker(
        fail_max=5,
        reset_timeout=60,
        state_storage=pybreaker.CircuitRedisStorage(pybreaker.STATE_CLOSED, redis))

**Do not** initialize the Redis connection with the ``decode_responses`` set to
``True``, this will force returning ASCII objects from redis and in Python3+ will
fail with:

    `AttributeError: 'str' object has no attribute 'decode'`


.. note::

  You may want to reuse a connection already created in your application, if you're
  using ``django_redis`` for example:

.. code:: python

    import pybreaker
    from django_redis import get_redis_connection

    db_breaker = pybreaker.CircuitBreaker(
        fail_max=5,
        reset_timeout=60,
        state_storage=pybreaker.CircuitRedisStorage(pybreaker.STATE_CLOSED, get_redis_connection('default')))

.. note::

  If you require multiple, independent CircuitBreakers and wish to store their states in Redis, it is essential to assign a ``unique namespace`` for each
  CircuitBreaker instance. This can be achieved by specifying a distinct namespace parameter in the CircuitRedisStorage constructor. for example:

.. code:: python

    import pybreaker
    from django_redis import get_redis_connection

    db_breaker = pybreaker.CircuitBreaker(
        fail_max=5,
        reset_timeout=60,
        state_storage=pybreaker.CircuitRedisStorage(pybreaker.STATE_CLOSED, get_redis_connection('default'),namespace='unique_namespace'))

Event Listening
```````````````

There's no need to subclass ``CircuitBreaker`` if you just want to take action
when certain events occur. In that case, it's better to subclass
``CircuitBreakerListener`` instead:

.. code:: python

    class DBListener(pybreaker.CircuitBreakerListener):
        "Listener used by circuit breakers that execute database operations."

        def before_call(self, cb, func, *args, **kwargs):
            "Called before the circuit breaker `cb` calls `func`."
            pass

        def state_change(self, cb, old_state, new_state):
            "Called when the circuit breaker `cb` state changes."
            pass

        def failure(self, cb, exc):
            "Called when a function invocation raises a system error."
            pass

        def success(self, cb):
            "Called when a function invocation succeeds."
            pass

    class LogListener(pybreaker.CircuitBreakerListener):
        "Listener used to log circuit breaker events."

        def state_change(self, cb, old_state, new_state):
            msg = "State Change: CB: {0}, New State: {1}".format(cb.name, new_state)
            logging.info(msg)


To add listeners to a circuit breaker:

.. code:: python

    # At creation time...
    db_breaker = pybreaker.CircuitBreaker(listeners=[DBListener(), LogListener()])

    # ...or later
    db_breaker.add_listeners(OneListener(), AnotherListener())


What Does a Circuit Breaker Do?
```````````````````````````````

Let's say you want to use a circuit breaker on a function that updates a row
in the ``customer`` database table:

.. code:: python

    @db_breaker
    def update_customer(cust):
        # Do stuff here...
        pass

    # Will trigger the circuit breaker
    updated_customer = update_customer(my_customer)


Or if you don't want to use the decorator syntax:

.. code:: python

    def update_customer(cust):
        # Do stuff here...
        pass

    # Will trigger the circuit breaker
    updated_customer = db_breaker.call(update_customer, my_customer)

Or use it as a context manager and a `with` statement:

.. code:: python

    # Will trigger the circuit breaker
    with db_breaker.calling():
        # Do stuff here...
        pass



According to the default parameters, the circuit breaker ``db_breaker`` will
automatically open the circuit after 5 consecutive failures in
``update_customer``.

When the circuit is open, all calls to ``update_customer`` will fail immediately
(raising ``CircuitBreakerError``) without any attempt to execute the real
operation. If you want the original error to be thrown when the circuit trips,
set the ``throw_new_error_on_trip`` option to ``False``:

.. code:: python

    pybreaker.CircuitBreaker(..., throw_new_error_on_trip=False)


After 60 seconds, the circuit breaker will allow the next call to
``update_customer`` pass through. If that call succeeds, the circuit is closed;
if it fails, however, the circuit is opened again until another timeout elapses.

Optional Tornado Support
````````````````````````
A circuit breaker can (optionally) be used to call asynchronous Tornado functions:

.. code:: python

    from tornado import gen

    @db_breaker(__pybreaker_call_async=True)
    @gen.coroutine
    def async_update(cust):
        # Do async stuff here...
        pass

Or if you don't want to use the decorator syntax:

.. code:: python

    @gen.coroutine
    def async_update(cust):
        # Do async stuff here...
        pass

    updated_customer = db_breaker.call_async(async_update, my_customer)


Excluding Exceptions
````````````````````

By default, a failed call is any call that raises an exception. However, it's
common to raise exceptions to also indicate business exceptions, and those
exceptions should be ignored by the circuit breaker as they don't indicate
system errors:

.. code:: python

    # At creation time...
    db_breaker = CircuitBreaker(exclude=[CustomerValidationError])

    # ...or later
    db_breaker.add_excluded_exception(CustomerValidationError)


In that case, when any function guarded by that circuit breaker raises
``CustomerValidationError`` (or any exception derived from
``CustomerValidationError``), that call won't be considered a system failure.

So as to cover cases where the exception class alone is not enough to determine
whether it represents a system error, you may also pass a callable rather than
a type:

.. code:: python

    db_breaker = CircuitBreaker(exclude=[lambda e: type(e) == HTTPError and e.status_code < 500])

You may mix types and filter callables freely.


Monitoring and Management
`````````````````````````

A circuit breaker provides properties and functions you can use to monitor and
change its current state:

.. code:: python

    # Get the current number of consecutive failures
    print(db_breaker.fail_counter)

    # Get the current number of consecutive successes
    print(db_breaker.success_counter)

    # Get/set the maximum number of consecutive failures
    print(db_breaker.fail_max)
    db_breaker.fail_max = 10

    # Get/set the success threshold
    print(db_breaker.success_threshold)
    db_breaker.success_threshold = 3

    # Get/set the current reset timeout period (in seconds)
    print(db_breaker.reset_timeout)
    db_breaker.reset_timeout = 60

    # Get the current state, i.e., 'open', 'half-open', 'closed'
    print(db_breaker.current_state)

    # Closes the circuit
    db_breaker.close()

    # Half-opens the circuit
    db_breaker.half_open()

    # Opens the circuit
    db_breaker.open()


These properties and functions might and should be exposed to the operations
staff somehow as they help them to detect problems in the system.

Changing the Policy at Runtime
``````````````````````````````

The parameters that decide when the circuit opens (`fail_max`) and recovers
(`reset_timeout`, `success_threshold`) can be grouped into a
``CircuitBreakerPolicy``:

.. code:: python

    import pybreaker

    breaker = pybreaker.CircuitBreaker(fail_max=5, reset_timeout=60)

    # Same values as a policy object; behaves exactly like the keyword arguments
    print(breaker.policy)
    breaker.policy = pybreaker.CircuitBreakerPolicy(fail_max=10, reset_timeout=30)

The legacy attributes keep working and are validated, so
``breaker.fail_max = 10`` remains supported. A value that cannot be used
(a negative threshold, a non-numeric timeout, ...) raises
``pybreaker.InvalidPolicyError`` and the circuit breaker keeps its previous
policy, so it is never left half configured.

Before changing the policy of a live circuit breaker you usually want to know
whether the new values would open it right away. ``evaluate_policy`` reports
that without executing the guarded operation, without touching the counters,
the state or the listeners:

.. code:: python

    evaluation = breaker.evaluate_policy(
        pybreaker.CircuitBreakerPolicy(fail_max=2, reset_timeout=10, success_threshold=1)
    )

    # Closed circuit: does the recorded failure count already prime the breaker
    # to open on the very next failure under the new fail_max?
    print(evaluation.would_trip_on_next_failure)

    # Open circuit: would a call be rejected now, and for how much longer?
    print(evaluation.would_open)
    print(evaluation.remaining_open_seconds)

    # Half-open: would the next success reach the new success_threshold?
    print(evaluation.would_close_on_next_success)

An outcome of ``None`` means "the recorded observations do not allow a
decision", and the observations that were unavailable are listed in
``evaluation.missing_history``. For example, a storage backend that predates
success counters cannot report them, and pybreaker reports that recovery
cannot be determined instead of assuming zero failures or successes.

Because an evaluation describes the observations it saw, applying it blindly
could overwrite newer observations recorded by concurrent calls. Apply it
through ``apply_policy_evaluation``, which re-reads the state and refuses the
evaluation if anything changed in the meantime:

.. code:: python

    evaluation = breaker.evaluate_policy(
        pybreaker.CircuitBreakerPolicy(fail_max=2, reset_timeout=10, success_threshold=1)
    )
    ...
    try:
        breaker.apply_policy_evaluation(evaluation)
    except pybreaker.StalePolicyEvaluationError:
        # A call failed/succeeded, or the state changed, since the evaluation;
        # evaluate again against the new observations before applying.
        ...

Contributing
-------------

Run tests::

    $ ./pw test

Code formatting (black and isort) and linting (mypy) ::

    $ ./pw format
    $ ./pw lint

Above commands will automatically install the necessary tools inside *.pyprojectx*
and also install pre-commit hooks.

List available commands::

    $ ./pw -i

.. _Python: http://python.org
.. _Jython: http://jython.org
.. _Release It!: https://pragprog.com/titles/mnee2/release-it-second-edition/
.. _PyPI: http://pypi.python.org
.. _Git: http://git-scm.com
