// Copyright (C) 2019-2020 Algorand, Inc.
// This file is part of go-algorand
//
// go-algorand is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as
// published by the Free Software Foundation, either version 3 of the
// License, or (at your option) any later version.
//
// go-algorand is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with go-algorand.  If not, see <https://www.gnu.org/licenses/>.

package transactions

import (
	"fmt"

	"github.com/algorand/go-algorand/data/basics"
)

type OnCompletion uint64

const (
	NoOpOC              OnCompletion = 0
	OptInOC             OnCompletion = 1
	CloseOutOC          OnCompletion = 2
	ClearStateOC        OnCompletion = 3
	UpdateApplicationOC OnCompletion = 4
	DeleteApplicationOC OnCompletion = 5
)

type ApplicationCallTxnFields struct {
	_struct struct{} `codec:",omitempty,omitemptyarray"`

	ApplicationID   basics.AppIndex  `codec:"apid"`
	OnCompletion    OnCompletion     `codec:"apan"`
	ApplicationArgs []string         `codec:"apaa,allocbound=1024"`
	Accounts        []basics.Address `codec:"apat,allocbound=1024"`

	LocalStateSchema  basics.StateSchema `codec:"apls"`
	GlobalStateSchema basics.StateSchema `codec:"apgs"`
	ApprovalProgram   string             `codec:"apap"`
	ClearStateProgram string             `codec:"apsu"`

	// If you add any fields here, remember you MUST modify the Empty
	// method below!
}

func (ac ApplicationCallTxnFields) Empty() bool {
	if ac.ApplicationID != 0 {
		return false
	}
	if ac.OnCompletion != 0 {
		return false
	}
	if ac.ApplicationArgs != nil {
		return false
	}
	if ac.Accounts != nil {
		return false
	}
	if ac.LocalStateSchema != (basics.StateSchema{}) {
		return false
	}
	if ac.GlobalStateSchema != (basics.StateSchema{}) {
		return false
	}
	if ac.ApprovalProgram != "" {
		return false
	}
	if ac.ClearStateProgram != "" {
		return false
	}
	return true
}

// Allocate the map of LocalStates if it is nil, and then clone all LocalStates
func cloneAppLocalStates(m map[basics.AppIndex]basics.AppLocalState) map[basics.AppIndex]basics.AppLocalState {
	res := make(map[basics.AppIndex]basics.AppLocalState, len(m))
	for k, v := range m {
		// TODO if required: performance improvement: only clone
		// LocalState for app idx affected by this transaction
		res[k] = v.Clone()
	}
	return res
}

// Allocate the map of AppParams if it is nil, and then clone all AppParams
func cloneAppParams(m map[basics.AppIndex]basics.AppParams) map[basics.AppIndex]basics.AppParams {
	res := make(map[basics.AppIndex]basics.AppParams, len(m))
	for k, v := range m {
		// TODO if required: performance improvement: only clone
		// AppParams (and thus GlobalState) for app idx affected
		// by this transaction
		res[k] = v.Clone()
	}
	return res
}

// getAppParams fetches the creator address and AppParams for the app index,
// if they exist. It does NOT clone the AppParams, so the returned params must
// not be modified directly.
func getAppParams(balances Balances, aidx basics.AppIndex) (params basics.AppParams, creator basics.Address, doesNotExist bool, err error) {
	creator, doesNotExist, err = balances.GetAppCreator(aidx)
	if err != nil {
		return
	}

	// App doesn't exist. Not an error, but return straight away
	if doesNotExist {
		return
	}

	record, err := balances.Get(creator, false)
	if err != nil {
		return
	}

	params, ok := record.AppParams[aidx]
	if !ok {
		// This should never happen. If app exists then we should have
		// found the creator successfully. TODO(applications) panic here?
		err = fmt.Errorf("app %d not found in account %s", aidx, creator.String())
		return
	}

	return
}

func applyDelta(stateDelta basics.StateDelta, kv basics.TealKeyValue) error {
	if kv == nil {
		return fmt.Errorf("cannot apply delta to nil TealKeyValue")
	}
	for key, valueDelta := range stateDelta {
		switch valueDelta.Action {
		case basics.SetUintAction:
			kv[key] = basics.TealValue{
				Type: basics.TealUintType,
				Uint: valueDelta.Uint,
			}
		case basics.SetBytesAction:
			kv[key] = basics.TealValue{
				Type:  basics.TealBytesType,
				Bytes: valueDelta.Bytes,
			}
		case basics.DeleteAction:
			delete(kv, key)
		default:
			return fmt.Errorf("unknown delta action %d", valueDelta.Action)
		}
	}
	return nil
}

// applyStateDeltas applies a basics.EvalDelta to the app's global key/value
// store as well as a set of local key/value stores. If this function returns
// an error, the transaction must not be committed.
func applyStateDeltas(evalDelta basics.EvalDelta, balances Balances, appIdx basics.AppIndex, errIfNotApplied bool) error {
	// Fetch the application parameters and the creator's address
	params, creator, doesNotExist, err := getAppParams(balances, appIdx)
	if err != nil {
		return err
	}

	if doesNotExist {
		// This case should not happen, because an application that does not
		// exist should not have programs that could produce state deltas (and
		// we shouldn't have been called, period)
		return fmt.Errorf("cannot apply state deltas to deleted application")
	}

	/*
	 * 1. Apply GlobalState delta, allocating the key/value store if req'd
	 */

	// Clone the parameters so that they are safe to modify
	params = params.Clone()

	// Apply the GlobalDelta in place
	err = applyDelta(evalDelta.GlobalDelta, params.GlobalState)
	if err != nil {
		return err
	}

	// Make sure we haven't violated the GlobalStateSchema
	if !params.GlobalState.SatisfiesSchema(params.GlobalStateSchema) {
		if !errIfNotApplied {
			return nil
		}
		return fmt.Errorf("GlobalState for app %d would use too much space", appIdx)
	}

	/*
	 * 2. Apply each LocalState delta, fail fast if any affected account
	 *    has not opted in to appIdx or would violate the LocalStateSchema.
	 *    Don't write anything back to the cow yet.
	 */

	changes := make(map[basics.Address]basics.AppLocalState, len(evalDelta.LocalDeltas))
	for addr, delta := range evalDelta.LocalDeltas {
		// Skip over empty deltas, because we shouldn't fail because of
		// a zero-delta on an account that hasn't opted in
		if len(delta) == 0 {
			continue
		}

		record, err := balances.Get(addr, false)
		if err != nil {
			return err
		}

		localState, ok := record.AppLocalStates[appIdx]
		if !ok {
			if !errIfNotApplied {
				return nil
			}
			return fmt.Errorf("cannot apply LocalState delta to %v: acct has not opted in to app %d", addr.String(), appIdx)
		}

		// Clone local states + app params, so that we have a copy that is
		// safe to modify
		localState = localState.Clone()
		err = applyDelta(delta, localState.KeyValue)
		if err != nil {
			return err
		}

		// Make sure we haven't violated the LocalStateSchema
		if !localState.KeyValue.SatisfiesSchema(localState.Schema) {
			if !errIfNotApplied {
				return nil
			}
			return fmt.Errorf("LocalState for %s for app %d would use too much space", addr.String(), appIdx)
		}

		// Stage the change to be committed after schema checks
		changes[addr] = localState
	}

	/*
	 * 3. Write GlobalState changes back to cow. This should be correct
	 *    even if creator is in the local deltas, because the updated
	 *    fields are orthogonal.
	 */

	record, err := balances.Get(creator, false)
	if err != nil {
		return err
	}

	record.AppParams = cloneAppParams(record.AppParams)
	record.AppParams[appIdx] = params

	err = balances.Put(record)
	if err != nil {
		return err
	}

	/*
	 * 4. Write LocalState changes back to cow
	 */

	for addr, newLocalState := range changes {
		record, err := balances.Get(addr, false)
		if err != nil {
			return err
		}

		record.AppLocalStates = cloneAppLocalStates(record.AppLocalStates)
		record.AppLocalStates[appIdx] = newLocalState

		err = balances.Put(record)
		if err != nil {
			return err
		}
	}

	return nil
}

func (ac ApplicationCallTxnFields) apply(header Header, balances Balances, steva StateEvaluator, spec SpecialAddresses, ad *ApplyData, txnCounter uint64) error {
	// Keep track of the application ID we're working on
	appIdx := ac.ApplicationID

	// If we're creating an application, allocate its AppParams
	if ac.ApplicationID == 0 {
		// Creating an application. Fetch the creator's balance record
		record, err := balances.Get(header.Sender, false)
		if err != nil {
			return err
		}

		// Clone local states + app params, so that we have a copy that is
		// safe to modify
		record.AppLocalStates = cloneAppLocalStates(record.AppLocalStates)
		record.AppParams = cloneAppParams(record.AppParams)

		// Allocate the new app params (+ 1 so that we never have
		// appIdx == 0 and to match Assets ID namespace)
		appIdx = basics.AppIndex(txnCounter + 1)
		record.AppParams[appIdx] = basics.AppParams{
			ApprovalProgram:   ac.ApprovalProgram,
			ClearStateProgram: ac.ClearStateProgram,
			LocalStateSchema:  ac.LocalStateSchema,
			GlobalStateSchema: ac.GlobalStateSchema,
			GlobalState:       make(basics.TealKeyValue),
		}

		// Write back to the creator's balance record and continue
		err = balances.Put(record)
		if err != nil {
			return err
		}
	}

	// Fetch the application parameters, if they exist
	params, creator, doesNotExist, err := getAppParams(balances, appIdx)
	if err != nil {
		return err
	}

	// Initialize our TEAL evaluation context. Internally, this manages
	// access to balance records for Stateful TEAL programs. Stateful TEAL
	// may only access the sender's balance record or the balance records
	// of accounts explicitly listed in ac.Accounts. Implicitly, the
	// creator's balance record may be accessed via GlobalState.
	whitelistWithSender := append(ac.Accounts, header.Sender)
	err = steva.InitLedger(balances, whitelistWithSender, appIdx)
	if err != nil {
		return err
	}

	// Clear out our LocalState. In this case, we don't execute the
	// ApprovalProgram, since clearing out is always allowed. We only
	// execute the ClearStateProgram, whose failures are ignored.
	if ac.OnCompletion == ClearStateOC {
		record, err := balances.Get(header.Sender, false)
		if err != nil {
			return err
		}

		// Ensure sender actually has LocalState allocated for this app.
		// Can't close out if not currently opted in
		_, ok := record.AppLocalStates[appIdx]
		if !ok {
			return fmt.Errorf("cannot clear state for app %d, not currently opted in", appIdx)
		}

		// If the application still exists...
		if !doesNotExist {
			// Execute the ClearStateProgram before we've deleted the LocalState
			// for this account. Ignore whether or not it succeeded or failed.
			// ClearState transactions may never be rejected by app logic.
			_, stateDeltas, err := steva.Eval([]byte(params.ClearStateProgram))
			if err == nil {
				// Program execution may produce some GlobalState and LocalState
				// deltas. Apply them, provided they don't exceed the bounds set by
				// the GlobalStateSchema and LocalStateSchema. If they do exceed
				// those bounds, then don't fail, but also don't apply the changes.
				failIfNotApplied := false
				err = applyStateDeltas(stateDeltas, balances, appIdx, failIfNotApplied)
				if err != nil {
					return err
				}
			} else {
				// Ignore errors from the ClearStateProgram
			}
		}

		// Deallocate the AppLocalState and finish
		record, err = balances.Get(header.Sender, false)
		if err != nil {
			return err
		}
		record.AppLocalStates = cloneAppLocalStates(record.AppLocalStates)
		delete(record.AppLocalStates, appIdx)

		return balances.Put(record)
	}

	// Past this point, the AppParams must exist. NoOp, OptIn, CloseOut,
	// Delete, and Update
	if doesNotExist {
		return fmt.Errorf("only clearing out is supported for applications that do not exist")
	}

	// If this is an OptIn transaction, ensure that the sender has already
	// allocated their LocalState
	if ac.OnCompletion == OptInOC {
		record, err := balances.Get(header.Sender, false)
		if err != nil {
			return err
		}

		// TODO(applications) is it OK for an application with an empty schema
		// to allocate an empty map here?

		// If the user hasn't opted in yet, allocate their LocalState
		_, ok := record.AppLocalStates[appIdx]
		if !ok {
			record.AppLocalStates = cloneAppLocalStates(record.AppLocalStates)
			record.AppLocalStates[appIdx] = basics.AppLocalState{
				Schema:   params.LocalStateSchema,
				KeyValue: make(basics.TealKeyValue),
			}
			err = balances.Put(record)
			if err != nil {
				return err
			}
		}
	}

	// Execute the Approval program
	approved, stateDeltas, err := steva.Eval([]byte(params.ApprovalProgram))
	if err != nil {
		return err
	}

	if !approved {
		return fmt.Errorf("transaction rejected by ApprovalProgram")
	}

	// Apply GlobalState and LocalState deltas, provided they don't exceed
	// the bounds set by the GlobalStateSchema and LocalStateSchema.
	// If they would exceed those bounds, then fail.
	failIfNotApplied := true
	err = applyStateDeltas(stateDeltas, balances, appIdx, failIfNotApplied)
	if err != nil {
		return err
	}

	switch ac.OnCompletion {
	case NoOpOC:
		// Nothing to do

	case OptInOC:
		// Handled above

	case CloseOutOC:
		// Closing out of the application. Fetch the sender's balance record
		record, err := balances.Get(header.Sender, false)
		if err != nil {
			return err
		}

		// If they haven't opted in, that's an error
		_, ok := record.AppLocalStates[appIdx]
		if !ok {
			return fmt.Errorf("account is not opted in to app %d", appIdx)
		}

		// Delete the local state
		record.AppLocalStates = cloneAppLocalStates(record.AppLocalStates)
		delete(record.AppLocalStates, appIdx)
		err = balances.Put(record)
		if err != nil {
			return err
		}

	case DeleteApplicationOC:
		// Deleting the application. Fetch the creator's balance record
		record, err := balances.Get(creator, false)
		if err != nil {
			return err
		}

		// Delete the AppParams
		record.AppParams = cloneAppParams(record.AppParams)
		delete(record.AppParams, appIdx)
		err = balances.Put(record)
		if err != nil {
			return err
		}

	case UpdateApplicationOC:
		// Ensure user isn't trying to update the local or global state
		// schemas, because that operation is not allowed
		if ac.LocalStateSchema != (basics.StateSchema{}) ||
			ac.GlobalStateSchema != (basics.StateSchema{}) {
			return fmt.Errorf("local and global state schemas are immutable")
		}

		// Updating the application. Fetch the creator's balance record
		record, err := balances.Get(creator, false)
		if err != nil {
			return err
		}

		record.AppParams = cloneAppParams(record.AppParams)

		// Fill in the updated programs
		params := record.AppParams[appIdx]

		params.ApprovalProgram = ac.ApprovalProgram
		params.ClearStateProgram = ac.ClearStateProgram

		record.AppParams[appIdx] = params
		err = balances.Put(record)
		if err != nil {
			return err
		}

	default:
		return fmt.Errorf("invalid application action")
	}

	return nil
}