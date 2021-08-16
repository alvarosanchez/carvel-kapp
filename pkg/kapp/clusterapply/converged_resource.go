// Copyright 2020 VMware, Inc.
// SPDX-License-Identifier: Apache-2.0

package clusterapply

import (
	"fmt"
	"reflect"
	"sort"
	"time"

	"github.com/cppforlife/color"
	ctlres "github.com/k14s/kapp/pkg/kapp/resources"
	ctlresm "github.com/k14s/kapp/pkg/kapp/resourcesmisc"
)

const (
	disableAssociatedResourcesWaitingAnnKey = "kapp.k14s.io/disable-associated-resources-wait" // valid value is ''
)

type ConvergedResource struct {
	res                  ctlres.Resource
	associatedRsFunc     func(ctlres.Resource, []ctlres.ResourceRef) ([]ctlres.Resource, error)
	specificResFactories []SpecificResFactory
	resourceWaitTimeout  time.Duration
}

type SpecificResFactory func(ctlres.Resource, []ctlres.Resource) (SpecificResource, []ctlres.ResourceRef)

func NewConvergedResource(res ctlres.Resource,
	associatedRsFunc func(ctlres.Resource, []ctlres.ResourceRef) ([]ctlres.Resource, error),
	specificResFactories []SpecificResFactory, resourceWaitTimeout time.Duration) ConvergedResource {
	return ConvergedResource{res, associatedRsFunc, specificResFactories, resourceWaitTimeout}
}

func (c ConvergedResource) IsDoneApplying() (ctlresm.DoneApplyState, []string, bool, error) {
	startTime := time.Now()
	var descMsgs []string

	associatedRs, err := c.associatedRs()
	if err != nil {
		return ctlresm.DoneApplyState{}, nil, false, err
	}

	convergedResState, err := c.isResourceDoneApplying(c.res, associatedRs)
	if err != nil {
		return ctlresm.DoneApplyState{Done: true}, descMsgs, false, err
	}

	if convergedResState != nil {
		// we're always interested in the parent's state description, regardless
		descMsgs = append(descMsgs, c.buildParentDescMsg(c.res, *convergedResState)...)
		// If the parent is done, remaining state calculations are waste; exit now.
		if convergedResState.Done {
			return *convergedResState, descMsgs, false, nil
		}
	}

	// If resource explicitly opts out of associated resource waiting
	// exit quickly with parent resource state or success.
	// For example, CronJobs should be annotated with this to avoid
	// picking up failed Pods from previous runs.
	disableARWVal, disableARWFound := c.res.Annotations()[disableAssociatedResourcesWaitingAnnKey]
	if disableARWFound {
		if disableARWVal != "" {
			return ctlresm.DoneApplyState{Done: true}, descMsgs, false,
				fmt.Errorf("Expected annotation '%s' on resource '%s' to have value ''",
					disableAssociatedResourcesWaitingAnnKey, c.res.Description())
		}
		if convergedResState != nil {
			return *convergedResState, descMsgs, false, nil
		}
		return ctlresm.DoneApplyState{Done: true, Successful: true}, descMsgs, false, nil
	}

	associatedRsStates := []ctlresm.DoneApplyState{}

	// Show associated resources even though we are waiting for the parent.
	// This additional info may be helpful in identifying what parent is waiting for.
	for _, res := range associatedRs {
		state, err := c.isResourceDoneApplying(res, associatedRs)
		if state == nil {
			state = &ctlresm.DoneApplyState{Done: true, Successful: true}
		}
		if err != nil {
			return *state, descMsgs, false, err
		}

		associatedRsStates = append(associatedRsStates, *state)
		descMsgs = append(descMsgs, c.buildChildDescMsg(res, *state)...)
	}

	// If parent state is present, ignore all associated resource states
	if convergedResState != nil {
		return *convergedResState, descMsgs, false, nil
	}

	if time.Now().Sub(startTime) > c.resourceWaitTimeout {
		return ctlresm.DoneApplyState{}, nil, true, fmt.Errorf("Resource wait timed out waiting after %s", c.resourceWaitTimeout)
	}

	for _, state := range associatedRsStates {
		if state.TerminallyFailed() {
			return state, descMsgs, false, nil
		}
	}

	for _, state := range associatedRsStates {
		if !state.Done {
			return state, descMsgs, false, nil
		}
	}
	return ctlresm.DoneApplyState{Done: true, Successful: true}, descMsgs, false, nil
}

func (c ConvergedResource) associatedRs() ([]ctlres.Resource, error) {
	if c.associatedRsFunc == nil {
		return nil, nil
	}
	for _, f := range c.specificResFactories {
		matchedRes, associatedResRefs := f(c.res, nil)
		// Is this SpecificResFactory applicable to that kind of resource?
		if !reflect.ValueOf(matchedRes).IsNil() {
			// querying the cluster (for associated res) is expensive
			if len(associatedResRefs) > 0 {
				associatedRs, err := c.associatedRsFunc(c.res, associatedResRefs)
				if err != nil {
					return nil, err
				}
				return c.sortAssociatedRs(associatedRs), nil
			}
			break
		}
	}
	return nil, nil
}

func (c ConvergedResource) sortAssociatedRs(associatedRs []ctlres.Resource) []ctlres.Resource {
	convergedResKey := ctlres.NewUniqueResourceKey(c.res).String()

	// Sort so that resources show up more or less consistently
	sort.Slice(associatedRs, func(i, j int) bool {
		return associatedRs[i].Description() > associatedRs[j].Description()
	})

	// Remove possibly found parent resource
	for i, res := range associatedRs {
		if convergedResKey == ctlres.NewUniqueResourceKey(res).String() {
			associatedRs = append(associatedRs[:i], associatedRs[i+1:]...)
			break
		}
	}

	return associatedRs
}

func (c ConvergedResource) isResourceDoneApplying(res ctlres.Resource,
	associatedRs []ctlres.Resource) (*ctlresm.DoneApplyState, error) {

	for _, f := range c.specificResFactories {
		matchedRes, _ := f(res, associatedRs)
		// Is this SpecificResFactory applicable to that kind of resource?
		if !reflect.ValueOf(matchedRes).IsNil() {
			state := matchedRes.IsDoneApplying()
			return &state, nil
		}
	}
	return nil, nil
}

var (
	uiWaitChildPrefix    = color.New(color.Faint).Sprintf(" L ") // consistent with inspect tree view
	uiWaitMsgPrefix      = color.New(color.Faint).Sprintf(" ^ ")
	uiWaitChildMsgPrefix = "   " + uiWaitMsgPrefix
)

func (c ConvergedResource) buildParentDescMsg(res ctlres.Resource, state ctlresm.DoneApplyState) []string {
	if len(state.Message) > 0 {
		return []string{uiWaitMsgPrefix + state.Message}
	}
	return []string{}
}

func (c ConvergedResource) buildChildDescMsg(res ctlres.Resource, state ctlresm.DoneApplyState) []string {
	msgs := []string{fmt.Sprintf(uiWaitChildPrefix+"%s: waiting on %s", NewDoneApplyStateUI(state, nil).State, res.Description())}

	if len(state.Message) > 0 {
		msgs = append(msgs, uiWaitChildMsgPrefix+state.Message)
	}

	return msgs
}
