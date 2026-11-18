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
	initTestBaseURL   = "http://api.test.local:9999"
	initTestInvokeURL = "http://invoke.test.local:9999"
)

// seedInitState installs a fresh state manager holding an admin token, both
// API keys, a selected function, and the given endpoints, then points the
// API keys service at a fake server that mints admin tokens.
func seedInitState(t *testing.T, baseURL string) {
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
	t.Cleanup(func() { viper.Reset() })
	t.Setenv("api_keys_service_url", srv.URL)

	orig := state.DefaultStateManager
	sm := state.NewStateManager()
	state.DefaultStateManager = sm
	t.Cleanup(func() { state.DefaultStateManager = orig })

	exp := time.Now().Add(time.Hour)
	sm.SetTokens("old-admin-token", "function-key", exp, exp)
	sm.SetNVCTAPIKey("task-key", exp)
	sm.SetFunction("fn-id", "ver-id", "fn")
	sm.SetEndpoints(baseURL, initTestInvokeURL, "nvcf-default")
	require.NoError(t, sm.Save())
}

func TestInit_KeepsAPIKeysForSameEndpoints(t *testing.T) {
	seedInitState(t, initTestBaseURL)

	require.NoError(t, runInit(nil, nil))

	got := state.DefaultStateManager.GetState()
	assert.Equal(t, "new-admin-token", got.Token, "admin token must be replaced")
	assert.Equal(t, "function-key", got.APIKey, "function API key must survive init")
	assert.Equal(t, "task-key", got.NVCTAPIKey, "task API key must survive init")
	assert.False(t, got.APIKeyExpiration.IsZero())
	assert.False(t, got.NVCTAPIKeyExpiration.IsZero())
	assert.Empty(t, got.FunctionID, "function context is still reset")
}

func TestInit_DropsAPIKeysWhenEndpointsChange(t *testing.T) {
	seedInitState(t, "http://other-cluster.test.local:9999")

	require.NoError(t, runInit(nil, nil))

	got := state.DefaultStateManager.GetState()
	assert.Equal(t, "new-admin-token", got.Token)
	assert.Empty(t, got.APIKey, "keys from another deployment must not be reused")
	assert.Empty(t, got.NVCTAPIKey)
	assert.Equal(t, initTestBaseURL, got.LastBaseURL)
}
