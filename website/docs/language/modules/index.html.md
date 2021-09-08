---
layout: "language"
page_title: "Modules Overview - Configuration Language"
---

# Modules

> **Hands-on:** Try the [Reuse Configuration with Modules](https://learn.hashicorp.com/collections/terraform/modules?utm_source=WEBSITE&utm_medium=WEB_IO&utm_offer=ARTICLE_PAGE&utm_content=DOCS) collection on HashiCorp Learn.

_Modules_ are containers for multiple resources that are used together. A module
consists of a collection of `.tf` and/or `.tf.json` files kept together in a
directory.

Modules are the main way to package and reuse resource configurations with
Terraform.

## The Root Module

Every Terraform configuration has at least one module, known as its
_root module_, which consists of the resources defined in the `.tf` files in
the main working directory.

## Child Modules

A Terraform module (usually the root module of a configuration) can _call_ other
modules to include their resources into the configuration. A module that has
been called by another module is often referred to as a _child module._

Child modules can be called multiple times within the same configuration, and
multiple configurations can use the same child module.

## Published Modules

In addition to modules from the local filesystem, Terraform can load modules
from a public or private registry. This makes it possible to publish modules for
others to use, and to use modules that others have published.

The [Terraform Registry](https://registry.terraform.io/browse/modules) hosts a
broad collection of publicly available Terraform modules for configuring many
kinds of common infrastructure. These modules are free to use, and Terraform can
download them automatically if you specify the appropriate source and version in
a module call block.

Also, members of your organization might produce modules specifically crafted
for your own infrastructure needs. [Terraform Cloud](/docs/cloud/index.html) and
[Terraform Enterprise](/docs/enterprise/index.html) both include a private
module registry for sharing modules internally within your organization.

## Using Modules

- [Module Blocks](/docs/language/modules/syntax.html) documents the syntax for
  calling a child module from a parent module, including meta-arguments like
  `for_each`.

- [Module Sources](/docs/language/modules/sources.html) documents what kinds of paths,
  addresses, and URIs can be used in the `source` argument of a module block.

- The Meta-Arguments section documents special arguments that can be used with
  every module, including
  [`providers`](/docs/language/meta-arguments/module-providers.html),
  [`depends_on`](/docs/language/meta-arguments/depends_on.html),
  [`count`](/docs/language/meta-arguments/count.html),
  and [`for_each`](/docs/language/meta-arguments/for_each.html).

## Developing Modules

For information about developing reusable modules, see
[Module Development](/docs/language/modules/develop/index.html).
