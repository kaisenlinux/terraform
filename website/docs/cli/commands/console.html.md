---
layout: "docs"
page_title: "Command: console"
sidebar_current: "docs-commands-console"
description: "The terraform console command provides an interactive console for
  evaluating expressions."
---

# Command: console

The `terraform console` command provides an interactive console for
evaluating [expressions](/docs/language/expressions/index.html).

## Usage

Usage: `terraform console [options]`

This command provides an interactive command-line console for evaluating and
experimenting with [expressions](/docs/language/expressions/index.html).
This is useful for testing interpolations before using them in configurations,
and for interacting with any values currently saved in
[state](/docs/language/state/index.html).

If the current state is empty or has not yet been created, the console can be
used to experiment with the expression syntax and
[built-in functions](/docs/language/functions/index.html).

You can close the console with the `exit` command or by pressing Control-C
or Control-D.

For configurations using
[the `local` backend](/docs/language/settings/backends/local.html) only,
`terraform console` accepts the legacy command line option
[`-state`](/docs/language/settings/backends/local.html#command-line-arguments).

## Scripting

The `terraform console` command can be used in non-interactive scripts
by piping newline-separated commands to it. Only the output from the
final command is printed unless an error occurs earlier.

For example:

```shell
$ echo "1 + 5" | terraform console
6
```

## Remote State

If [remote state](/docs/language/state/remote.html) is used by the current backend,
Terraform will read the state for the current workspace from the backend
before evaluating any expressions.
