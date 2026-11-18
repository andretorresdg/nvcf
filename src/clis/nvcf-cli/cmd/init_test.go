/*
SPDX-FileCopyrightText: Copyright (c) NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cmd

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"nvcf-cli/internal/state"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	initTestBaseURL        = "http://api.test.local:9999"
	initTestInvokeURL      = "http://invoke.test.local:9999"
	initTestNVCTURL        = "http://nvct.test.local:9999"
	initTestEventLedgerURL = "http://events.test.local:9999"
	initTestAccount        = "nvcf-default"
)

// initPrev is the endpoint set that the seeded state was saved against.
type initPrev struct {
	baseURL, invokeURL, account, nvctURL, eventLedgerURL string
}

func matchingInitPrev() initPrev {
	return initPrev{
		baseURL:        initTestBaseURL,
		invokeURL:      initTestInvokeURL,
		account:        initTestAccount,
		nvctURL:        initTestNVCTURL,
		eventLedgerURL: initTestEventLedgerURL,
	}
}

// seedInitState installs a fresh state manager holding an admin token, the
// function, task and Event Ledger API keys, a selected function, and the given
// previous endpoints. It points every API keys URL at a fake server that mints
// admin tokens and returns the expiration it gave the saved keys.
func seedInitState(t *testing.T, prev initPrev) time.Time {
	t.Helper()
	t.Setenv("HOME", t.TempDir())

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"value":"new-admin-token"}`))
	}))
	t.Cleanup(srv.Close)

	viper.Reset()
	viper.Set("base_http_url", initTestBaseURL)
	viper.Set("base_grpc_url", "localhost:50051")
	viper.Set("invoke_url", initTestInvokeURL)
	viper.Set("base_nvct_url", initTestNVCTURL)
	viper.Set("event_ledger_url", initTestEventLedgerURL)
	viper.Set("client_id", initTestAccount)
	t.Cleanup(func() { viper.Reset() })
	t.Setenv("api_keys_service_url", srv.URL)
	t.Setenv("api_keys_admin_service_url", srv.URL)

	orig := state.DefaultStateManager
	sm := state.NewStateManager()
	state.DefaultStateManager = sm
	t.Cleanup(func() { state.DefaultStateManager = orig })

	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	sm.SetTokens("old-admin-token", "function-key", exp, exp)
	sm.SetNVCTAPIKey("task-key", exp)
	sm.SetEventLedgerAPIKey("event-ledger-key", exp)
	sm.SetFunction("fn-id", "ver-id", "fn")
	sm.SetEndpoints(prev.baseURL, prev.invokeURL, prev.account)
	sm.SetKeyEndpoints(prev.nvctURL, prev.eventLedgerURL)
	require.NoError(t, sm.Save())
	return exp
}

func TestInit_KeepsAPIKeysForSameDeployment(t *testing.T) {
	exp := seedInitState(t, matchingInitPrev())

	require.NoError(t, runInit(nil, nil))

	got := state.DefaultStateManager.GetState()
	assert.Equal(t, "new-admin-token", got.Token, "admin token must be replaced")
	assert.Equal(t, "function-key", got.APIKey, "function API key must survive init")
	assert.Equal(t, "task-key", got.NVCTAPIKey, "task API key must survive init")
	assert.Equal(t, "event-ledger-key", got.EventLedgerAPIKey, "Event Ledger key must survive init")
	assert.True(t, exp.Equal(got.APIKeyExpiration), "function key expiration must be unchanged")
	assert.True(t, exp.Equal(got.NVCTAPIKeyExpiration), "task key expiration must be unchanged")
	assert.True(t, exp.Equal(got.EventLedgerAPIKeyExpiration), "Event Ledger key expiration must be unchanged")
	assert.Empty(t, got.FunctionID, "function context is still reset")
}

// Each case changes exactly one input, so removing any single comparison from
// runInit makes its case fail.
func TestInit_DropsAPIKeysWhenTheirTargetChanges(t *testing.T) {
	cases := []struct {
		name                           string
		mutate                         func(*initPrev)
		wantFunction, wantTask, wantEL bool
	}{
		{"base URL", func(p *initPrev) { p.baseURL = "http://other.test.local:9999" }, false, false, false},
		{"invoke URL", func(p *initPrev) { p.invokeURL = "http://other-invoke.test.local:9999" }, false, false, false},
		{"account", func(p *initPrev) { p.account = "other-account" }, false, false, false},
		{"NVCT URL only", func(p *initPrev) { p.nvctURL = "http://other-nvct.test.local:9999" }, true, false, true},
		{"Event Ledger URL only", func(p *initPrev) { p.eventLedgerURL = "http://other-events.test.local:9999" }, true, true, false},
		{"state without recorded NVCT URL", func(p *initPrev) { p.nvctURL = "" }, true, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prev := matchingInitPrev()
			tc.mutate(&prev)
			seedInitState(t, prev)

			require.NoError(t, runInit(nil, nil))

			got := state.DefaultStateManager.GetState()
			assert.Equal(t, "new-admin-token", got.Token)
			assertKey(t, tc.wantFunction, "function-key", got.APIKey, got.APIKeyExpiration)
			assertKey(t, tc.wantTask, "task-key", got.NVCTAPIKey, got.NVCTAPIKeyExpiration)
			assertKey(t, tc.wantEL, "event-ledger-key", got.EventLedgerAPIKey, got.EventLedgerAPIKeyExpiration)
			assert.Equal(t, initTestBaseURL, got.LastBaseURL, "current endpoints are recorded")
			assert.Equal(t, initTestNVCTURL, got.LastNVCTURL)
			assert.Equal(t, initTestEventLedgerURL, got.LastEventLedgerURL)
		})
	}
}

func assertKey(t *testing.T, keep bool, want, got string, exp time.Time) {
	t.Helper()
	if keep {
		assert.Equal(t, want, got)
		assert.False(t, exp.IsZero())
		return
	}
	assert.Empty(t, got)
	assert.True(t, exp.IsZero(), "expiration must be cleared with the key")
}
