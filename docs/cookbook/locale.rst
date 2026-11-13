=======
Locales
=======

agate strives to work equally well for users from all parts of the world. This means properly handling foreign currencies, date formats, etc. To facilitate this, agate makes a hard distinction between *your* locale and the locale of *the data* you are working with. This allows you to work seamlessly with data from other countries.

Set your locale
===============

Setting your locale will change how numbers are displayed when you print an agate :class:`.Table` or serialize it to, for example, a CSV file. This works the same as it does for any other Python module. See the :mod:`locale` documentation for details. Changing your locale will not affect how they are parsed from the files you are using. To change how data is parsed see :ref:`specify_locale_of_numbers`.

.. _specify_locale_of_numbers:

Specify locale of numbers
=========================

To correctly parse numbers from non-US locales, you must pass a :code:`locale` parameter to the :class:`.Number` constructor. For example, to parse Dutch numbers (which use a period to separate thousands and a comma to separate fractions):

.. code-block:: python

    dutch_numbers = agate.Number(locale='nl_NL')

    column_names = ['city', 'population']
    column_types = [text_type, dutch_numbers]

    table = agate.Table.from_csv('dutch_cities.csv', columns)

Validate digit grouping
=======================

By default, :class:`.Number` removes grouping symbols without checking their placement, so an incorrectly grouped value such as :code:`12,34,567` is parsed as a number. Pass :code:`strict_grouping=True` to validate the integer portion against the locale's primary and secondary grouping lengths. Ungrouped values are still accepted, but grouping symbols are rejected in the fractional and exponent portions:

.. code-block:: python

    strict_numbers = agate.Number(strict_grouping=True)

    strict_numbers.cast('1,000,000')  # Decimal('1000000')
    strict_numbers.cast('1000000')    # Decimal('1000000')
    strict_numbers.cast('12,34,567')  # raises CastError

The locale's pattern drives the rules, so Indian-style grouping works as well:

.. code-block:: python

    indian_numbers = agate.Number(locale='en_IN', strict_grouping=True)

    indian_numbers.cast('1,23,456')    # Decimal('123456')
    indian_numbers.cast('1,234,567')  # raises CastError

The same rules apply during type inference, because :meth:`.DataType.test` uses :meth:`.Number.cast`:

.. code-block:: python

    tester = agate.TypeTester(types=[agate.Number(strict_grouping=True), agate.Text()])
