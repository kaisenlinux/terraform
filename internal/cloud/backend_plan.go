package cloud

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	tfe "github.com/hashicorp/go-tfe"
	"github.com/hashicorp/terraform/internal/backend"
	"github.com/hashicorp/terraform/internal/plans"
	"github.com/hashicorp/terraform/internal/tfdiags"
)

var planConfigurationVersionsPollInterval = 500 * time.Millisecond

func (b *Cloud) opPlan(stopCtx, cancelCtx context.Context, op *backend.Operation, w *tfe.Workspace) (*tfe.Run, error) {
	log.Printf("[INFO] cloud: starting Plan operation")

	var diags tfdiags.Diagnostics

	if !w.Permissions.CanQueueRun {
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Insufficient rights to generate a plan",
			"The provided credentials have insufficient rights to generate a plan. In order "+
				"to generate plans, at least plan permissions on the workspace are required.",
		))
		return nil, diags.Err()
	}

	if b.ContextOpts != nil && b.ContextOpts.Parallelism != defaultParallelism {
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Custom parallelism values are currently not supported",
			`Terraform Cloud does not support setting a custom parallelism `+
				`value at this time.`,
		))
	}

	if op.PlanFile != nil {
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Displaying a saved plan is currently not supported",
			`Terraform Cloud currently requires configuration to be present and `+
				`does not accept an existing saved plan as an argument at this time.`,
		))
	}

	if op.PlanOutPath != "" {
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Saving a generated plan is currently not supported",
			`Terraform Cloud does not support saving the generated execution `+
				`plan locally at this time.`,
		))
	}

	if !op.HasConfig() && op.PlanMode != plans.DestroyMode {
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"No configuration files found",
			`Plan requires configuration to be present. Planning without a configuration `+
				`would mark everything for destruction, which is normally not what is desired. `+
				`If you would like to destroy everything, please run plan with the "-destroy" `+
				`flag or create a single empty configuration file. Otherwise, please create `+
				`a Terraform configuration file in the path being executed and try again.`,
		))
	}

	// Return if there are any errors.
	if diags.HasErrors() {
		return nil, diags.Err()
	}

	return b.plan(stopCtx, cancelCtx, op, w)
}

func (b *Cloud) plan(stopCtx, cancelCtx context.Context, op *backend.Operation, w *tfe.Workspace) (*tfe.Run, error) {
	if b.CLI != nil {
		header := planDefaultHeader
		if op.Type == backend.OperationTypeApply || op.Type == backend.OperationTypeRefresh {
			header = applyDefaultHeader
		}
		b.CLI.Output(b.Colorize().Color(strings.TrimSpace(header) + "\n"))
	}

	configOptions := tfe.ConfigurationVersionCreateOptions{
		AutoQueueRuns: tfe.Bool(false),
		Speculative:   tfe.Bool(op.Type == backend.OperationTypePlan),
	}

	cv, err := b.client.ConfigurationVersions.Create(stopCtx, w.ID, configOptions)
	if err != nil {
		return nil, generalError("Failed to create configuration version", err)
	}

	var configDir string
	if op.ConfigDir != "" {
		// De-normalize the configuration directory path.
		configDir, err = filepath.Abs(op.ConfigDir)
		if err != nil {
			return nil, generalError(
				"Failed to get absolute path of the configuration directory: %v", err)
		}

		// Make sure to take the working directory into account by removing
		// the working directory from the current path. This will result in
		// a path that points to the expected root of the workspace.
		configDir = filepath.Clean(strings.TrimSuffix(
			filepath.Clean(configDir),
			filepath.Clean(w.WorkingDirectory),
		))

		// If the workspace has a subdirectory as its working directory then
		// our configDir will be some parent directory of the current working
		// directory. Users are likely to find that surprising, so we'll
		// produce an explicit message about it to be transparent about what
		// we are doing and why.
		if w.WorkingDirectory != "" && filepath.Base(configDir) != w.WorkingDirectory {
			if b.CLI != nil {
				b.CLI.Output(fmt.Sprintf(strings.TrimSpace(`
The remote workspace is configured to work with configuration at
%s relative to the target repository.

Terraform will upload the contents of the following directory,
excluding files or directories as defined by a .terraformignore file
at %s/.terraformignore (if it is present),
in order to capture the filesystem context the remote workspace expects:
    %s
`), w.WorkingDirectory, configDir, configDir) + "\n")
			}
		}

	} else {
		// We did a check earlier to make sure we either have a config dir,
		// or the plan is run with -destroy. So this else clause will only
		// be executed when we are destroying and doesn't need the config.
		configDir, err = ioutil.TempDir("", "tf")
		if err != nil {
			return nil, generalError("Failed to create temporary directory", err)
		}
		defer os.RemoveAll(configDir)

		// Make sure the configured working directory exists.
		err = os.MkdirAll(filepath.Join(configDir, w.WorkingDirectory), 0700)
		if err != nil {
			return nil, generalError(
				"Failed to create temporary working directory", err)
		}
	}

	err = b.client.ConfigurationVersions.Upload(stopCtx, cv.UploadURL, configDir)
	if err != nil {
		return nil, generalError("Failed to upload configuration files", err)
	}

	uploaded := false
	for i := 0; i < 60 && !uploaded; i++ {
		select {
		case <-stopCtx.Done():
			return nil, context.Canceled
		case <-cancelCtx.Done():
			return nil, context.Canceled
		case <-time.After(planConfigurationVersionsPollInterval):
			cv, err = b.client.ConfigurationVersions.Read(stopCtx, cv.ID)
			if err != nil {
				return nil, generalError("Failed to retrieve configuration version", err)
			}

			if cv.Status == tfe.ConfigurationUploaded {
				uploaded = true
			}
		}
	}

	if !uploaded {
		return nil, generalError(
			"Failed to upload configuration files", errors.New("operation timed out"))
	}

	runOptions := tfe.RunCreateOptions{
		ConfigurationVersion: cv,
		Refresh:              tfe.Bool(op.PlanRefresh),
		Workspace:            w,
		AutoApply:            tfe.Bool(op.AutoApprove),
	}

	switch op.PlanMode {
	case plans.NormalMode:
		// okay, but we don't need to do anything special for this
	case plans.RefreshOnlyMode:
		runOptions.RefreshOnly = tfe.Bool(true)
	case plans.DestroyMode:
		runOptions.IsDestroy = tfe.Bool(true)
	default:
		// Shouldn't get here because we should update this for each new
		// plan mode we add, mapping it to the corresponding RunCreateOptions
		// field.
		return nil, generalError(
			"Invalid plan mode",
			fmt.Errorf("Terraform Cloud doesn't support %s", op.PlanMode),
		)
	}

	if len(op.Targets) != 0 {
		runOptions.TargetAddrs = make([]string, 0, len(op.Targets))
		for _, addr := range op.Targets {
			runOptions.TargetAddrs = append(runOptions.TargetAddrs, addr.String())
		}
	}

	if len(op.ForceReplace) != 0 {
		runOptions.ReplaceAddrs = make([]string, 0, len(op.ForceReplace))
		for _, addr := range op.ForceReplace {
			runOptions.ReplaceAddrs = append(runOptions.ReplaceAddrs, addr.String())
		}
	}

	config, _, configDiags := op.ConfigLoader.LoadConfigWithSnapshot(op.ConfigDir)
	if configDiags.HasErrors() {
		return nil, fmt.Errorf("error loading config with snapshot: %w", configDiags.Errs()[0])
	}
	variables, varDiags := ParseCloudRunVariables(op.Variables, config.Module.Variables)

	if varDiags.HasErrors() {
		return nil, varDiags.Err()
	}

	runVariables := make([]*tfe.RunVariable, len(variables))
	for name, value := range variables {
		runVariables = append(runVariables, &tfe.RunVariable{
			Key:   name,
			Value: value,
		})
	}
	runOptions.Variables = runVariables

	r, err := b.client.Runs.Create(stopCtx, runOptions)
	if err != nil {
		return r, generalError("Failed to create run", err)
	}

	// When the lock timeout is set, if the run is still pending and
	// cancellable after that period, we attempt to cancel it.
	if lockTimeout := op.StateLocker.Timeout(); lockTimeout > 0 {
		go func() {
			select {
			case <-stopCtx.Done():
				return
			case <-cancelCtx.Done():
				return
			case <-time.After(lockTimeout):
				// Retrieve the run to get its current status.
				r, err := b.client.Runs.Read(cancelCtx, r.ID)
				if err != nil {
					log.Printf("[ERROR] error reading run: %v", err)
					return
				}

				if r.Status == tfe.RunPending && r.Actions.IsCancelable {
					if b.CLI != nil {
						b.CLI.Output(b.Colorize().Color(strings.TrimSpace(lockTimeoutErr)))
					}

					// We abuse the auto aprove flag to indicate that we do not
					// want to ask if the remote operation should be canceled.
					op.AutoApprove = true

					p, err := os.FindProcess(os.Getpid())
					if err != nil {
						log.Printf("[ERROR] error searching process ID: %v", err)
						return
					}
					p.Signal(syscall.SIGINT)
				}
			}
		}()
	}

	if b.CLI != nil {
		b.CLI.Output(b.Colorize().Color(strings.TrimSpace(fmt.Sprintf(
			runHeader, b.hostname, b.organization, op.Workspace, r.ID)) + "\n"))
	}

	// Retrieve the run to get task stages.
	// Task Stages are calculated upfront so we only need to call this once for the run.
	taskStages := make([]*tfe.TaskStage, 0)
	result, err := b.client.Runs.ReadWithOptions(stopCtx, r.ID, &tfe.RunReadOptions{
		Include: []tfe.RunIncludeOpt{tfe.RunTaskStages},
	})
	if err == nil {
		taskStages = result.TaskStages
	} else {
		// This error would be expected for older versions of TFE that do not allow
		// fetching task_stages.
		if !strings.HasSuffix(err.Error(), "Invalid include parameter") {
			return r, generalError("Failed to retrieve run", err)
		}
	}

	if stageID := getTaskStageIDByName(taskStages, tfe.PrePlan); stageID != nil {
		if err := b.waitTaskStage(stopCtx, cancelCtx, op, r, *stageID, "Pre-plan Tasks"); err != nil {
			return r, err
		}
	}

	r, err = b.waitForRun(stopCtx, cancelCtx, op, "plan", r, w)
	if err != nil {
		return r, err
	}

	logs, err := b.client.Plans.Logs(stopCtx, r.Plan.ID)
	if err != nil {
		return r, generalError("Failed to retrieve logs", err)
	}
	reader := bufio.NewReaderSize(logs, 64*1024)

	if b.CLI != nil {
		for next := true; next; {
			var l, line []byte

			for isPrefix := true; isPrefix; {
				l, isPrefix, err = reader.ReadLine()
				if err != nil {
					if err != io.EOF {
						return r, generalError("Failed to read logs", err)
					}
					next = false
				}
				line = append(line, l...)
			}

			if next || len(line) > 0 {
				b.CLI.Output(b.Colorize().Color(string(line)))
			}
		}
	}

	// Retrieve the run to get its current status.
	r, err = b.client.Runs.Read(stopCtx, r.ID)
	if err != nil {
		return r, generalError("Failed to retrieve run", err)
	}

	// If the run is canceled or errored, we still continue to the
	// cost-estimation and policy check phases to ensure we render any
	// results available. In the case of a hard-failed policy check, the
	// status of the run will be "errored", but there is still policy
	// information which should be shown.

	if stageID := getTaskStageIDByName(taskStages, tfe.PostPlan); stageID != nil {
		if err := b.waitTaskStage(stopCtx, cancelCtx, op, r, *stageID, "Post-plan Tasks"); err != nil {
			return r, err
		}
	}

	// Show any cost estimation output.
	if r.CostEstimate != nil {
		err = b.costEstimate(stopCtx, cancelCtx, op, r)
		if err != nil {
			return r, err
		}
	}

	// Check any configured sentinel policies.
	if len(r.PolicyChecks) > 0 {
		err = b.checkPolicy(stopCtx, cancelCtx, op, r)
		if err != nil {
			return r, err
		}
	}

	return r, nil
}

func getTaskStageIDByName(stages []*tfe.TaskStage, stageName tfe.Stage) *string {
	if len(stages) == 0 {
		return nil
	}

	for _, stage := range stages {
		if stage.Stage == stageName {
			return &stage.ID
		}
	}
	return nil
}

const planDefaultHeader = `
[reset][yellow]Running plan in Terraform Cloud. Output will stream here. Pressing Ctrl-C
will stop streaming the logs, but will not stop the plan running remotely.[reset]

Preparing the remote plan...
`

const runHeader = `
[reset][yellow]To view this run in a browser, visit:
https://%s/app/%s/%s/runs/%s[reset]
`

// The newline in this error is to make it look good in the CLI!
const lockTimeoutErr = `
[reset][red]Lock timeout exceeded, sending interrupt to cancel the remote operation.
[reset]
`
