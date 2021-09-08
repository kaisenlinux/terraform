---
layout: "language"
page_title: "Function Calls - Configuration Language"
---

# Function Calls

> **Hands-on:** Try the [Perform Dynamic Operations with Functions](https://learn.hashicorp.com/tutorials/terraform/functions?in=terraform/configuration-language&utm_source=WEBSITE&utm_medium=WEB_IO&utm_offer=ARTICLE_PAGE&utm_content=DOCS) tutorial on HashiCorp Learn.

The Terraform language has a number of
[built-in functions](/docs/language/functions/index.html) that can be used
in expressions to transform and combine values. These
are similar to the operators but all follow a common syntax:

```hcl
<FUNCTION NAME>(<ARGUMENT 1>, <ARGUMENT 2>)
```

The function name specifies which function to call. Each defined function
expects a specific number of arguments with specific value types, and returns a
specific value type as a result.

Some functions take an arbitrary number of arguments. For example, the `min`
function takes any amount of number arguments and returns the one that is
numerically smallest:

```hcl
min(55, 3453, 2)
```

A function call expression evaluates to the function's return value.

## Available Functions

For a full list of available functions, see
[the function reference](/docs/language/functions/index.html).

## Expanding Function Arguments

If the arguments to pass to a function are available in a list or tuple value,
that value can be _expanded_ into separate arguments. Provide the list value as
an argument and follow it with the `...` symbol:

```hcl
min([55, 2453, 2]...)
```

The expansion symbol is three periods (`...`), not a Unicode ellipsis character
(`…`). Expansion is a special syntax that is only available in function calls.

## Using Sensitive Data as Function Arguments

When using sensitive data, such as [an input variable](https://www.terraform.io/docs/language/values/variables.html#suppressing-values-in-cli-output)
or [an output defined](https://www.terraform.io/docs/language/values/outputs.html#sensitive-suppressing-values-in-cli-output) as sensitive
as function arguments, the result of the function call will be marked as sensitive.

This is a conservative behavior that is true irrespective of the function being
called. For example, passing an object containing a sensitive input variable to
the `keys()` function will result in a list that is sensitive:

```shell
> local.baz
{
  "a" = (sensitive)
  "b" = "dog"
}
> keys(local.baz)
(sensitive)
```

## When Terraform Calls Functions

Most of Terraform's built-in functions are, in programming language terms,
[pure functions](https://en.wikipedia.org/wiki/Pure_function). This means that
their result is based only on their arguments and so it doesn't make any
practical difference when Terraform would call them.

However, a small subset of functions interact with outside state and so for
those it can be helpful to know when Terraform will call them in relation to
other events that occur in a Terraform run.

The small set of special functions includes
[`file`](/docs/language/functions/file.html),
[`templatefile`](/docs/language/functions/templatefile.html),
[`timestamp`](/docs/language/functions/timestamp.html),
and [`uuid`](/docs/language/functions/uuid.html).
If you are not working with these functions then you don't need
to read this section, although the information here may still be interesting
background information.

The `file` and `templatefile` functions are intended for reading files that
are included as a static part of the configuration and so Terraform will
execute these functions as part of initial configuration validation, before
taking any other actions with the configuration. That means you cannot use
either function to read files that your configuration might generate
dynamically on disk as part of the plan or apply steps.

The `timestamp` function returns a representation of the current system time
at the point when Terraform calls it, and the `uuid` function returns a random
result which differs on each call. Without any special behavior these would
would both cause the final configuration during the apply step not to match the
actions shown in the plan, which violates the Terraform execution model.

For that reason, Terraform arranges for both of those functions to produce
[unknown value](references.html#values-not-yet-known) results during the
plan step, with the real result being decided only during the apply step.
For `timestamp` in particular, this means that the recorded time will be
the instant when Terraform began applying the change, rather than when
Terraform _planned_ the change.

For more details on the behavior of these functions, refer to their own
documentation pages.
