---
layout: "language"
page_title: "Provisioner Connection Settings"
sidebar_current: "docs-provisioners-connection"
description: "The connection block allows you to manage provisioner connection defaults for SSH and WinRM."
---

# Provisioner Connection Settings

Most provisioners require access to the remote resource via SSH or WinRM, and
expect a nested `connection` block with details about how to connect.

-> **Note:** Provisioners should only be used as a last resort. For most
common situations there are better alternatives. For more information, see
[the main Provisioners page](./).

-> **Note:** In Terraform 0.11 and earlier, providers could set default values
for some connection settings, so that `connection` blocks could sometimes be
omitted. This feature was removed in 0.12 in order to make Terraform's behavior
more predictable.

-> **Note:** Since the SSH connection type is most often used with
newly-created remote resources, validation of SSH host keys is disabled by
default. In scenarios where this is not acceptable, a separate mechanism for
key distribution could be established and the `host_key` directive documented
below explicitly set to verify against a specific key or signing CA.

Connection blocks don't take a block label, and can be nested within either a
`resource` or a `provisioner`.

- A `connection` block nested directly within a `resource` affects all of
  that resource's provisioners.
- A `connection` block nested in a `provisioner` block only affects that
  provisioner, and overrides any resource-level connection settings.

One use case for providing multiple connections is to have an initial
provisioner connect as the `root` user to set up user accounts, and have
subsequent provisioners connect as a user with more limited permissions.

## Example usage

```hcl
# Copies the file as the root user using SSH
provisioner "file" {
  source      = "conf/myapp.conf"
  destination = "/etc/myapp.conf"

  connection {
    type     = "ssh"
    user     = "root"
    password = "${var.root_password}"
    host     = "${var.host}"
  }
}

# Copies the file as the Administrator user using WinRM
provisioner "file" {
  source      = "conf/myapp.conf"
  destination = "C:/App/myapp.conf"

  connection {
    type     = "winrm"
    user     = "Administrator"
    password = "${var.admin_password}"
    host     = "${var.host}"
  }
}
```

## The `self` Object

Expressions in `connection` blocks cannot refer to their parent resource by
name. Instead, they can use the special `self` object.

The `self` object represents the connection's parent resource, and has all of
that resource's attributes. For example, use `self.public_ip` to reference an
`aws_instance`'s `public_ip` attribute.

-> **Technical note:** Resource references are restricted here because
references create dependencies. Referring to a resource by name within its own
block would create a dependency cycle.

## Argument Reference

**The following arguments are supported by all connection types:**

* `type` - The connection type that should be used. Valid types are `ssh` and `winrm`.
           Defaults to `ssh`.

* `user` - The user that we should use for the connection.
           Defaults to `root` when using type `ssh` and defaults to `Administrator` when using type `winrm`.

* `password` - The password we should use for the connection. In some cases this is
  specified by the provider.

* `host` - (Required) The address of the resource to connect to.

* `port` - The port to connect to.
           Defaults to `22` when using type `ssh` and defaults to `5985` when using type `winrm`.

* `timeout` - The timeout to wait for the connection to become available. Should be provided as a string like `30s` or `5m`.
              Defaults to 5 minutes.

* `script_path` - The path used to copy scripts meant for remote execution.

**Additional arguments only supported by the `ssh` connection type:**

* `private_key` - The contents of an SSH key to use for the connection. These can
  be loaded from a file on disk using
  [the `file` function](/docs/language/functions/file.html). This takes
  preference over the password if provided.

* `certificate` - The contents of a signed CA Certificate. The certificate argument must be
  used in conjunction with a `private_key`. These can
  be loaded from a file on disk using the [the `file` function](/docs/language/functions/file.html).

* `agent` - Set to `false` to disable using `ssh-agent` to authenticate. On Windows the
  only supported SSH authentication agent is
  [Pageant](http://the.earth.li/~sgtatham/putty/0.66/htmldoc/Chapter9.html#pageant).

* `agent_identity` - The preferred identity from the ssh agent for authentication.

* `host_key` - The public key from the remote host or the signing CA, used to
  verify the connection.
* `target_platform` - The target platform to connect to. Valid values are `windows` and `unix`. Defaults to `unix` if not set.

    If the platform is set to `windows`, the default `script_path` is `c:\windows\temp\terraform_%RAND%.cmd`, assuming [the SSH default shell](https://docs.microsoft.com/en-us/windows-server/administration/openssh/openssh_server_configuration#configuring-the-default-shell-for-openssh-in-windows) is `cmd.exe`. If the SSH default shell is PowerShell, set `script_path` to `"c:/windows/temp/terraform_%RAND%.ps1"`  

**Additional arguments only supported by the `winrm` connection type:**

* `https` - Set to `true` to connect using HTTPS instead of HTTP.

* `insecure` - Set to `true` to not validate the HTTPS certificate chain.

* `use_ntlm` - Set to `true` to use NTLM authentication, rather than default (basic authentication), removing the requirement for basic authentication to be enabled within the target guest. Further reading for remote connection authentication can be found [here](https://docs.microsoft.com/en-us/windows/win32/winrm/authentication-for-remote-connections?redirectedfrom=MSDN).

* `cacert` - The CA certificate to validate against.

<a id="bastion"></a>

## Connecting through a Bastion Host with SSH

The `ssh` connection also supports the following fields to facilitate connnections via a
[bastion host](https://en.wikipedia.org/wiki/Bastion_host).

* `bastion_host` - Setting this enables the bastion Host connection. This host
  will be connected to first, and then the `host` connection will be made from there.

* `bastion_host_key` - The public key from the remote host or the signing CA,
  used to verify the host connection.

* `bastion_port` - The port to use connect to the bastion host. Defaults to the
  value of the `port` field.

* `bastion_user` - The user for the connection to the bastion host. Defaults to
  the value of the `user` field.

* `bastion_password` - The password we should use for the bastion host.
  Defaults to the value of the `password` field.

* `bastion_private_key` - The contents of an SSH key file to use for the bastion
  host. These can be loaded from a file on disk using
  [the `file` function](/docs/language/functions/file.html).
  Defaults to the value of the `private_key` field.

* `bastion_certificate` - The contents of a signed CA Certificate. The certificate argument
  must be used in conjunction with a `bastion_private_key`. These can be loaded from
  a file on disk using the [the `file` function](/docs/language/functions/file.html).
