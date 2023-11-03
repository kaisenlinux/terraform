## 1.6.3 (November 1, 2023)

ENHANCEMENTS:
* backend/s3: Adds the parameter `skip_s3_checksum` to allow users to disable checksum on S3 uploads for compatibility with "S3-compatible" APIs. ([#34127](https://github.com/hashicorp/terraform/pull/34127))

## 1.6.2 (October 18, 2023)

BUG FIXES
* `terraform test`: Fix performance issues when using provisioners within configs being tested. ([#34026](https://github.com/hashicorp/terraform/pull/34026))
* `terraform test`: Only process and parse relevant variables for each run block. ([#34072](https://github.com/hashicorp/terraform/pull/34072))
* Fix occasional crash when destroying configurations with variables containing validations. ([#34101](https://github.com/hashicorp/terraform/pull/34101))
* Fix interoperability issues between v1.6 series and earlier series by removing variable validations from the state file ([#34058](https://github.com/hashicorp/terraform/pull/34058)).
* cloud: Fixes panic when saving state in Terraform Cloud when certain types of API errors are returned ([#34074](https://github.com/hashicorp/terraform/pull/34074)).
* config: Fix crash in conditional statements with certain combinations of unknown values. Improve handling of refined values into the conditional expression results ([#34096](https://github.com/hashicorp/terraform/issues/34096))
* config: Update HCL to fix bug when decoding objects with optional attributes ([#34108](https://github.com/hashicorp/terraform/issues/34108))
* backend/s3: Some configurations would require `-reconfigure` during each `init` when config was not decoded correctly ([#34108](https://github.com/hashicorp/terraform/issues/34108))

## 1.6.1 (October 10, 2023)

ENHANCEMENTS:
* backend/s3: The `skip_requesting_account_id` argument supports AWS API implementations that do not have the IAM, STS, or metadata API. ([#34002](https://github.com/hashicorp/terraform/pull/34002))

BUG FIXES:
* config: Using sensitive values as one or both of the results of a conditional expression will no longer crash. ([#33996](https://github.com/hashicorp/terraform/issues/33996))
* config: Conditional expression returning refined-non-null result will no longer crash. ([#33996](https://github.com/hashicorp/terraform/issues/33996))
* cli: Reverted back to previous behavior of ignoring signing key expiration for provider installation, since it's the provider registry's responsibility to verify key validity at publication time. ([#34004](https://github.com/hashicorp/terraform/issues/34004))
* cli: `GIT_SSH_COMMAND` is now preserved again when fetching modules from git source addresses. ([#34045](https://github.com/hashicorp/terraform/issues/34045))
* cloud: The `TF_WORKSPACE` environment variable works with the `cloud` block again; it can specify a workspace when none is configured, or select an active workspace when the config specifies `tags`. ([#34012](https://github.com/hashicorp/terraform/issues/34012))
* backend/s3: S3, DynamoDB, IAM, and STS endpoint parameters will no longer fail validation if the parsed scheme or hostname is empty. ([#34017](https://github.com/hashicorp/terraform/pull/34017))
* backend/s3: Providing a key alias to the `kms_key_id` argument will no longer fail validation. ([#33993](https://github.com/hashicorp/terraform/pull/33993))

## 1.6.0 (October 4, 2023)

UPGRADE NOTES:
* On macOS, Terraform now requires macOS 10.15 Catalina or later; support for previous versions has been discontinued.
* On Windows, Terraform now requires at least Windows 10 or Windows Server 2016; support for previous versions has been discontinued.
* The S3 backend has a number of significant changes to its configuration format in this release, intended to match with recent changes in the `hashicorp/aws` provider:
    * Configuration settings related to assuming IAM roles now belong to a nested block `assume_role`. The top-level arguments `role_arn`, `session_name`, `external_id`, `assume_role_duration_seconds`, `assume_role_policy_arns`, `assume_role_tags`, and `assume_role_transitive_tag_keys` are all now deprecated in favor of the nested equivalents. ([#30495](https://github.com/hashicorp/terraform/issues/30495))
    * Configuration settings related to overriding the locations of AWS service endpoints used by the provider now belong to a nested block `endpoints`. The top-level arguments `dynamodb_endpoint`, `iam_endpoint`, `endpoint` (fir S3), and `sts_endpoint` are now deprecated in favor of the nested equivalents. ([#30492](https://github.com/hashicorp/terraform/issues/30492))
    * The backend now uses the following environment variables for overriding the default locations of AWS service endpoints used by the provider: `AWS_ENDPOINT_URL_DYNAMODB`, `AWS_ENDPOINT_URL_IAM`, `AWS_ENDPOINT_URL_S3`, and `AWS_ENDPOINT_URL_STS`. The old non-standard names for these environment variables are now deprecated: `AWS_DYNAMODB_ENDPOINT`, `AWS_IAM_ENDPOINT`, `AWS_S3_ENDPOINT`, and `AWS_STS_ENDPOINT`. ([#30479](https://github.com/hashicorp/terraform/issues/30479))
    * The singular `shared_credentials_file` argument is deprecated in favor of the plural `shared_credentials_files`.
    * The `force_path_style` argument is deprecated in favor of `use_path_style` for consistency with the AWS SDK. ([#30491](https://github.com/hashicorp/terraform/issues/30491))

NEW FEATURES:
* `terraform test`: The `terraform test` command is now generally available. This comes with a significant change to how tests are written and executed, based on feedback from the experimental phase.

    Terraform tests are written in `.tftest.hcl` files, containing a series of `run` blocks. Each `run` block executes a Terraform plan and optional apply against the Terraform configuration under test and can check conditions against the resulting plan and state.

ENHANCEMENTS:
* config: The `import` block `id` field now accepts expressions referring to other values such as resource attributes, as long as the value is a string known at plan time. ([#33618](https://github.com/hashicorp/terraform/issues/33618))
* Terraform Cloud integration: Remote plans on Terraform Cloud/Enterprise can now be saved using the `-out` option, viewed using `terraform show`, and applied using `terraform apply` with the saved plan filename. ([#33492](https://github.com/hashicorp/terraform/issues/33492))
* config: Terraform can now track some additional detail about values that won't be known until the apply step, such as the range of possible lengths for a collection or whether an unknown value can possibly be null. When this information is available, Terraform can potentially generate known results for some operations on unknown values. This doesn't mean that Terraform can immediately track that detail in all cases, but the type system now supports that and so over time we can improve the level of detail generated by built-in functions, language operators, Terraform providers, etc. ([#33234](https://github.com/hashicorp/terraform/issues/33234))
* core: Provider schemas can now be cached globally for compatible providers, allowing them to be reused throughout core without requesting them for each new provider instance. This can significantly reduce memory usage when there are many instances of the same provider in a single configuration ([#33482](https://github.com/hashicorp/terraform/pull/33482))
* config: The `try` and `can` functions can now return more precise and consistent results when faced with unknown arguments ([#33758](https://github.com/hashicorp/terraform/pull/33758))
* `terraform show -json`: Now includes `errored` property, indicating whether the planning process halted with an error. An errored plan is not applyable. ([#33372](https://github.com/hashicorp/terraform/issues/33372))
* core: Terraform will now skip requesting the (possibly very large) provider schema from providers which indicate during handshake that they don't require that for correct behavior, in situations where Terraform Core itself does not need the schema. ([#33486](https://github.com/hashicorp/terraform/pull/33486))
* backend/kubernetes: The Kubernetes backend is no longer limited to storing states below 1MiB in size, and can now scale by splitting state across multiple secrets. ([#29678](https://github.com/hashicorp/terraform/pull/29678))
* backend/s3: Various improvements for consistency with `hashicorp/aws` provider capabilities:
    * `assume_role_with_web_identity` nested block for assuming a role with dynamic credentials such as a JSON Web Token. ([#31244](https://github.com/hashicorp/terraform/issues/31244))
    * Now honors the standard AWS environment variables for credential and configuration files: `AWS_CONFIG_FILE` and `AWS_SHARED_CREDENTIALS_FILE`. ([#30493](https://github.com/hashicorp/terraform/issues/30493))
    * `shared_config_files` and `shared_credentials_files` arguments for specifying credential and configuration files as part of the backend configuration. ([#30493](https://github.com/hashicorp/terraform/issues/30493))
    * Internally the backend now uses AWS SDK for Go v2, which should address various other missing behaviors that are handled by the SDK rather than by Terraform itself. ([#30443](https://github.com/hashicorp/terraform/issues/30443))
    * `custom_ca_bundle` argument and support for the corresponding AWS environment variable, `AWS_CA_BUNDLE`, for providing custom root and intermediate certificates. ([#33689](https://github.com/hashicorp/terraform/issues/33689))
    * `ec2_metadata_service_endpoint` and `ec2_metadata_service_endpoint_mode` arguments and support for the corresponding AWS environment variables, `AWS_EC2_METADATA_SERVICE_ENDPOINT` and `AWS_EC2_METADATA_SERVICE_ENDPOINT_MODE` for setting the EC2 metadata service (IMDS) endpoint. The environment variable `AWS_METADATA_URL` is also supported for compatibility with the AWS provider, but is deprecated. ([#30444](https://github.com/hashicorp/terraform/issues/30444))
    * `http_proxy`, `insecure`, `use_fips_endpoint`, and `use_dualstack_endpoint` arguments and support for the corresponding environment variables, `HTTP_PROXY` and `HTTPS_PROXY`, which enable custom HTTP proxy configurations and the resolution of AWS endpoints with extended capabilities. ([#30496](https://github.com/hashicorp/terraform/issues/30496))
    * `sts_region` argument to use an alternative region for STS operations. ([#33693](https://github.com/hashicorp/terraform/issues/33693))
    * `retry_mode` argument and support for the corresponding `AWS_RETRY_MODE` environment variable to configure how retries are attempted. ([#33692](https://github.com/hashicorp/terraform/issues/33692))
    * `allowed_account_ids` and `forbidden_account_ids` arguments to prevent unintended modifications to specified environments. ([#33688](https://github.com/hashicorp/terraform/issues/33688))
* backend/cos: Support custom HTTP(S) endpoint and root domain for the API client. ([#33656](https://github.com/hashicorp/terraform/issues/33656))

BUG FIXES:
* core: Transitive dependencies were lost during apply when the referenced resource expanded into zero instances. ([#33403](https://github.com/hashicorp/terraform/issues/33403))
* cli: Terraform will no longer override SSH settings in local git configuration when installing modules. ([#33592](https://github.com/hashicorp/terraform/issues/33592))
* `terraform` built-in provider: The upstream dependency that Terraform uses for service discovery of Terraform-native services such as Terraform Cloud/Enterprise state storage was previously not concurrency-safe, but Terraform was treating it as if it was in situations like when a configuration has multiple `terraform_remote_state` blocks all using the "remote" backend. Terraform is now using a newer version of that library which updates its internal caches in a concurrency-safe way. ([#33364](https://github.com/hashicorp/terraform/issues/33364))
* `terraform init`: Terraform will no longer allow downloading remote modules to invalid paths. ([#33745](https://github.com/hashicorp/terraform/issues/33745))
* Ignore potential remote terraform version mismatch when running force-unlock ([#28853](https://github.com/hashicorp/terraform/issues/28853))
* cloud: Fixed a bug that would prevent nested symlinks from being dereferenced into the config sent to Terraform Cloud ([#31895](https://github.com/hashicorp/terraform/issues/31895))
* cloud: state snapshots could not be disabled when header x-terraform-snapshot-interval is absent ([#33820](https://github.com/hashicorp/terraform/pull/33820))

## Previous Releases

For information on prior major and minor releases, see their changelogs:

* [v1.5](https://github.com/hashicorp/terraform/blob/v1.5/CHANGELOG.md)
* [v1.4](https://github.com/hashicorp/terraform/blob/v1.4/CHANGELOG.md)
* [v1.3](https://github.com/hashicorp/terraform/blob/v1.3/CHANGELOG.md)
* [v1.2](https://github.com/hashicorp/terraform/blob/v1.2/CHANGELOG.md)
* [v1.1](https://github.com/hashicorp/terraform/blob/v1.1/CHANGELOG.md)
* [v1.0](https://github.com/hashicorp/terraform/blob/v1.0/CHANGELOG.md)
* [v0.15](https://github.com/hashicorp/terraform/blob/v0.15/CHANGELOG.md)
* [v0.14](https://github.com/hashicorp/terraform/blob/v0.14/CHANGELOG.md)
* [v0.13](https://github.com/hashicorp/terraform/blob/v0.13/CHANGELOG.md)
* [v0.12](https://github.com/hashicorp/terraform/blob/v0.12/CHANGELOG.md)
* [v0.11 and earlier](https://github.com/hashicorp/terraform/blob/v0.11/CHANGELOG.md)
