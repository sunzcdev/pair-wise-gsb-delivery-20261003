# W3C XML conformance-style samples

The XML files in this directory are compact conformance-style samples modeled
on the test categories used by the W3C XML Conformance Test Suites
(XML 1.0: https://www.w3.org/XML/Test/ and XML 1.1).

- Files prefixed `valid-` must be accepted by the strict parser for the
  version named in the XML declaration (`1.1` samples under XML 1.1, the rest
  under XML 1.0).
- Files prefixed `invalid-` must be rejected. `invalid-dtd.xml` must produce
  `ErrUnsupportedDTD`.

They exercise UTF-8 with a byte order mark, multibyte content, namespaces,
predefined entities, CDATA sections, comments, XML 1.1 restricted character
references, XML 1.1 line endings, duplicate attributes, mismatched tags,
unclosed elements, invalid names, undefined entities, illegal character
references, DTD rejection, non-UTF-8 encoding rejection, multiple roots and
illegal comments.
