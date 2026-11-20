Add ``--seed-manifest`` and ``--strict-seed-manifest`` options so fixed runners can pin the seed
packages they accept. A manifest entry names the distribution, exact version, SHA-256 digest and
optional wheel file name; selection only accepts a wheel that matches every pin and supports the
target interpreter, checking the search dirs before the bundled fallback. Strict mode rejects any
seed package the manifest does not name, while ordinary virtualenv selection is unchanged when no
manifest is given.
