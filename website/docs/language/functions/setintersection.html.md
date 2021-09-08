---
layout: "language"
page_title: "setintersection - Functions - Configuration Language"
sidebar_current: "docs-funcs-collection-setintersection"
description: |-
  The setintersection function takes multiple sets and produces a single set
  containing only the elements that all of the given sets have in common.
---

# `setintersection` Function

The `setintersection` function takes multiple sets and produces a single set
containing only the elements that all of the given sets have in common.
In other words, it computes the
[intersection](https://en.wikipedia.org/wiki/Intersection_(set_theory)) of the sets.

```hcl
setintersection(sets...)
```

## Examples

```
> setintersection(["a", "b"], ["b", "c"], ["b", "d"])
[
  "b",
]
```

The given arguments are converted to sets, so the result is also a set and
the ordering of the given elements is not preserved.

## Related Functions

* [`contains`](./contains.html) tests whether a given list or set contains
  a given element value.
* [`setproduct`](./setproduct.html) computes the _Cartesian product_ of multiple
  sets.
* [`setsubtract`](./setsubtract.html) computes the _relative complement_ of two sets
* [`setunion`](./setunion.html) computes the _union_ of
  multiple sets.
