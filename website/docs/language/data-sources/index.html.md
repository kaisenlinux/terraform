---
layout: "language"
page_title: "Data Sources - Configuration Language"
sidebar_current: "docs-config-data-sources"
description: "Data sources allow Terraform to use external data, function output, and data from other configurations. Learn data resource arguments, behavior, and lifecycle."
---

# Data Sources

_Data sources_ allow Terraform to use information defined outside of Terraform,
defined by another separate Terraform configuration, or modified by functions.

> **Hands-on:** Try the [Query Data Sources](https://learn.hashicorp.com/tutorials/terraform/data-sources) tutorial on HashiCorp Learn.

Each [provider](/docs/language/providers/index.html) may offer data sources
alongside its set of [resource](/docs/language/resources/index.html)
types.

## Using Data Sources

A data source is accessed via a special kind of resource known as a
_data resource_, declared using a `data` block:

```hcl
data "aws_ami" "example" {
  most_recent = true

  owners = ["self"]
  tags = {
    Name   = "app-server"
    Tested = "true"
  }
}
```

A `data` block requests that Terraform read from a given data source ("aws_ami")
and export the result under the given local name ("example"). The name is used
to refer to this resource from elsewhere in the same Terraform module, but has
no significance outside of the scope of a module.

The data source and name together serve as an identifier for a given
resource and so must be unique within a module.

Within the block body (between `{` and `}`) are query constraints defined by
the data source. Most arguments in this section depend on the
data source, and indeed in this example `most_recent`, `owners` and `tags` are
all arguments defined specifically for [the `aws_ami` data source](https://registry.terraform.io/providers/hashicorp/aws/latest/docs/data-sources/ami).

When distinguishing from data resources, the primary kind of resource (as declared
by a `resource` block) is known as a _managed resource_. Both kinds of resources
take arguments and export attributes for use in configuration, but while
managed resources cause Terraform to create, update, and delete infrastructure
objects, data resources cause Terraform only to _read_ objects. For brevity,
managed resources are often referred to just as "resources" when the meaning
is clear from context.

## Data Source Arguments

Each data resource is associated with a single data source, which determines
the kind of object (or objects) it reads and what query constraint arguments
are available.

Each data source in turn belongs to a [provider](/docs/language/providers/index.html),
which is a plugin for Terraform that offers a collection of resource types and
data sources that most often belong to a single cloud or on-premises
infrastructure platform.

Most of the items within the body of a `data` block are defined by and
specific to the selected data source, and these arguments can make full
use of [expressions](/docs/language/expressions/index.html) and other dynamic
Terraform language features.

However, there are some "meta-arguments" that are defined by Terraform itself
and apply across all data sources. These arguments often have additional
restrictions on what language features can be used with them, and are described
in more detail in the following sections.

## Data Resource Behavior

If the query constraint arguments for a data resource refer only to constant
values or values that are already known, the data resource will be read and its
state updated during Terraform's "refresh" phase, which runs prior to creating a plan.
This ensures that the retrieved data is available for use during planning and
so Terraform's plan will show the actual values obtained.

Query constraint arguments may refer to values that cannot be determined until
after configuration is applied, such as the id of a managed resource that has
not been created yet. In this case, reading from the data source is deferred
until the apply phase, and any references to the results of the data resource
elsewhere in configuration will themselves be unknown until after the
configuration has been applied.

## Local-only Data Sources

While many data sources correspond to an infrastructure object type that
is accessed via a remote network API, some specialized data sources operate
only within Terraform itself, calculating some results and exposing them
for use elsewhere.

For example, local-only data sources exist for
[rendering templates](https://registry.terraform.io/providers/hashicorp/template/latest/docs/data-sources/file),
[reading local files](https://registry.terraform.io/providers/hashicorp/local/latest/docs/data-sources/file), and
[rendering AWS IAM policies](https://registry.terraform.io/providers/hashicorp/aws/latest/docs/data-sources/iam_policy_document).

The behavior of local-only data sources is the same as all other data
sources, but their result data exists only temporarily during a Terraform
operation, and is re-calculated each time a new plan is created.

## Data Resource Dependencies

Data resources have the same dependency resolution behavior
[as defined for managed resources](/docs/language/resources/behavior.html#resource-dependencies).
Setting the `depends_on` meta-argument within `data` blocks defers reading of
the data source until after all changes to the dependencies have been applied.

In order to ensure that data sources are accessing the most up to date
information possible in a wide variety of use cases, arguments directly
referencing managed resources are treated the same as if the resource was
listed in `depends_on`. This behavior can be avoided when desired by indirectly
referencing the managed resource values through a `local` value.

~> **NOTE:** **In Terraform 0.12 and earlier**, due to the data resource behavior of deferring the read until the apply phase when depending on values that are not yet known, using `depends_on` with `data` resources will force the read to always be deferred to the apply phase, and therefore a configuration that uses `depends_on` with a `data` resource can never converge. Due to this behavior, we do not recommend using `depends_on` with data resources.


## Multiple Resource Instances

Data resources support [`count`](/docs/language/meta-arguments/count.html)
and [`for_each`](/docs/language/meta-arguments/for_each.html)
meta-arguments as defined for managed resources, with the same syntax and behavior.

As with managed resources, when `count` or `for_each` is present it is important to
distinguish the resource itself from the multiple resource _instances_ it
creates. Each instance will separately read from its data source with its
own variant of the constraint arguments, producing an indexed result.

## Selecting a Non-default Provider Configuration

Data resources support [the `provider` meta-argument](/docs/language/meta-arguments/resource-provider.html)
as defined for managed resources, with the same syntax and behavior.

## Lifecycle Customizations

Data resources do not currently have any customization settings available
for their lifecycle, but the `lifecycle` nested block is reserved in case
any are added in future versions.

## Example

A data source configuration looks like the following:

```hcl
# Find the latest available AMI that is tagged with Component = web
data "aws_ami" "web" {
  filter {
    name   = "state"
    values = ["available"]
  }

  filter {
    name   = "tag:Component"
    values = ["web"]
  }

  most_recent = true
}
```

## Description

The `data` block creates a data instance of the given _type_ (first
block label) and _name_ (second block label). The combination of the type
and name must be unique.

Within the block (the `{ }`) is configuration for the data instance. The
configuration is dependent on the type; as with
[resources](/docs/language/resources/index.html), each provider on the
[Terraform Registry](https://registry.terraform.io/browse/providers) has its own
documentation for configuring and using the data types it provides.

Each data instance will export one or more attributes, which can be
used in other resources as reference expressions of the form
`data.<TYPE>.<NAME>.<ATTRIBUTE>`. For example:

```hcl
resource "aws_instance" "web" {
  ami           = data.aws_ami.web.id
  instance_type = "t1.micro"
}
```

## Meta-Arguments

As data sources are essentially a read only subset of resources, they also
support the same [meta-arguments](/docs/language/resources/syntax.html#meta-arguments) of resources
with the exception of the
[`lifecycle` configuration block](/docs/language/meta-arguments/lifecycle.html).

### Non-Default Provider Configurations

Similarly to [resources](/docs/language/resources/index.html), when
a module has multiple configurations for the same provider you can specify which
configuration to use with the `provider` meta-argument:

```hcl
data "aws_ami" "web" {
  provider = aws.west

  # ...
}
```

See
[The Resource `provider` Meta-Argument](/docs/language/meta-arguments/resource-provider.html)
for more information.

## Data Source Lifecycle

If the arguments of a data instance contain no references to computed values,
such as attributes of resources that have not yet been created, then the
data instance will be read and its state updated during Terraform's "refresh"
phase, which by default runs prior to creating a plan. This ensures that the
retrieved data is available for use during planning and the diff will show
the real values obtained.

Data instance arguments may refer to computed values, in which case the
attributes of the instance itself cannot be resolved until all of its
arguments are defined. In this case, refreshing the data instance will be
deferred until the "apply" phase, and all interpolations of the data instance
attributes will show as "computed" in the plan since the values are not yet
known.
