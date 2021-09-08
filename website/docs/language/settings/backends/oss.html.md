---
layout: "language"
page_title: "Backend Type: oss"
sidebar_current: "docs-backends-types-standard-oss"
description: |-
  Terraform can store state remotely in OSS and lock that state with OSS.
---

# OSS

**Kind: Standard (with locking via TableStore)**

Stores the state as a given key in a given bucket on Stores
[Alibaba Cloud OSS](https://www.alibabacloud.com/help/product/31815.htm).
This backend also supports state locking and consistency checking via
[Alibaba Cloud Table Store](https://www.alibabacloud.com/help/doc-detail/27280.htm), which can be enabled by setting
the `tablestore_table` field to an existing TableStore table name.

-> **Note:** The OSS backend is available from terraform version 0.12.2.

## Example Configuration

```hcl
terraform {
  backend "oss" {
    bucket = "bucket-for-terraform-state"
    prefix   = "path/mystate"
    key   = "version-1.tfstate"
    region = "cn-beijing"
    tablestore_endpoint = "https://terraform-remote.cn-hangzhou.ots.aliyuncs.com"
    tablestore_table = "statelock"
  }
}
```

This assumes we have a [OSS Bucket](https://registry.terraform.io/providers/aliyun/alicloud/latest/docs/resources/oss_bucket) created called `bucket-for-terraform-state`,
a [OTS Instance](https://registry.terraform.io/providers/aliyun/alicloud/latest/docs/resources/ots_instance) called `terraform-remote` and
a [OTS TableStore](https://registry.terraform.io/providers/aliyun/alicloud/latest/docs/resources/ots_table) called `statelock`. The
Terraform state will be written into the file `path/mystate/version-1.tfstate`. The `TableStore` must have a primary key named `LockID` of type `String`.


## Data Source Configuration

To make use of the OSS remote state in another configuration, use the
[`terraform_remote_state` data
source](/docs/language/state/remote-state-data.html).

```hcl
terraform {
  backend "oss" {
    bucket = "remote-state-dns"
    prefix = "mystate/state"
    key    = "terraform.tfstate"
    region = "cn-beijing"
  }
}
```

The `terraform_remote_state` data source will return all of the root outputs
defined in the referenced remote state, an example output might look like:

```
data "terraform_remote_state" "network" {
    backend   = "oss"
    config    = {
        bucket = "remote-state-dns"
        key    = "terraform.tfstate"
        prefix = "mystate/state"
        region = "cn-beijing"
    }
    outputs   = {}
    workspace = "default"
}
```

## Configuration variables

The following configuration options or environment variables are supported:

 * `access_key` - (Optional) Alibaba Cloud access key. It supports environment variables `ALICLOUD_ACCESS_KEY` and  `ALICLOUD_ACCESS_KEY_ID`.
 * `secret_key` - (Optional) Alibaba Cloud secret access key. It supports environment variables `ALICLOUD_SECRET_KEY` and  `ALICLOUD_ACCESS_KEY_SECRET`.
 * `security_token` - (Optional) STS access token. It supports environment variable `ALICLOUD_SECURITY_TOKEN`.
 * `ecs_role_name` - (Optional, Available in 0.12.14+) The RAM Role Name attached on a ECS instance for API operations. You can retrieve this from the 'Access Control' section of the Alibaba Cloud console.
 * `region` - (Optional) The region of the OSS bucket. It supports environment variables `ALICLOUD_REGION` and `ALICLOUD_DEFAULT_REGION`.
 * `endpoint` - (Optional) A custom endpoint for the OSS API. It supports environment variables `ALICLOUD_OSS_ENDPOINT` and `OSS_ENDPOINT`.
 * `bucket` - (Required) The name of the OSS bucket.
 * `prefix` - (Opeional) The path directory of the state file will be stored. Default to "env:".
 * `key` - (Optional) The name of the state file. Defaults to `terraform.tfstate`.
 * `tablestore_endpoint` / `ALICLOUD_TABLESTORE_ENDPOINT` - (Optional) A custom endpoint for the TableStore API.
 * `tablestore_table` - (Optional) A TableStore table for state locking and consistency. The table must have a primary key named `LockID` of type `String`.
 * `encrypt` - (Optional) Whether to enable server side
   encryption of the state file. If it is true, OSS will use 'AES256' encryption algorithm to encrypt state file.
 * `acl` - (Optional) [Object
   ACL](https://www.alibabacloud.com/help/doc-detail/52284.htm)
   to be applied to the state file.
 * `shared_credentials_file` - (Optional, Available in 0.12.8+) This is the path to the shared credentials file. It can also be sourced from the `ALICLOUD_SHARED_CREDENTIALS_FILE` environment variable. If this is not set and a profile is specified, `~/.aliyun/config.json` will be used.
 * `profile` - (Optional, Available in 0.12.8+)  This is the Alibaba Cloud profile name as set in the shared credentials file. It can also be sourced from the `ALICLOUD_PROFILE` environment variable.
 * `assume_role` - (Optional, Available in 0.12.6+) If provided with a role ARN, will attempt to assume this role using the supplied credentials.

The nested `assume_role` block supports the following:

* `role_arn` - (Required) The ARN of the role to assume. If ARN is set to an empty string, it does not perform role switching. It supports environment variable `ALICLOUD_ASSUME_ROLE_ARN`.
  Terraform executes configuration on account with provided credentials.

* `policy` - (Optional) A more restrictive policy to apply to the temporary credentials. This gives you a way to further restrict the permissions for the resulting temporary
  security credentials. You cannot use this policy to grant permissions which exceed those of the role that is being assumed.

* `session_name` - (Optional) The session name to use when assuming the role. If omitted, 'terraform' is passed to the AssumeRole call as session name. It supports environment variable `ALICLOUD_ASSUME_ROLE_SESSION_NAME`.

* `session_expiration` - (Optional) The time after which the established session for assuming role expires. Valid value range: [900-3600] seconds. Default to 3600 (in this case Alibaba Cloud use own default value). It supports environment variable `ALICLOUD_ASSUME_ROLE_SESSION_EXPIRATION`.

-> **Note:** If you want to store state in the custom OSS endpoint, you can specify a environment variable `OSS_ENDPOINT`, like "oss-cn-beijing-internal.aliyuncs.com"
