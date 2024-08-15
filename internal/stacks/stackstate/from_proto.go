// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package stackstate

import (
	"fmt"
	"log"

	"github.com/zclconf/go-cty/cty"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/hashicorp/terraform/internal/addrs"
	"github.com/hashicorp/terraform/internal/plans/planfile"
	"github.com/hashicorp/terraform/internal/stacks/stackaddrs"
	"github.com/hashicorp/terraform/internal/stacks/stackstate/statekeys"
	"github.com/hashicorp/terraform/internal/stacks/tfstackdata1"
	"github.com/hashicorp/terraform/internal/states"
)

// LoadFromProto produces a [State] object by decoding a raw state map.
//
// This is the primary way to load a "prior state" provided by a caller
// into memory so we can use it in the stack runtime.
func LoadFromProto(msgs map[string]*anypb.Any) (*State, error) {
	ret := NewState()
	ret.inputRaw = msgs
	for rawKey, rawMsg := range msgs {
		key, err := statekeys.Parse(rawKey)
		if err != nil {
			// "invalid" here means that it was either not syntactically
			// valid at all or was a recognized type but with the wrong
			// syntax for that type.
			// An unrecognized key type is NOT invalid; we handle that below.
			return nil, fmt.Errorf("invalid tracking key %q in state: %w", rawKey, err)
		}

		if !statekeys.RecognizedType(key) {
			err = handleUnrecognizedKey(key, ret)
			if err != nil {
				return nil, err
			}
			continue
		}

		if rawMsg == nil {
			// This suggests a state mutation bug where a deleted object was
			// written as a map entry without a value, as opposed to deleting
			// the value. We tolerate this here just because otherwise it
			// would be harder to recover once a state has been mutated
			// incorrectly.
			log.Panicf("[WARN] stackstate.LoadFromProto: key %s has no associated object; ignoring", rawKey)
			continue
		}

		msg, err := anypb.UnmarshalNew(rawMsg, proto.UnmarshalOptions{})
		if err != nil {
			return nil, fmt.Errorf("invalid raw value for raw state key %q: %w", rawKey, err)
		}

		err = handleProtoMsg(key, msg, ret)
		if err != nil {
			return nil, err
		}
	}
	return ret, nil
}

// LoadFromDirectProto is a variation of the primary entry-point [LoadFromProto]
// which accepts direct messages of the relevant types from the tfstackdata1
// package, rather than the [anypb.Raw] representation thereof.
//
// This is primarily for internal testing purposes, where it's typically more
// convenient to write out a struct literal for one of the message types
// directly rather than having to first serialize it to [anypb.Any] only for
// it to be unserialized again promptly afterwards.
//
// Unlike [LoadFromProto], the state object produced by this function will not
// have any record of the "raw state" it was created from, because this function
// is bypassing the concept of raw state. [State.InputRaw] will therefore
// return an empty map.
//
// Prefer to use [LoadFromProto] when processing user input. This function
// cannot accept [anypb.Any] messages even though the Go compiler can't enforce
// that at compile time.
func LoadFromDirectProto(msgs map[string]protoreflect.ProtoMessage) (*State, error) {
	ret := NewState()
	ret.inputRaw = nil // this doesn't get populated by this entry point
	for keyStr, msg := range msgs {
		// The following should be equivalent to the similar loop in
		// [LoadFromProto] except for skipping the parsing/unmarshalling
		// steps since msg is already in its in-memory form.
		key, err := statekeys.Parse(keyStr)
		if err != nil {
			return nil, fmt.Errorf("invalid tracking key %q: %w", keyStr, err)
		}
		if !statekeys.RecognizedType(key) {
			err := handleUnrecognizedKey(key, ret)
			if err != nil {
				return nil, err
			}
			continue
		}
		err = handleProtoMsg(key, msg, ret)
		if err != nil {
			return nil, err
		}
	}
	return ret, nil
}

func handleUnrecognizedKey(key statekeys.Key, state *State) error {
	// There are three different strategies for dealing with
	// unrecognized keys, which we recognize based on naming
	// conventions of the key types.
	switch handling := key.KeyType().UnrecognizedKeyHandling(); handling {

	case statekeys.FailIfUnrecognized:
		// This is for keys whose messages materially change the
		// meaning of the state and so cannot be ignored. Keys
		// with this treatment are forwards-incompatible (old versions
		// of Terraform will fail to load a state containing them) so
		// should be added only as a last resort.
		return fmt.Errorf("state was created by a newer version of Terraform Core (unrecognized tracking key %q)", statekeys.String(key))

	case statekeys.PreserveIfUnrecognized:
		// This is for keys whose messages can safely be left entirely
		// unchanged if applying a plan with a version of Terraform
		// that doesn't understand them. Keys in this category should
		// typically be standalone and not refer to or depend on any
		// other objects in the state, to ensure that removing or
		// updating other objects will not cause the preserved message
		// to become misleading or invalid.
		// We don't need to do anything special with these ones because
		// the caller should preserve any object we don't explicitly
		// update or delete during the apply phase.
		return nil

	case statekeys.DiscardIfUnrecognized:
		// This is for keys which can be discarded when planning or
		// applying with an older version of Terraform that doesn't
		// understand them. This category is for optional ancillary
		// information -- not actually required for correct subsequent
		// planning -- especially if it could be recomputed again and
		// repopulated if later planning and applying with a newer
		// version of Terraform Core.
		// For these ones we need to remember their keys so that we
		// can emit "delete" messages early in the apply phase to
		// actually discard them from the caller's records.
		state.discardUnsupportedKeys.Add(key)
		return nil

	default:
		// Should not get here. The above should be exhaustive.
		panic(fmt.Sprintf("unsupported UnrecognizedKeyHandling value %s", handling))
	}
}

func handleProtoMsg(key statekeys.Key, msg protoreflect.ProtoMessage, state *State) error {
	switch key := key.(type) {

	case statekeys.ComponentInstance:
		return handleComponentInstanceMsg(key, msg, state)

	case statekeys.ResourceInstanceObject:
		return handleResourceInstanceObjectMsg(key, msg, state)

	default:
		// Should not get here: the above should be exhaustive for all
		// possible key types.
		panic(fmt.Sprintf("unsupported state key type %T", key))
	}
}

func handleComponentInstanceMsg(key statekeys.ComponentInstance, msg protoreflect.ProtoMessage, state *State) error {
	// For this particular object type all of the information is in the key,
	// for now at least.
	_, ok := msg.(*tfstackdata1.StateComponentInstanceV1)
	if !ok {
		return fmt.Errorf("unsupported message type %T for %s state", msg, key.ComponentInstanceAddr)
	}

	state.ensureComponentInstanceState(key.ComponentInstanceAddr)
	return nil
}

func handleResourceInstanceObjectMsg(key statekeys.ResourceInstanceObject, msg protoreflect.ProtoMessage, state *State) error {
	fullAddr := stackaddrs.AbsResourceInstanceObject{
		Component: key.ResourceInstance.Component,
		Item: addrs.AbsResourceInstanceObject{
			ResourceInstance: key.ResourceInstance.Item,
			DeposedKey:       key.DeposedKey,
		},
	}

	riMsg, ok := msg.(*tfstackdata1.StateResourceInstanceObjectV1)
	if !ok {
		return fmt.Errorf("unsupported message type %T for state of %s", msg, fullAddr.String())
	}

	objSrc, err := DecodeProtoResourceInstanceObject(riMsg)
	if err != nil {
		return fmt.Errorf("invalid stored state object for %s: %w", fullAddr, err)
	}

	providerConfigAddr, diags := addrs.ParseAbsProviderConfigStr(riMsg.ProviderConfigAddr)
	if diags.HasErrors() {
		return fmt.Errorf("provider configuration reference %q for %s", riMsg.ProviderConfigAddr, fullAddr)
	}

	state.addResourceInstanceObject(fullAddr, objSrc, providerConfigAddr)
	return nil
}

func DecodeProtoResourceInstanceObject(protoObj *tfstackdata1.StateResourceInstanceObjectV1) (*states.ResourceInstanceObjectSrc, error) {
	objSrc := &states.ResourceInstanceObjectSrc{
		SchemaVersion:       protoObj.SchemaVersion,
		AttrsJSON:           protoObj.ValueJson,
		CreateBeforeDestroy: protoObj.CreateBeforeDestroy,
		Private:             protoObj.ProviderSpecificData,
	}

	switch protoObj.Status {
	case tfstackdata1.StateResourceInstanceObjectV1_READY:
		objSrc.Status = states.ObjectReady
	case tfstackdata1.StateResourceInstanceObjectV1_DAMAGED:
		objSrc.Status = states.ObjectTainted
	default:
		return nil, fmt.Errorf("unsupported status %s", protoObj.Status.String())
	}

	paths := make([]cty.Path, 0, len(protoObj.SensitivePaths))
	for _, p := range protoObj.SensitivePaths {
		path, err := planfile.PathFromProto(p)
		if err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	objSrc.AttrSensitivePaths = paths

	if len(protoObj.Dependencies) != 0 {
		objSrc.Dependencies = make([]addrs.ConfigResource, len(protoObj.Dependencies))
		for i, raw := range protoObj.Dependencies {
			instAddr, diags := addrs.ParseAbsResourceInstanceStr(raw)
			if diags.HasErrors() {
				return nil, fmt.Errorf("invalid dependency %q", raw)
			}
			// We used the resource instance address parser here but we
			// actually want the "config resource" subset of that syntax only.
			configAddr := instAddr.ConfigResource()
			if configAddr.String() != instAddr.String() {
				return nil, fmt.Errorf("invalid dependency %q", raw)
			}
			objSrc.Dependencies[i] = configAddr
		}
	}

	return objSrc, nil
}
