Added
-----

*   Preserve Media RSS ``media:group`` structure in a new ``media_groups`` entry field. Each group keeps its ``media_defaults`` and its enclosed ``media:content`` records, and every content exposes inherited ``media_effective`` metadata. Media content outside a group is exposed in ``media_ungrouped_content`` without inheriting any ended group defaults.
