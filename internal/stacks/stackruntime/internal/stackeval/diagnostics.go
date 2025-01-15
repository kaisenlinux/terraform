// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package stackeval

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/hashicorp/terraform/internal/promising"
	"github.com/hashicorp/terraform/internal/tfdiags"
	"github.com/zclconf/go-cty/cty"
)

type withDiagnostics[T any] struct {
	Result      T
	Diagnostics tfdiags.Diagnostics
}

// doOnceWithDiags is a helper for the common pattern of evaluating a
// promising.Once that returns both a result and some diagnostics.
//
// It encapsulates all of the visual noise of sneaking the (result, diags)
// tuple through a struct type to compensate for the fact that Go generics
// cannot make a function generic over the number of results it returns.
//
// It also transforms any promise/task-related errors into user-oriented
// diagnostics, for which it needs to be provided a namedPromiseReporter "root"
// that covers the whole scope of possible promises that could be involved
// in the valuation. Typically root should be set to the relevant [Main]
// object to cover all of the promises across the whole evaluation.
func doOnceWithDiags[T any](
	ctx context.Context,
	once *promising.Once[withDiagnostics[T]],
	root namedPromiseReporter,
	f func(ctx context.Context) (T, tfdiags.Diagnostics),
) (T, tfdiags.Diagnostics) {
	if once == nil {
		panic("doOnceWithDiags with nil Once")
	}
	ret, err := once.Do(ctx, func(ctx context.Context) (withDiagnostics[T], error) {
		ret, diags := f(ctx)
		return withDiagnostics[T]{
			Result:      ret,
			Diagnostics: diags,
		}, nil
	})
	if err != nil {
		ret.Diagnostics = ret.Diagnostics.Append(diagnosticsForPromisingTaskError(err, root))
	}
	return ret.Result, ret.Diagnostics
}

// withCtyDynamicValPlaceholder is a workaround for an annoying wrinkle with
// [doOnceWithDiags] where a wrapped function can't return its own placeholder
// value if the call fails due to a promise-related error like a
// self-dependency.
//
// In that case the result is generated by the promises system rather than
// by the function being called and so it ends up returning [cty.NilVal], the
// zero value of [cty.Value]. This function intercepts that result and
// replaces the zero value with [cty.DynamicVal], which is typically the more
// reasonable placeholder since it allows dependent expressions to resolve
// without any knock-on errors.
//
// To use this, pass the result of [doOnceWithDiags] directly into it:
//
//	return withCtyDynamicValPlaceholder(doOnceWithDiags(/* ... */))
func withCtyDynamicValPlaceholder(result cty.Value, diags tfdiags.Diagnostics) (cty.Value, tfdiags.Diagnostics) {
	if result == cty.NilVal {
		result = cty.DynamicVal
	}
	return result, diags
}

// syncDiagnostics is a synchronization helper for functions that run two or
// more asynchronous tasks that can potentially generate diagnostics.
//
// It allows concurrent tasks to all safely append new diagnostics into a
// mutable container without data races.
type syncDiagnostics struct {
	diags tfdiags.Diagnostics
	mu    sync.Mutex
}

// Append converts all of the given arguments to zero or more diagnostics
// and appends them to the internal diagnostics list, modifying this object
// in-place.
func (sd *syncDiagnostics) Append(new ...any) {
	sd.mu.Lock()
	sd.diags = sd.diags.Append(new...)
	sd.mu.Unlock()
}

// Take retrieves all of the diagnostics accumulated so far and resets
// the internal list to empty so that future calls can append more without
// any confusion about which diagnostics were already taken.
func (sd *syncDiagnostics) Take() tfdiags.Diagnostics {
	sd.mu.Lock()
	ret := sd.diags
	sd.diags = nil
	sd.mu.Unlock()
	return ret
}

// finalDiagnosticsFromEval prepares a set of diagnostics generated by some
// calls to evaluation functions to be returned to a caller outside of this
// package. This should typically be used as a final step in functions that
// act as entry points into this package from callers in package stackruntime.
//
// Currently the only special work this does is removing any duplicate
// diagnostics relating to self-dependency problems. These tend to appear
// multiple times since all of the promises in the chain all fail at the
// same time and thus effectively the same diagnostic gets appended multiple
// times by different paths. Only the first such diagnostic will be preserved
// by this function.
func finalDiagnosticsFromEval(diags tfdiags.Diagnostics) tfdiags.Diagnostics {
	if len(diags) == 0 {
		return diags // handle the happy path as quickly as possible
	}
	if !diags.HasErrors() {
		return diags // also a relatively happy path: just warnings
	}

	// If we have at least two errors then we could potentially have a
	// duplicate self-dependency error. Self-dependency errors should be
	// relatively rare so we'll first count how many we have and only
	// go to the trouble of shuffling the diagnostics once we've proven
	// we really need to.
	foundSelfDepErrs := 0
	for _, diag := range diags {
		if diagIsPromiseSelfReference(diag) {
			foundSelfDepErrs++
		}
	}
	if foundSelfDepErrs <= 1 {
		return diags // no massaging needed
	}

	// If we get here then we _do_ have at least two self-dependency errors,
	// and so we'll perform a more expensive scan-and-shift process over the
	// diagnostics, skipping over all but the first of these errors.
	fixedSelfDepErrors := 0
	for i := 0; i < len(diags); i++ {
		diag := diags[i]
		if !diagIsPromiseSelfReference(diag) {
			continue
		}
		fixedSelfDepErrors++
		if fixedSelfDepErrors == 1 {
			continue // don't actually need to "fix" the first one
		}
		// If we get here then we have found a duplicate error, and so we'll
		// shift all of the subsequent errors to earlier indices in the slice.
		copy(diags[i:], diags[i+1:])
		diags = diags[:len(diags)-1]
		i-- // must still visit the next item that we've moved to an earlier index
	}
	return diags
}

func diagIsPromiseSelfReference(diag tfdiags.Diagnostic) bool {
	// This intentionally diverges from our usual convention of
	// using interface types for "extra info" matching because this
	// is a very specialized case confined only to this package, and
	// so we can be sure that nothing else will need to generate
	// differently-typed variants of this information.
	// (Refer to type taskSelfDependencyDiagnostic below for more on this.)
	ptr := tfdiags.ExtraInfo[*promising.ErrSelfDependent](diag)
	return ptr != nil
}

// diagnosticsForPromisingTaskError takes an error returned by
// promising.MainTask or promising.Once.Do, if any, and transforms it into one
// or more diagnostics describing the problem in a manner suitable for
// presentation directly to end-users.
//
// If the given error is nil then this always returns an empty diagnostics.
//
// This is intended only for tasks where the error result is exclusively
// used for promise- and task-related errors, with other errors already being
// presented as diagnostics. The result of this function will be relatively
// unhelpful for other errors and so better to handle those some other way.
func diagnosticsForPromisingTaskError(err error, root namedPromiseReporter) tfdiags.Diagnostics {
	if err == nil {
		return nil
	}

	var diags tfdiags.Diagnostics
	switch err := err.(type) {
	case promising.ErrSelfDependent:
		diags = diags.Append(taskSelfDependencyDiagnostics(err, root))
	case promising.ErrUnresolved:
		diags = diags.Append(taskPromisesUnresolvedDiagnostics(err, root))
	default:
		// For all other errors we'll just let tfdiags.Diagnostics do its
		// usual best effort to coerse into diagnostics.
		diags = diags.Append(err)
	}
	return diags
}

// taskSelfDependencyDiagnostics transforms a [promising.ErrSelfDependent]
// error into one or more error diagnostics suitable for returning to an
// end user, after first trying to discover user-friendly names for each
// of the promises involved using the given namedPromiseReporter.
func taskSelfDependencyDiagnostics(err promising.ErrSelfDependent, root namedPromiseReporter) tfdiags.Diagnostics {
	var diags tfdiags.Diagnostics
	// For now we just save the context about the problem, and then we'll
	// generate the human-readable description on demand once someone asks
	// for the diagnostic description.
	diags = diags.Append(taskSelfDependencyDiagnostic{
		err:  err,
		root: root,
	})
	return diags
}

// taskPromisesUnresolvedDiagnostics transforms a [promising.ErrUnresolved]
// error into one or more error diagnostics suitable for returning to an
// end user, after first trying to discover user-friendly names for each
// of the promises involved using the given namedPromiseReporter.
func taskPromisesUnresolvedDiagnostics(err promising.ErrUnresolved, root namedPromiseReporter) tfdiags.Diagnostics {
	var diags tfdiags.Diagnostics
	// For now we just save the context about the problem, and then we'll
	// generate the human-readable description on demand once someone asks
	// for the diagnostic description.
	diags = diags.Append(taskPromisesUnresolvedDiagnostic{
		err:  err,
		root: root,
	})
	return diags
}

// taskSelfDependencyDiagnostic is an implementation of tfdiags.Diagnostic
// which represents self-dependency errors in a user-oriented way.
//
// This is a special diagnostic type because self-dependency errors tend to
// emerge via multiple return paths (since they blow up all of the promises
// in the cycle all at once) and so our main entry points rely on the
// behaviors of this special type to dedupe the diagnostics before returning.
type taskSelfDependencyDiagnostic struct {
	err  promising.ErrSelfDependent
	root namedPromiseReporter
}

var _ tfdiags.Diagnostic = taskSelfDependencyDiagnostic{}

// Description implements tfdiags.Diagnostic.
func (diag taskSelfDependencyDiagnostic) Description() tfdiags.Description {
	// We build the user-oriented error message on demand, since it
	// requires collecting some supporting information from the
	// evaluation root so we know what result each of the promises was
	// actually representing.

	err := diag.err
	root := diag.root
	promiseNames := collectPromiseNames(root)
	distinctPromises := make(map[promising.PromiseID]struct{})
	for _, id := range err {
		distinctPromises[id] = struct{}{}
	}

	switch len(distinctPromises) {
	case 0:
		// Should not get here; there can't be a promise cycle without any
		// promises involved in it.
		panic("promising.ErrSelfDependent without any promises")
	case 1:
		const diagSummary = "Self-dependent item in configuration"
		var promiseID promising.PromiseID
		for id := range distinctPromises {
			promiseID = id
		}
		name, ok := promiseNames[promiseID]
		if !ok {
			// This is the worst case to report, since something depended on
			// itself but we don't actually know its name. We can't really say
			// anything useful here, so we'll treat this as a bug and then
			// we can add whatever promise name was missing in order to fix
			// that bug.
			return tfdiags.Description{
				Summary: diagSummary,
				Detail:  "One of the items in your configuration refers to its own results, but Terraform was not able to detect which one. The fact that Terraform cannot name the item is a bug; please report it!",
			}
		}
		return tfdiags.Description{
			Summary: diagSummary,
			Detail:  fmt.Sprintf("The item %q depends on its own results, so there is no correct order of operations.", name),
		}
	default:
		// If we have more than one promise involved then it's non-deterministic
		// which one we'll detect, since it depends on how the tasks get
		// scheduled by the Go runtime. To return a deterministic-ish result
		// anyway we'll arbitrarily descide to report whichever promise has
		// the lexically-least name as defined by Go's own less than operator
		// when applied to strings.
		selectedIdx := 0
		selectedName := promiseNames[err[0]]
		for i, id := range err {
			if selectedName == "" {
				// If we don't have a name yet then we'll take whatever we get
				selectedIdx = i
				selectedName = promiseNames[id]
				continue
			}
			candidateName := promiseNames[id]
			if candidateName != "" && candidateName < selectedName {
				selectedIdx = i
				selectedName = candidateName
			}
		}
		// Now we'll rotate the list of promise IDs so that the one we selected
		// appears first.
		ids := make([]promising.PromiseID, 0, len(err))
		ids = append(ids, err[selectedIdx:]...)
		ids = append(ids, err[:selectedIdx]...)
		var nameList strings.Builder
		for _, id := range ids {
			name := promiseNames[id]
			if name == "" {
				// We should minimize the number of unnamed promises so that
				// we can typically say at least something useful about what
				// objects are involved.
				name = "(...)"
			}
			fmt.Fprintf(&nameList, "\n  - %s", name)
		}
		return tfdiags.Description{
			Summary: "Self-dependent items in configuration",
			Detail: fmt.Sprintf(
				"The following items in your configuration form a circular dependency chain through their references:%s\n\nTerraform uses references to decide a suitable order for performing operations, so configuration items may not refer to their own results either directly or indirectly.",
				nameList.String(),
			),
		}
	}
}

// ExtraInfo implements tfdiags.Diagnostic.
func (diag taskSelfDependencyDiagnostic) ExtraInfo() interface{} {
	// The "extra info" for a self-dependency error is the error itself,
	// so callers can access the original promise IDs if they want to for
	// some reason.
	return &diag.err
}

// FromExpr implements tfdiags.Diagnostic.
func (diag taskSelfDependencyDiagnostic) FromExpr() *tfdiags.FromExpr {
	return nil
}

// Severity implements tfdiags.Diagnostic.
func (diag taskSelfDependencyDiagnostic) Severity() tfdiags.Severity {
	return tfdiags.Error
}

// Source implements tfdiags.Diagnostic.
func (diag taskSelfDependencyDiagnostic) Source() tfdiags.Source {
	// Self-dependency errors tend to involve multiple configuration locations
	// all at once, and so we describe the affected objects in the detail
	// text rather than as a single source location.
	return tfdiags.Source{}
}

// taskPromisesUnresolvedDiagnostic is an implementation of tfdiags.Diagnostic
// which represents a task's failure to resolve promises in a user-oriented way.
//
// This is a special dependency type just because that way we can defer
// formatting the description as long as possible, once our namedPromiseReporter
// has accumulated name information for as many promises as possible.
type taskPromisesUnresolvedDiagnostic struct {
	err  promising.ErrUnresolved
	root namedPromiseReporter
}

var _ tfdiags.Diagnostic = taskPromisesUnresolvedDiagnostic{}

// Description implements tfdiags.Diagnostic.
func (diag taskPromisesUnresolvedDiagnostic) Description() tfdiags.Description {
	// We build the user-oriented error message on demand, since it
	// requires collecting some supporting information from the
	// evaluation root so we know what result each of the promises was
	// actually representing.

	err := diag.err
	root := diag.root
	promiseNames := collectPromiseNames(root)
	distinctPromises := make(map[promising.PromiseID]struct{})
	for _, id := range err {
		distinctPromises[id] = struct{}{}
	}

	// If we have more than one promise involved then it's non-deterministic
	// which one we'll detect, since it depends on how the tasks get
	// scheduled by the Go runtime. To return a deterministic-ish result
	// anyway we'll arbitrarily descide to report whichever promise has
	// the lexically-least name as defined by Go's own less than operator
	// when applied to strings.
	selectedIdx := 0
	selectedName := promiseNames[err[0]]
	for i, id := range err {
		if selectedName == "" {
			// If we don't have a name yet then we'll take whatever we get
			selectedIdx = i
			selectedName = promiseNames[id]
			continue
		}
		candidateName := promiseNames[id]
		if candidateName != "" && candidateName < selectedName {
			selectedIdx = i
			selectedName = candidateName
		}
	}
	// Now we'll rotate the list of promise IDs so that the one we selected
	// appears first.
	ids := make([]promising.PromiseID, 0, len(err))
	ids = append(ids, err[selectedIdx:]...)
	ids = append(ids, err[:selectedIdx]...)
	var nameList strings.Builder
	for _, id := range ids {
		name := promiseNames[id]
		if name == "" {
			// We should minimize the number of unnamed promises so that
			// we can typically say at least something useful about what
			// objects are involved.
			name = "(unnamed promise)"
		}
		fmt.Fprintf(&nameList, "\n  - %s", name)
	}
	return tfdiags.Description{
		Summary: "Stack language evaluation error",
		Detail: fmt.Sprintf(
			"While evaluating the stack configuration, the following items were left unresolved:%s\n\nOther errors returned along with this one may provide more details. This is a bug in Teraform; please report it!",
			nameList.String(),
		),
	}
}

// ExtraInfo implements tfdiags.Diagnostic.
func (diag taskPromisesUnresolvedDiagnostic) ExtraInfo() interface{} {
	// The "extra info" for a resolution error is the error itself,
	// so callers can access the original promise IDs if they want to for
	// some reason.
	return &diag.err
}

// FromExpr implements tfdiags.Diagnostic.
func (diag taskPromisesUnresolvedDiagnostic) FromExpr() *tfdiags.FromExpr {
	return nil
}

// Severity implements tfdiags.Diagnostic.
func (diag taskPromisesUnresolvedDiagnostic) Severity() tfdiags.Severity {
	return tfdiags.Error
}

// Source implements tfdiags.Diagnostic.
func (diag taskPromisesUnresolvedDiagnostic) Source() tfdiags.Source {
	// A failure to resolve promises is a bug in the stacks runtime rather
	// than a problem with the provided configuration, so there's no
	// particularly-relevant source location to report.
	return tfdiags.Source{}
}

// namedPromiseReporter is an interface implemented by the types in this
// package that perform asynchronous work using the promises model implemented
// by package promising, allowing discovery of user-friendly names for promises
// involved in a particular operation.
//
// We handle this as an out-of-band action so we can avoid the overhead of
// maintaining this metadata in the common case, and instead deal with it
// retroactively only in the rare case that there's a self-dependency problem
// that exhibits as a promise resolution error.
type namedPromiseReporter interface {
	// reportNamedPromises calls the given callback for each promise that
	// the caller is responsible for, giving a user-friendly name for
	// whatever data or action that promise was responsible for.
	//
	// reportNamedPromises should also delegate to the same method on any
	// directly-nested objects that might themselves have promises, so that
	// collectPromiseNames can walk the whole tree. This should be done only
	// in situations where the original reciever's implementation is itself
	// acting as the physical container for the child objects, and not just
	// when an object is _logically_ nested within another object.
	reportNamedPromises(func(id promising.PromiseID, name string))
}

func collectPromiseNames(r namedPromiseReporter) map[promising.PromiseID]string {
	ret := make(map[promising.PromiseID]string)
	r.reportNamedPromises(func(id promising.PromiseID, name string) {
		if id != promising.NoPromise {
			ret[id] = name
		}
	})
	return ret
}

// diagnosticCausedBySensitive can be assigned to the "Extra" field of a
// diagnostic to hint to the UI layer that the sensitivity of values in scope
// is relevant to the diagnostic message.
type diagnosticCausedBySensitive bool

var _ tfdiags.DiagnosticExtraBecauseSensitive = diagnosticCausedBySensitive(false)

// DiagnosticCausedBySensitive implements tfdiags.DiagnosticExtraBecauseSensitive.
func (d diagnosticCausedBySensitive) DiagnosticCausedBySensitive() bool {
	return bool(d)
}

// diagnosticCausedByEphemeral can be assigned to the "Extra" field of a
// diagnostic to hint to the UI layer that the ephemerality of values in scope
// is relevant to the diagnostic message.
type diagnosticCausedByEphemeral bool

var _ tfdiags.DiagnosticExtraBecauseEphemeral = diagnosticCausedByEphemeral(false)

// DiagnosticCausedByEphemeral implements tfdiags.DiagnosticExtraBecauseEphemeral.
func (d diagnosticCausedByEphemeral) DiagnosticCausedByEphemeral() bool {
	return bool(d)
}