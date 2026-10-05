# Deterministic MED

By default, MED is only compared between paths from the same neighboring AS.
GoBGP inserts paths in arrival order, so paths from different neighboring ASes
can produce a different best path depending on processing order. Enabling
`deterministic-med` makes this selection independent of arrival order by
selecting the best path within each neighboring-AS group first and comparing
only those group winners afterward.

```toml
[global.route-selection-options.config]
  deterministic-med = true
```

Rules:

- The option is disabled by default, preserving the existing behavior.
- With `deterministic-med` enabled, MED is never compared between different
  neighboring AS groups.
- A missing MED attribute is treated as MED 0, including within its group.
- Enabling both `deterministic-med` and `always-compare-med` makes
  `always-compare-med` take precedence, so MED is compared globally.
- When ECMP is enabled with `deterministic-med`, equal multipath candidates
  must belong to the winning neighboring-AS group. With
  `always-compare-med`, equal global MED paths from different neighboring ASes
  can remain ECMP-eligible, subject to the existing ECMP criteria.

The final set of paths produces the same best path regardless of whether they
arrived incrementally, were re-added after withdrawal, or were loaded as a
batch.
