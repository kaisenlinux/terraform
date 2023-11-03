// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package cloud

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"

	tfe "github.com/hashicorp/go-tfe"
	version "github.com/hashicorp/go-version"
	svchost "github.com/hashicorp/terraform-svchost"
	"github.com/hashicorp/terraform-svchost/disco"
	"github.com/mitchellh/cli"
	"github.com/mitchellh/colorstring"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/gocty"

	"github.com/hashicorp/terraform/internal/backend"
	"github.com/hashicorp/terraform/internal/command/jsonformat"
	"github.com/hashicorp/terraform/internal/command/views"
	"github.com/hashicorp/terraform/internal/configs/configschema"
	"github.com/hashicorp/terraform/internal/plans"
	"github.com/hashicorp/terraform/internal/states/statemgr"
	"github.com/hashicorp/terraform/internal/terraform"
	"github.com/hashicorp/terraform/internal/tfdiags"
	tfversion "github.com/hashicorp/terraform/version"

	backendLocal "github.com/hashicorp/terraform/internal/backend/local"
)

const (
	defaultHostname    = "app.terraform.io"
	defaultParallelism = 10
	tfeServiceID       = "tfe.v2"
	headerSourceKey    = "X-Terraform-Integration"
	headerSourceValue  = "cloud"
	genericHostname    = "localterraform.com"
)

// Cloud is an implementation of EnhancedBackend in service of the Terraform Cloud/Enterprise
// integration for Terraform CLI. This backend is not intended to be surfaced at the user level and
// is instead an implementation detail of cloud.Cloud.
type Cloud struct {
	// CLI and Colorize control the CLI output. If CLI is nil then no CLI
	// output will be done. If CLIColor is nil then no coloring will be done.
	CLI      cli.Ui
	CLIColor *colorstring.Colorize

	// ContextOpts are the base context options to set when initializing a
	// new Terraform context. Many of these will be overridden or merged by
	// Operation. See Operation for more details.
	ContextOpts *terraform.ContextOpts

	// client is the Terraform Cloud/Enterprise API client.
	client *tfe.Client

	// viewHooks implements functions integrating the tfe.Client with the CLI
	// output.
	viewHooks views.CloudHooks

	// Hostname of Terraform Cloud or Terraform Enterprise
	Hostname string

	// token for Terraform Cloud or Terraform Enterprise
	token string

	// organization is the organization that contains the target workspaces.
	organization string

	// WorkspaceMapping contains strategies for mapping CLI workspaces in the working directory
	// to remote Terraform Cloud workspaces.
	WorkspaceMapping WorkspaceMapping

	// services is used for service discovery
	services *disco.Disco

	// renderer is used for rendering JSON plan output and streamed logs.
	renderer *jsonformat.Renderer

	// local allows local operations, where Terraform Cloud serves as a state storage backend.
	local backend.Enhanced

	// forceLocal, if true, will force the use of the local backend.
	forceLocal bool

	// opLock locks operations
	opLock sync.Mutex

	// ignoreVersionConflict, if true, will disable the requirement that the
	// local Terraform version matches the remote workspace's configured
	// version. This will also cause VerifyWorkspaceTerraformVersion to return
	// a warning diagnostic instead of an error.
	ignoreVersionConflict bool

	runningInAutomation bool

	// input stores the value of the -input flag, since it will be used
	// to determine whether or not to ask the user for approval of a run.
	input bool
}

var _ backend.Backend = (*Cloud)(nil)
var _ backend.Enhanced = (*Cloud)(nil)
var _ backend.Local = (*Cloud)(nil)

// New creates a new initialized cloud backend.
func New(services *disco.Disco) *Cloud {
	return &Cloud{
		services: services,
	}
}

// ConfigSchema implements backend.Enhanced.
func (b *Cloud) ConfigSchema() *configschema.Block {
	return &configschema.Block{
		Attributes: map[string]*configschema.Attribute{
			"hostname": {
				Type:        cty.String,
				Optional:    true,
				Description: schemaDescriptionHostname,
			},
			"organization": {
				Type:        cty.String,
				Optional:    true,
				Description: schemaDescriptionOrganization,
			},
			"token": {
				Type:        cty.String,
				Optional:    true,
				Description: schemaDescriptionToken,
			},
		},

		BlockTypes: map[string]*configschema.NestedBlock{
			"workspaces": {
				Block: configschema.Block{
					Attributes: map[string]*configschema.Attribute{
						"name": {
							Type:        cty.String,
							Optional:    true,
							Description: schemaDescriptionName,
						},
						"project": {
							Type:        cty.String,
							Optional:    true,
							Description: schemaDescriptionProject,
						},
						"tags": {
							Type:        cty.Set(cty.String),
							Optional:    true,
							Description: schemaDescriptionTags,
						},
					},
				},
				Nesting: configschema.NestingSingle,
			},
		},
	}
}

// PrepareConfig implements backend.Backend. Per the interface contract, it
// should catch invalid contents in the config value and populate knowable
// default values, but must NOT consult environment variables or other knowledge
// outside the config value itself.
func (b *Cloud) PrepareConfig(obj cty.Value) (cty.Value, tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics
	if obj.IsNull() {
		return obj, diags
	}

	// Since this backend uses environment variables extensively, this function
	// can't do very much! We do our main validity checks in resolveCloudConfig,
	// which is allowed to resolve fallback values from the environment. About
	// the only thing we can check for here is whether the conflicting `name`
	// and `tags` attributes are both set.
	if workspaces := obj.GetAttr("workspaces"); !workspaces.IsNull() {
		if val := workspaces.GetAttr("name"); !val.IsNull() {
			if val := workspaces.GetAttr("tags"); !val.IsNull() {
				diags = diags.Append(invalidWorkspaceConfigMisconfiguration)
			}
		}
	}

	return obj, diags
}

func (b *Cloud) ServiceDiscoveryAliases() ([]backend.HostAlias, error) {
	aliasHostname, err := svchost.ForComparison(genericHostname)
	if err != nil {
		// This should never happen because the hostname is statically defined.
		return nil, fmt.Errorf("failed to create backend alias from alias %q. The hostname is not in the correct format. This is a bug in the backend", genericHostname)
	}

	targetHostname, err := svchost.ForComparison(b.Hostname)
	if err != nil {
		// This should never happen because the 'to' alias is the backend host, which has
		// already been ev
		return nil, fmt.Errorf("failed to create backend alias to target %q. The hostname is not in the correct format.", b.Hostname)
	}

	return []backend.HostAlias{
		{
			From: aliasHostname,
			To:   targetHostname,
		},
	}, nil
}

// Configure implements backend.Enhanced.
func (b *Cloud) Configure(obj cty.Value) tfdiags.Diagnostics {
	var diags tfdiags.Diagnostics
	if obj.IsNull() {
		return diags
	}

	// Combine environment variables and the cloud block to get the full config.
	// We are now completely done with `obj`!
	config, configDiags := resolveCloudConfig(obj)
	diags = diags.Append(configDiags)
	if diags.HasErrors() {
		return diags
	}

	// Use resolved config to set fields on backend (except token, see below)
	b.Hostname = config.hostname
	b.organization = config.organization
	b.WorkspaceMapping = config.workspaceMapping

	// Discover the service URL to confirm that it provides the Terraform Cloud/Enterprise API
	hostname, err := svchost.ForComparison(b.Hostname)

	var service *url.URL
	if err == nil {
		// We want to handle the errors returned by svchost and discover in the
		// same way. So we only execute this if there wasn't an error previously
		// and let the block below handle an error that occurred in either
		// place.
		service, err = discover(hostname, b.services)
	}

	// Check for errors before we continue.
	if err != nil {
		diags = diags.Append(tfdiags.AttributeValue(
			tfdiags.Error,
			strings.ToUpper(err.Error()[:1])+err.Error()[1:],
			"", // no description is needed here, the error is clear
			cty.Path{cty.GetAttrStep{Name: "hostname"}},
		))
		return diags
	}

	// Token time. First, see if the configuration had one:
	token := config.token

	// Get the token from the CLI Config File in the credentials section
	// if no token was set in the configuration
	if token == "" {
		token, err = cliConfigToken(hostname, b.services)
		if err != nil {
			diags = diags.Append(tfdiags.AttributeValue(
				tfdiags.Error,
				strings.ToUpper(err.Error()[:1])+err.Error()[1:],
				"", // no description is needed here, the error is clear
				cty.Path{cty.GetAttrStep{Name: "hostname"}},
			))
			return diags
		}
	}

	// Return an error if we still don't have a token at this point.
	if token == "" {
		loginCommand := "terraform login"
		if b.Hostname != defaultHostname {
			loginCommand = loginCommand + " " + b.Hostname
		}
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Required token could not be found",
			fmt.Sprintf(
				"Run the following command to generate a token for %s:\n    %s",
				b.Hostname,
				loginCommand,
			),
		))
		return diags
	}

	b.token = token

	if b.client == nil {
		cfg := &tfe.Config{
			Address:      service.String(),
			BasePath:     service.Path,
			Token:        token,
			Headers:      make(http.Header),
			RetryLogHook: b.retryLogHook,
		}

		// Set the version header to the current version.
		cfg.Headers.Set(tfversion.Header, tfversion.Version)
		cfg.Headers.Set(headerSourceKey, headerSourceValue)

		// Create the TFC/E API client.
		b.client, err = tfe.NewClient(cfg)
		if err != nil {
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Failed to create the Terraform Cloud/Enterprise client",
				fmt.Sprintf(
					`Encountered an unexpected error while creating the `+
						`Terraform Cloud/Enterprise client: %s.`, err,
				),
			))
			return diags
		}
	}

	// Check if the organization exists by reading its entitlements.
	entitlements, err := b.client.Organizations.ReadEntitlements(context.Background(), b.organization)
	if err != nil {
		if err == tfe.ErrResourceNotFound {
			err = fmt.Errorf("organization %q at host %s not found.\n\n"+
				"Please ensure that the organization and hostname are correct "+
				"and that your API token for %s is valid.",
				b.organization, b.Hostname, b.Hostname)
		}
		diags = diags.Append(tfdiags.AttributeValue(
			tfdiags.Error,
			fmt.Sprintf("Failed to read organization %q at host %s", b.organization, b.Hostname),
			fmt.Sprintf("Encountered an unexpected error while reading the "+
				"organization settings: %s", err),
			cty.Path{cty.GetAttrStep{Name: "organization"}},
		))
		return diags
	}

	// If TF_WORKSPACE specifies a current workspace to use, make sure it's usable.
	if ws, ok := os.LookupEnv("TF_WORKSPACE"); ok {
		if ws == b.WorkspaceMapping.Name || b.WorkspaceMapping.Strategy() == WorkspaceTagsStrategy {
			diag := b.validWorkspaceEnvVar(context.Background(), b.organization, ws)
			if diag != nil {
				diags = diags.Append(diag)
				return diags
			}
		}
	}

	// Check for the minimum version of Terraform Enterprise required.
	//
	// For API versions prior to 2.3, RemoteAPIVersion will return an empty string,
	// so if there's an error when parsing the RemoteAPIVersion, it's handled as
	// equivalent to an API version < 2.3.
	currentAPIVersion, parseErr := version.NewVersion(b.client.RemoteAPIVersion())
	desiredAPIVersion, _ := version.NewVersion("2.5")

	if parseErr != nil || currentAPIVersion.LessThan(desiredAPIVersion) {
		log.Printf("[TRACE] API version check failed; want: >= %s, got: %s", desiredAPIVersion.Original(), currentAPIVersion)
		if b.runningInAutomation {
			// It should never be possible for this Terraform process to be mistakenly
			// used internally within an unsupported Terraform Enterprise install - but
			// just in case it happens, give an actionable error.
			diags = diags.Append(
				tfdiags.Sourceless(
					tfdiags.Error,
					"Unsupported Terraform Enterprise version",
					cloudIntegrationUsedInUnsupportedTFE,
				),
			)
		} else {
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Unsupported Terraform Enterprise version",
				`The 'cloud' option is not supported with this version of Terraform Enterprise.`,
			),
			)
		}
	}

	// Configure a local backend for when we need to run operations locally.
	b.local = backendLocal.NewWithBackend(b)

	// Determine if we are forced to use the local backend.
	b.forceLocal = os.Getenv("TF_FORCE_LOCAL_BACKEND") != "" || !entitlements.Operations

	// Enable retries for server errors as the backend is now fully configured.
	b.client.RetryServerErrors(true)

	return diags
}

// resolveCloudConfig fills in a potentially incomplete cloud config block using
// environment variables and defaults. If the returned Diagnostics are clean,
// the resulting value is a logically valid cloud config. If the Diagnostics
// contain any errors, the resolved config value is invalid and should not be
// used. Note that this function does not verify that any objects referenced in
// the config actually exist in the remote system; it only validates that the
// resulting config is internally consistent.
func resolveCloudConfig(obj cty.Value) (cloudConfig, tfdiags.Diagnostics) {
	var ret cloudConfig
	var diags tfdiags.Diagnostics

	// Get the hostname. Config beats environment. Absent means use the default
	// hostname.
	if val := obj.GetAttr("hostname"); !val.IsNull() && val.AsString() != "" {
		ret.hostname = val.AsString()
		log.Printf("[TRACE] cloud: using hostname %q from cloud config block", ret.hostname)
	} else {
		ret.hostname = os.Getenv("TF_CLOUD_HOSTNAME")
		log.Printf("[TRACE] cloud: using hostname %q from TF_CLOUD_HOSTNAME variable", ret.hostname)
	}
	if ret.hostname == "" {
		ret.hostname = defaultHostname
		log.Printf("[TRACE] cloud: using default hostname %q", ret.hostname)
	}

	// Get the organization. Config beats environment. There's no default, so
	// absent means error.
	if val := obj.GetAttr("organization"); !val.IsNull() && val.AsString() != "" {
		ret.organization = val.AsString()
		log.Printf("[TRACE] cloud: using organization %q from cloud config block", ret.organization)
	} else {
		ret.organization = os.Getenv("TF_CLOUD_ORGANIZATION")
		log.Printf("[TRACE] cloud: using organization %q from TF_CLOUD_ORGANIZATION variable", ret.organization)
	}
	if ret.organization == "" {
		diags = diags.Append(missingConfigAttributeAndEnvVar("organization", "TF_CLOUD_ORGANIZATION"))
	}

	// Get the token. We only report what's in the config! An empty value is
	// ok; later, after this function is called, Configure() can try to resolve
	// per-hostname credentials from a variety of sources (including
	// hostname-specific env vars).
	if val := obj.GetAttr("token"); !val.IsNull() {
		ret.token = val.AsString()
		log.Printf("[TRACE] cloud: found token in cloud config block")
	}

	// Grab any workspace/project info from the nested config object in one go,
	// so it's easier to work with.
	var name, project string
	var tags []string
	if workspaces := obj.GetAttr("workspaces"); !workspaces.IsNull() {
		if val := workspaces.GetAttr("name"); !val.IsNull() {
			name = val.AsString()
			log.Printf("[TRACE] cloud: found workspace name %q in cloud config block", name)
		}
		if val := workspaces.GetAttr("tags"); !val.IsNull() {
			err := gocty.FromCtyValue(val, &tags)
			if err != nil {
				diags = diags.Append(fmt.Errorf("an unexpected error occurred: %w", err))
			}
			log.Printf("[TRACE] cloud: using tags %q from cloud config block", tags)
		}
		if val := workspaces.GetAttr("project"); !val.IsNull() {
			project = val.AsString()
			log.Printf("[TRACE] cloud: found project name %q in cloud config block", project)
		}
	}

	// Get the project. Config beats environment, and the default value is the
	// empty string.
	if project != "" {
		ret.workspaceMapping.Project = project
		log.Printf("[TRACE] cloud: using project %q from cloud config block", ret.workspaceMapping.Project)
	} else {
		ret.workspaceMapping.Project = os.Getenv("TF_CLOUD_PROJECT")
		log.Printf("[TRACE] cloud: using project %q from TF_CLOUD_PROJECT variable", ret.workspaceMapping.Project)
	}

	// Get the tags from the config. There's no environment variable.
	ret.workspaceMapping.Tags = tags

	// Get the name, and validate the WorkspaceMapping as a whole. This is the
	// only real tricky one, because TF_WORKSPACE is used in places beyond
	// the cloud backend config. The rules are:
	// - If the config had neither `name` nor `tags`, we fall back to TF_WORKSPACE as the name.
	// - If the config had `tags`, it's still legal to set TF_WORKSPACE, and it indicates
	//   which workspace should be *current,* but we leave Name blank in the mapping.
	//   This is mostly useful in CI.
	// - If the config had `name`, it's NOT LEGAL to set TF_WORKSPACE, but we make
	//   an exception if it's the same as the specified `name` because the intent was clear.

	// Start out with the name from the config (if any)
	ret.workspaceMapping.Name = name

	// Then examine the combination of name + tags:
	switch ret.workspaceMapping.Strategy() {
	// Invalid can't really happen here because b.PrepareConfig() already
	// checked for it. But, still:
	case WorkspaceInvalidStrategy:
		diags = diags.Append(invalidWorkspaceConfigMisconfiguration)
	// If both name and TF_WORKSPACE are set, error (unless they match)
	case WorkspaceNameStrategy:
		if tfws, ok := os.LookupEnv("TF_WORKSPACE"); ok && tfws != ret.workspaceMapping.Name {
			diags = diags.Append(invalidWorkspaceConfigNameConflict)
		} else {
			log.Printf("[TRACE] cloud: using workspace name %q from cloud config block", ret.workspaceMapping.Name)
		}
	// If config had nothing, use TF_WORKSPACE.
	case WorkspaceNoneStrategy:
		ret.workspaceMapping.Name = os.Getenv("TF_WORKSPACE")
		log.Printf("[TRACE] cloud: using workspace name %q from TF_WORKSPACE variable", ret.workspaceMapping.Name)
		// And, if config only had tags, do nothing.
	}

	// If our workspace mapping is still None after all that, then we don't have
	// a valid completed config!
	if ret.workspaceMapping.Strategy() == WorkspaceNoneStrategy {
		diags = diags.Append(invalidWorkspaceConfigMissingValues)
	}

	return ret, diags
}

// discover the TFC/E API service URL and version constraints.
func discover(hostname svchost.Hostname, services *disco.Disco) (*url.URL, error) {
	host, err := services.Discover(hostname)
	if err != nil {
		var serviceDiscoErr *disco.ErrServiceDiscoveryNetworkRequest

		switch {
		case errors.As(err, &serviceDiscoErr):
			err = fmt.Errorf("a network issue prevented cloud configuration; %w", err)
			return nil, err
		default:
			return nil, err
		}
	}

	service, err := host.ServiceURL(tfeServiceID)
	// Return the error, unless its a disco.ErrVersionNotSupported error.
	if _, ok := err.(*disco.ErrVersionNotSupported); !ok && err != nil {
		return nil, err
	}

	return service, err
}

// cliConfigToken returns the token for this host as configured in the credentials
// section of the CLI Config File. If no token was configured, an empty
// string will be returned instead.
func cliConfigToken(hostname svchost.Hostname, services *disco.Disco) (string, error) {
	creds, err := services.CredentialsForHost(hostname)
	if err != nil {
		log.Printf("[WARN] Failed to get credentials for %s: %s (ignoring)", hostname.ForDisplay(), err)
		return "", nil
	}
	if creds != nil {
		return creds.Token(), nil
	}
	return "", nil
}

// retryLogHook is invoked each time a request is retried allowing the
// backend to log any connection issues to prevent data loss.
func (b *Cloud) retryLogHook(attemptNum int, resp *http.Response) {
	if b.CLI != nil {
		if output := b.viewHooks.RetryLogHook(attemptNum, resp, true); len(output) > 0 {
			b.CLI.Output(b.Colorize().Color(output))
		}
	}
}

// Workspaces implements backend.Enhanced, returning a filtered list of workspace names according to
// the workspace mapping strategy configured.
func (b *Cloud) Workspaces() ([]string, error) {
	// Create a slice to contain all the names.
	var names []string

	// If configured for a single workspace, return that exact name only.  The StateMgr for this
	// backend will automatically create the remote workspace if it does not yet exist.
	if b.WorkspaceMapping.Strategy() == WorkspaceNameStrategy {
		names = append(names, b.WorkspaceMapping.Name)
		return names, nil
	}

	// Otherwise, multiple workspaces are being mapped. Query Terraform Cloud for all the remote
	// workspaces by the provided mapping strategy.
	options := &tfe.WorkspaceListOptions{}
	if b.WorkspaceMapping.Strategy() == WorkspaceTagsStrategy {
		taglist := strings.Join(b.WorkspaceMapping.Tags, ",")
		options.Tags = taglist
	}

	if b.WorkspaceMapping.Project != "" {
		listOpts := &tfe.ProjectListOptions{
			Name: b.WorkspaceMapping.Project,
		}
		projects, err := b.client.Projects.List(context.Background(), b.organization, listOpts)
		if err != nil && err != tfe.ErrResourceNotFound {
			return nil, fmt.Errorf("failed to retrieve project %s: %v", listOpts.Name, err)
		}
		for _, p := range projects.Items {
			if p.Name == b.WorkspaceMapping.Project {
				options.ProjectID = p.ID
				break
			}
		}
	}

	for {
		wl, err := b.client.Workspaces.List(context.Background(), b.organization, options)
		if err != nil {
			return nil, err
		}

		for _, w := range wl.Items {
			names = append(names, w.Name)
		}

		// Exit the loop when we've seen all pages.
		if wl.CurrentPage >= wl.TotalPages {
			break
		}

		// Update the page number to get the next page.
		options.PageNumber = wl.NextPage
	}

	// Sort the result so we have consistent output.
	sort.StringSlice(names).Sort()

	return names, nil
}

// DeleteWorkspace implements backend.Enhanced.
func (b *Cloud) DeleteWorkspace(name string, force bool) error {
	if name == backend.DefaultStateName {
		return backend.ErrDefaultWorkspaceNotSupported
	}

	if b.WorkspaceMapping.Strategy() == WorkspaceNameStrategy {
		return backend.ErrWorkspacesNotSupported
	}

	workspace, err := b.client.Workspaces.Read(context.Background(), b.organization, name)
	if err == tfe.ErrResourceNotFound {
		return nil // If the workspace does not exist, succeed
	}

	if err != nil {
		return fmt.Errorf("failed to retrieve workspace %s: %v", name, err)
	}

	// Configure the remote workspace name.
	State := &State{tfeClient: b.client, organization: b.organization, workspace: workspace, enableIntermediateSnapshots: false}
	return State.Delete(force)
}

// StateMgr implements backend.Enhanced.
func (b *Cloud) StateMgr(name string) (statemgr.Full, error) {
	var remoteTFVersion string

	if name == backend.DefaultStateName {
		return nil, backend.ErrDefaultWorkspaceNotSupported
	}

	if b.WorkspaceMapping.Strategy() == WorkspaceNameStrategy && name != b.WorkspaceMapping.Name {
		return nil, backend.ErrWorkspacesNotSupported
	}

	workspace, err := b.client.Workspaces.Read(context.Background(), b.organization, name)
	if err != nil && err != tfe.ErrResourceNotFound {
		return nil, fmt.Errorf("Failed to retrieve workspace %s: %v", name, err)
	}
	if workspace != nil {
		remoteTFVersion = workspace.TerraformVersion
	}

	var configuredProject *tfe.Project

	// Attempt to find project if configured
	if b.WorkspaceMapping.Project != "" {
		listOpts := &tfe.ProjectListOptions{
			Name: b.WorkspaceMapping.Project,
		}
		projects, err := b.client.Projects.List(context.Background(), b.organization, listOpts)
		if err != nil && err != tfe.ErrResourceNotFound {
			// This is a failure to make an API request, fail to initialize
			return nil, fmt.Errorf("Attempted to find configured project %s but was unable to.", b.WorkspaceMapping.Project)
		}
		for _, p := range projects.Items {
			if p.Name == b.WorkspaceMapping.Project {
				configuredProject = p
				break
			}
		}

		if configuredProject == nil {
			// We were able to read project, but were unable to find the configured project
			// This is not fatal as we may attempt to create the project if we need to create
			// the workspace
			log.Printf("[TRACE] cloud: Attempted to find configured project %s but was unable to.", b.WorkspaceMapping.Project)
		}
	}

	if err == tfe.ErrResourceNotFound {
		// Create workspace if it was not found

		// Workspace Create Options
		workspaceCreateOptions := tfe.WorkspaceCreateOptions{
			Name:    tfe.String(name),
			Tags:    b.WorkspaceMapping.tfeTags(),
			Project: configuredProject,
		}

		// Create project if not exists, otherwise use it
		if workspaceCreateOptions.Project == nil && b.WorkspaceMapping.Project != "" {
			// If we didn't find the project, try to create it
			if workspaceCreateOptions.Project == nil {
				createOpts := tfe.ProjectCreateOptions{
					Name: b.WorkspaceMapping.Project,
				}
				// didn't find project, create it instead
				log.Printf("[TRACE] cloud: Creating Terraform Cloud project %s/%s", b.organization, b.WorkspaceMapping.Project)
				project, err := b.client.Projects.Create(context.Background(), b.organization, createOpts)
				if err != nil && err != tfe.ErrResourceNotFound {
					return nil, fmt.Errorf("failed to create project %s: %v", b.WorkspaceMapping.Project, err)
				}
				configuredProject = project
				workspaceCreateOptions.Project = configuredProject
			}
		}

		// Create a workspace
		log.Printf("[TRACE] cloud: Creating Terraform Cloud workspace %s/%s", b.organization, name)
		workspace, err = b.client.Workspaces.Create(context.Background(), b.organization, workspaceCreateOptions)
		if err != nil {
			return nil, fmt.Errorf("error creating workspace %s: %v", name, err)
		}

		remoteTFVersion = workspace.TerraformVersion

		// Attempt to set the new workspace to use this version of Terraform. This
		// can fail if there's no enabled tool_version whose name matches our
		// version string, but that's expected sometimes -- just warn and continue.
		versionOptions := tfe.WorkspaceUpdateOptions{
			TerraformVersion: tfe.String(tfversion.String()),
		}
		_, err := b.client.Workspaces.UpdateByID(context.Background(), workspace.ID, versionOptions)
		if err == nil {
			remoteTFVersion = tfversion.String()
		} else {
			// TODO: Ideally we could rely on the client to tell us what the actual
			// problem was, but we currently can't get enough context from the error
			// object to do a nicely formatted message, so we're just assuming the
			// issue was that the version wasn't available since that's probably what
			// happened.
			log.Printf("[TRACE] cloud: Attempted to select version %s for TFC workspace; unavailable, so %s will be used instead.", tfversion.String(), workspace.TerraformVersion)
			if b.CLI != nil {
				versionUnavailable := fmt.Sprintf(unavailableTerraformVersion, tfversion.String(), workspace.TerraformVersion)
				b.CLI.Output(b.Colorize().Color(versionUnavailable))
			}
		}
	}

	if b.workspaceTagsRequireUpdate(workspace, b.WorkspaceMapping) {
		options := tfe.WorkspaceAddTagsOptions{
			Tags: b.WorkspaceMapping.tfeTags(),
		}
		log.Printf("[TRACE] cloud: Adding tags for Terraform Cloud workspace %s/%s", b.organization, name)
		err = b.client.Workspaces.AddTags(context.Background(), workspace.ID, options)
		if err != nil {
			return nil, fmt.Errorf("Error updating workspace %s: %v", name, err)
		}
	}

	// This is a fallback error check. Most code paths should use other
	// mechanisms to check the version, then set the ignoreVersionConflict
	// field to true. This check is only in place to ensure that we don't
	// accidentally upgrade state with a new code path, and the version check
	// logic is coarser and simpler.
	if !b.ignoreVersionConflict {
		// Explicitly ignore the pseudo-version "latest" here, as it will cause
		// plan and apply to always fail.
		if remoteTFVersion != tfversion.String() && remoteTFVersion != "latest" {
			return nil, fmt.Errorf("Remote workspace Terraform version %q does not match local Terraform version %q", remoteTFVersion, tfversion.String())
		}
	}

	return &State{tfeClient: b.client, organization: b.organization, workspace: workspace, enableIntermediateSnapshots: false}, nil
}

// Operation implements backend.Enhanced.
func (b *Cloud) Operation(ctx context.Context, op *backend.Operation) (*backend.RunningOperation, error) {
	// Retrieve the workspace for this operation.
	w, err := b.fetchWorkspace(ctx, b.organization, op.Workspace)
	if err != nil {
		return nil, err
	}

	// Terraform remote version conflicts are not a concern for operations. We
	// are in one of three states:
	//
	// - Running remotely, in which case the local version is irrelevant;
	// - Workspace configured for local operations, in which case the remote
	//   version is meaningless;
	// - Forcing local operations, which should only happen in the Terraform Cloud worker, in
	//   which case the Terraform versions by definition match.
	b.IgnoreVersionConflict()

	// Check if we need to use the local backend to run the operation.
	if b.forceLocal || isLocalExecutionMode(w.ExecutionMode) {
		// Record that we're forced to run operations locally to allow the
		// command package UI to operate correctly
		b.forceLocal = true
		return b.local.Operation(ctx, op)
	}

	// Set the remote workspace name.
	op.Workspace = w.Name

	// Determine the function to call for our operation
	var f func(context.Context, context.Context, *backend.Operation, *tfe.Workspace) (*tfe.Run, error)
	switch op.Type {
	case backend.OperationTypePlan:
		f = b.opPlan
	case backend.OperationTypeApply:
		f = b.opApply
	case backend.OperationTypeRefresh:
		// The `terraform refresh` command has been deprecated in favor of `terraform apply -refresh-state`.
		// Rather than respond with an error telling the user to run the other command we can just run
		// that command instead. We will tell the user what we are doing, and then do it.
		if b.CLI != nil {
			b.CLI.Output(b.Colorize().Color(strings.TrimSpace(refreshToApplyRefresh) + "\n"))
		}
		op.PlanMode = plans.RefreshOnlyMode
		op.PlanRefresh = true
		op.AutoApprove = true
		f = b.opApply
	default:
		return nil, fmt.Errorf(
			"\n\nTerraform Cloud does not support the %q operation.", op.Type)
	}

	// Lock
	b.opLock.Lock()

	// Build our running operation
	// the runninCtx is only used to block until the operation returns.
	runningCtx, done := context.WithCancel(context.Background())
	runningOp := &backend.RunningOperation{
		Context:   runningCtx,
		PlanEmpty: true,
	}

	// stopCtx wraps the context passed in, and is used to signal a graceful Stop.
	stopCtx, stop := context.WithCancel(ctx)
	runningOp.Stop = stop

	// cancelCtx is used to cancel the operation immediately, usually
	// indicating that the process is exiting.
	cancelCtx, cancel := context.WithCancel(context.Background())
	runningOp.Cancel = cancel

	// Do it.
	go func() {
		defer done()
		defer stop()
		defer cancel()

		defer b.opLock.Unlock()

		r, opErr := f(stopCtx, cancelCtx, op, w)
		if opErr != nil && opErr != context.Canceled {
			var diags tfdiags.Diagnostics
			diags = diags.Append(opErr)
			op.ReportResult(runningOp, diags)
			return
		}

		if r == nil && opErr == context.Canceled {
			runningOp.Result = backend.OperationFailure
			return
		}

		if r != nil {
			// Retrieve the run to get its current status.
			r, err := b.client.Runs.Read(cancelCtx, r.ID)
			if err != nil {
				var diags tfdiags.Diagnostics
				diags = diags.Append(generalError("Failed to retrieve run", err))
				op.ReportResult(runningOp, diags)
				return
			}

			// Record if there are any changes.
			runningOp.PlanEmpty = !r.HasChanges

			if opErr == context.Canceled {
				if err := b.cancel(cancelCtx, op, r); err != nil {
					var diags tfdiags.Diagnostics
					diags = diags.Append(generalError("Failed to retrieve run", err))
					op.ReportResult(runningOp, diags)
					return
				}
			}

			if r.Status == tfe.RunCanceled || r.Status == tfe.RunErrored {
				runningOp.Result = backend.OperationFailure
			}
		}
	}()

	// Return the running operation.
	return runningOp, nil
}

func (b *Cloud) cancel(cancelCtx context.Context, op *backend.Operation, r *tfe.Run) error {
	if r.Actions.IsCancelable {
		// Only ask if the remote operation should be canceled
		// if the auto approve flag is not set.
		if !op.AutoApprove {
			v, err := op.UIIn.Input(cancelCtx, &terraform.InputOpts{
				Id:          "cancel",
				Query:       "\nDo you want to cancel the remote operation?",
				Description: "Only 'yes' will be accepted to cancel.",
			})
			if err != nil {
				return generalError("Failed asking to cancel", err)
			}
			if v != "yes" {
				if b.CLI != nil {
					b.CLI.Output(b.Colorize().Color(strings.TrimSpace(operationNotCanceled)))
				}
				return nil
			}
		} else {
			if b.CLI != nil {
				// Insert a blank line to separate the ouputs.
				b.CLI.Output("")
			}
		}

		// Try to cancel the remote operation.
		err := b.client.Runs.Cancel(cancelCtx, r.ID, tfe.RunCancelOptions{})
		if err != nil {
			return generalError("Failed to cancel run", err)
		}
		if b.CLI != nil {
			b.CLI.Output(b.Colorize().Color(strings.TrimSpace(operationCanceled)))
		}
	}

	return nil
}

// IgnoreVersionConflict allows commands to disable the fall-back check that
// the local Terraform version matches the remote workspace's configured
// Terraform version. This should be called by commands where this check is
// unnecessary, such as those performing remote operations, or read-only
// operations. It will also be called if the user uses a command-line flag to
// override this check.
func (b *Cloud) IgnoreVersionConflict() {
	b.ignoreVersionConflict = true
}

// VerifyWorkspaceTerraformVersion compares the local Terraform version against
// the workspace's configured Terraform version. If they are compatible, this
// means that there are no state compatibility concerns, so it returns no
// diagnostics.
//
// If the versions aren't compatible, it returns an error (or, if
// b.ignoreVersionConflict is set, a warning).
func (b *Cloud) VerifyWorkspaceTerraformVersion(workspaceName string) tfdiags.Diagnostics {
	var diags tfdiags.Diagnostics

	workspace, err := b.getRemoteWorkspace(context.Background(), workspaceName)
	if err != nil {
		// If the workspace doesn't exist, there can be no compatibility
		// problem, so we can return. This is most likely to happen when
		// migrating state from a local backend to a new workspace.
		if err == tfe.ErrResourceNotFound {
			return nil
		}

		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Error looking up workspace",
			fmt.Sprintf("Workspace read failed: %s", err),
		))
		return diags
	}

	// If the workspace has the pseudo-version "latest", all bets are off. We
	// cannot reasonably determine what the intended Terraform version is, so
	// we'll skip version verification.
	if workspace.TerraformVersion == "latest" {
		return nil
	}

	// If the workspace has execution-mode set to local, the remote Terraform
	// version is effectively meaningless, so we'll skip version verification.
	if isLocalExecutionMode(workspace.ExecutionMode) {
		return nil
	}

	remoteConstraint, err := version.NewConstraint(workspace.TerraformVersion)
	if err != nil {
		message := fmt.Sprintf(
			"The remote workspace specified an invalid Terraform version or constraint (%s), "+
				"and it isn't possible to determine whether the local Terraform version (%s) is compatible.",
			workspace.TerraformVersion,
			tfversion.String(),
		)
		diags = diags.Append(incompatibleWorkspaceTerraformVersion(message, b.ignoreVersionConflict))
		return diags
	}

	remoteVersion, _ := version.NewSemver(workspace.TerraformVersion)

	// We can use a looser version constraint if the workspace specifies a
	// literal Terraform version, and it is not a prerelease. The latter
	// restriction is because we cannot compare prerelease versions with any
	// operator other than simple equality.
	if remoteVersion != nil && remoteVersion.Prerelease() == "" {
		v014 := version.Must(version.NewSemver("0.14.0"))
		v130 := version.Must(version.NewSemver("1.3.0"))

		// Versions from 0.14 through the early 1.x series should be compatible
		// (though we don't know about 1.3 yet).
		if remoteVersion.GreaterThanOrEqual(v014) && remoteVersion.LessThan(v130) {
			early1xCompatible, err := version.NewConstraint(fmt.Sprintf(">= 0.14.0, < %s", v130.String()))
			if err != nil {
				panic(err)
			}
			remoteConstraint = early1xCompatible
		}

		// Any future new state format will require at least a minor version
		// increment, so x.y.* will always be compatible with each other.
		if remoteVersion.GreaterThanOrEqual(v130) {
			rwvs := remoteVersion.Segments64()
			if len(rwvs) >= 3 {
				// ~> x.y.0
				minorVersionCompatible, err := version.NewConstraint(fmt.Sprintf("~> %d.%d.0", rwvs[0], rwvs[1]))
				if err != nil {
					panic(err)
				}
				remoteConstraint = minorVersionCompatible
			}
		}
	}

	// Re-parsing tfversion.String because tfversion.SemVer omits the prerelease
	// prefix, and we want to allow constraints like `~> 1.2.0-beta1`.
	fullTfversion := version.Must(version.NewSemver(tfversion.String()))

	if remoteConstraint.Check(fullTfversion) {
		return diags
	}

	message := fmt.Sprintf(
		"The local Terraform version (%s) does not meet the version requirements for remote workspace %s/%s (%s).",
		tfversion.String(),
		b.organization,
		workspace.Name,
		remoteConstraint,
	)
	diags = diags.Append(incompatibleWorkspaceTerraformVersion(message, b.ignoreVersionConflict))
	return diags
}

func (b *Cloud) IsLocalOperations() bool {
	return b.forceLocal
}

// Colorize returns the Colorize structure that can be used for colorizing
// output. This is guaranteed to always return a non-nil value and so useful
// as a helper to wrap any potentially colored strings.
//
// TODO SvH: Rename this back to Colorize as soon as we can pass -no-color.
//
//lint:ignore U1000 see above todo
func (b *Cloud) cliColorize() *colorstring.Colorize {
	if b.CLIColor != nil {
		return b.CLIColor
	}

	return &colorstring.Colorize{
		Colors:  colorstring.DefaultColors,
		Disable: true,
	}
}

func (b *Cloud) workspaceTagsRequireUpdate(workspace *tfe.Workspace, workspaceMapping WorkspaceMapping) bool {
	if workspaceMapping.Strategy() != WorkspaceTagsStrategy {
		return false
	}

	existingTags := map[string]struct{}{}
	for _, t := range workspace.TagNames {
		existingTags[t] = struct{}{}
	}

	for _, tag := range workspaceMapping.Tags {
		if _, ok := existingTags[tag]; !ok {
			return true
		}
	}

	return false
}

type WorkspaceMapping struct {
	Name    string
	Project string
	Tags    []string
}

type workspaceStrategy string

const (
	WorkspaceTagsStrategy    workspaceStrategy = "tags"
	WorkspaceNameStrategy    workspaceStrategy = "name"
	WorkspaceNoneStrategy    workspaceStrategy = "none"
	WorkspaceInvalidStrategy workspaceStrategy = "invalid"
)

func (wm WorkspaceMapping) Strategy() workspaceStrategy {
	switch {
	case len(wm.Tags) > 0 && wm.Name == "":
		return WorkspaceTagsStrategy
	case len(wm.Tags) == 0 && wm.Name != "":
		return WorkspaceNameStrategy
	case len(wm.Tags) == 0 && wm.Name == "":
		return WorkspaceNoneStrategy
	default:
		// Any other combination is invalid as each strategy is mutually exclusive
		return WorkspaceInvalidStrategy
	}
}

// cloudConfig is an intermediate type that represents the completed
// cloud block config as a plain Go value.
type cloudConfig struct {
	hostname         string
	organization     string
	token            string
	workspaceMapping WorkspaceMapping
}

func isLocalExecutionMode(execMode string) bool {
	return execMode == "local"
}

func (b *Cloud) fetchWorkspace(ctx context.Context, organization string, workspace string) (*tfe.Workspace, error) {
	// Retrieve the workspace for this operation.
	w, err := b.client.Workspaces.Read(ctx, organization, workspace)
	if err != nil {
		switch err {
		case context.Canceled:
			return nil, err
		case tfe.ErrResourceNotFound:
			return nil, fmt.Errorf(
				"workspace %s not found\n\n"+
					"For security, Terraform Cloud returns '404 Not Found' responses for resources\n"+
					"for resources that a user doesn't have access to, in addition to resources that\n"+
					"do not exist. If the resource does exist, please check the permissions of the provided token.",
				workspace,
			)
		default:
			err := fmt.Errorf(
				"Terraform Cloud returned an unexpected error:\n\n%s",
				err,
			)
			return nil, err
		}
	}

	return w, nil
}

// validWorkspaceEnvVar ensures we have selected a valid workspace using TF_WORKSPACE:
// First, it ensures the workspace specified by TF_WORKSPACE exists in the organization.
// (This is because we deliberately DON'T implicitly create a workspace from TF_WORKSPACE,
// unlike with a workspace specified via `name`.)
// Second, if tags are specified in the configuration, it ensures TF_WORKSPACE belongs to the set
// of available workspaces with those given tags.
func (b *Cloud) validWorkspaceEnvVar(ctx context.Context, organization, workspace string) tfdiags.Diagnostic {
	// first ensure the workspace exists
	_, err := b.client.Workspaces.Read(ctx, organization, workspace)
	if err != nil && err != tfe.ErrResourceNotFound {
		return tfdiags.Sourceless(
			tfdiags.Error,
			"Terraform Cloud returned an unexpected error",
			err.Error(),
		)
	}

	if err == tfe.ErrResourceNotFound {
		return tfdiags.Sourceless(
			tfdiags.Error,
			"Invalid workspace selection",
			fmt.Sprintf(`Terraform failed to find workspace %q in organization %s.`, workspace, organization),
		)
	}

	// if the configuration has specified tags, we need to ensure TF_WORKSPACE
	// is a valid member
	if b.WorkspaceMapping.Strategy() == WorkspaceTagsStrategy {
		opts := &tfe.WorkspaceListOptions{}
		opts.Tags = strings.Join(b.WorkspaceMapping.Tags, ",")

		for {
			wl, err := b.client.Workspaces.List(ctx, b.organization, opts)
			if err != nil {
				return tfdiags.Sourceless(
					tfdiags.Error,
					"Terraform Cloud returned an unexpected error",
					err.Error(),
				)
			}

			for _, ws := range wl.Items {
				if ws.Name == workspace {
					return nil
				}
			}

			if wl.CurrentPage >= wl.TotalPages {
				break
			}

			opts.PageNumber = wl.NextPage
		}

		return tfdiags.Sourceless(
			tfdiags.Error,
			"Invalid workspace selection",
			fmt.Sprintf(
				"Terraform failed to find workspace %q with the tags specified in your configuration:\n[%s]",
				workspace,
				strings.ReplaceAll(opts.Tags, ",", ", "),
			),
		)
	}

	return nil
}

func (wm WorkspaceMapping) tfeTags() []*tfe.Tag {
	var tags []*tfe.Tag

	if wm.Strategy() != WorkspaceTagsStrategy {
		return tags
	}

	for _, tag := range wm.Tags {
		t := tfe.Tag{Name: tag}
		tags = append(tags, &t)
	}

	return tags
}

func generalError(msg string, err error) error {
	var diags tfdiags.Diagnostics

	if urlErr, ok := err.(*url.Error); ok {
		err = urlErr.Err
	}

	switch err {
	case context.Canceled:
		return err
	case tfe.ErrResourceNotFound:
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			fmt.Sprintf("%s: %v", msg, err),
			"For security, Terraform Cloud returns '404 Not Found' responses for resources\n"+
				"for resources that a user doesn't have access to, in addition to resources that\n"+
				"do not exist. If the resource does exist, please check the permissions of the provided token.",
		))
		return diags.Err()
	default:
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			fmt.Sprintf("%s: %v", msg, err),
			`Terraform Cloud returned an unexpected error. Sometimes `+
				`this is caused by network connection problems, in which case you could retry `+
				`the command. If the issue persists please open a support ticket to get help `+
				`resolving the problem.`,
		))
		return diags.Err()
	}
}

const operationCanceled = `
[reset][red]The remote operation was successfully cancelled.[reset]
`

const operationNotCanceled = `
[reset][red]The remote operation was not cancelled.[reset]
`

const refreshToApplyRefresh = `[bold][yellow]Proceeding with 'terraform apply -refresh-only -auto-approve'.[reset]`

const unavailableTerraformVersion = `
[reset][yellow]The local Terraform version (%s) is not available in Terraform Cloud, or your
organization does not have access to it. The new workspace will use %s. You can
change this later in the workspace settings.[reset]`

const cloudIntegrationUsedInUnsupportedTFE = `
This version of Terraform Cloud/Enterprise does not support the state mechanism
attempting to be used by the platform. This should never happen.

Please reach out to HashiCorp Support to resolve this issue.`

var (
	workspaceConfigurationHelp = fmt.Sprintf(
		`The 'workspaces' block configures how Terraform CLI maps its workspaces for this single
configuration to workspaces within a Terraform Cloud organization. Two strategies are available:

[bold]tags[reset] - %s

[bold]name[reset] - %s`, schemaDescriptionTags, schemaDescriptionName)

	schemaDescriptionHostname = `The Terraform Enterprise hostname to connect to. This optional argument defaults to app.terraform.io
for use with Terraform Cloud.`

	schemaDescriptionOrganization = `The name of the organization containing the targeted workspace(s).`

	schemaDescriptionToken = `The token used to authenticate with Terraform Cloud/Enterprise. Typically this argument should not
be set, and 'terraform login' used instead; your credentials will then be fetched from your CLI
configuration file or configured credential helper.`

	schemaDescriptionTags = `A set of tags used to select remote Terraform Cloud workspaces to be used for this single
configuration. New workspaces will automatically be tagged with these tag values. Generally, this
is the primary and recommended strategy to use.  This option conflicts with "name".`

	schemaDescriptionName = `The name of a single Terraform Cloud workspace to be used with this configuration.
When configured, only the specified workspace can be used. This option conflicts with "tags"
and with the TF_WORKSPACE environment variable.`

	schemaDescriptionProject = `The name of a Terraform Cloud project. Workspaces that need creating
will be created within this project.`
)
