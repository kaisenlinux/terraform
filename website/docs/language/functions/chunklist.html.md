---
layout: "language"
page_title: "chunklist - Functions - Configuration Language"
sidebar_current: "docs-funcs-collection-chunklist"
description: |-
  The chunklist function splits a single list into fixed-size chunks, returning
  a list of lists.
---

# `chunklist` Function

`chunklist` splits a single list into fixed-size chunks, returning a list
of lists.

```hcl
chunklist(list, chunk_size)
```

## Examples

```
> chunklist(["a", "b", "c", "d", "e"], 2)
[
  [
    "a",
    "b",
  ],
  [
    "c",
    "d",
  ],
  [
    "e",
  ],
]
> chunklist(["a", "b", "c", "d", "e"], 1)
[
  [
    "a",
  ],
  [
    "b",
  ],
  [
    "c",
  ],
  [
    "d",
  ],
  [
    "e",
  ],
]
```
