/*
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
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

package worker

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/dsx-ai-factory/nvcf/src/libraries/go/worker/consumer"
)

func TestNewNatsConsumerWithRetry(t *testing.T) {
	errNatsDown := errors.New("context deadline exceeded")

	t.Run("recovers after transient failures", func(t *testing.T) {
		want := &consumer.GlobalNatsConsumer{}
		attempts := 0
		got, err := newNatsConsumerWithRetry(context.Background(), &backoff.ZeroBackOff{}, func() (*consumer.GlobalNatsConsumer, error) {
			attempts++
			if attempts < 3 {
				return nil, errNatsDown
			}
			return want, nil
		})
		require.NoError(t, err)
		assert.Same(t, want, got)
		assert.Equal(t, 3, attempts)
	})

	t.Run("returns the last error when the backoff stops", func(t *testing.T) {
		attempts := 0
		_, err := newNatsConsumerWithRetry(context.Background(), backoff.WithMaxRetries(&backoff.ZeroBackOff{}, 2), func() (*consumer.GlobalNatsConsumer, error) {
			attempts++
			return nil, errNatsDown
		})
		require.ErrorIs(t, err, errNatsDown)
		assert.Equal(t, 3, attempts)
	})

	t.Run("keeps retrying through a long outage until NATS is back", func(t *testing.T) {
		attempts := 0
		got, err := newNatsConsumerWithRetry(context.Background(), &backoff.ZeroBackOff{}, func() (*consumer.GlobalNatsConsumer, error) {
			attempts++
			if attempts < 500 {
				return nil, errNatsDown
			}
			return &consumer.GlobalNatsConsumer{}, nil
		})
		require.NoError(t, err)
		assert.NotNil(t, got)
		assert.Equal(t, 500, attempts)
	})

	t.Run("stops when context is cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		attempts := 0
		_, err := newNatsConsumerWithRetry(ctx, &backoff.ZeroBackOff{}, func() (*consumer.GlobalNatsConsumer, error) {
			attempts++
			cancel()
			return nil, errNatsDown
		})
		require.Error(t, err)
		assert.Equal(t, 1, attempts)
	})

	t.Run("does not retry errors that retrying cannot fix", func(t *testing.T) {
		for _, code := range []codes.Code{codes.Unauthenticated, codes.PermissionDenied, codes.InvalidArgument} {
			t.Run(code.String(), func(t *testing.T) {
				rejected := fmt.Errorf("failed to create consumer for primary region ncp: %w", status.Error(code, "rejected"))
				attempts := 0
				_, err := newNatsConsumerWithRetry(context.Background(), &backoff.ZeroBackOff{}, func() (*consumer.GlobalNatsConsumer, error) {
					attempts++
					return nil, rejected
				})
				require.ErrorIs(t, err, rejected)
				assert.Equal(t, 1, attempts)
			})
		}
	})

	t.Run("retry warning includes the caller's log fields", func(t *testing.T) {
		core, logs := observer.New(zap.WarnLevel)
		t.Cleanup(zap.ReplaceGlobals(zap.New(core)))
		attempts := 0
		_, err := newNatsConsumerWithRetry(context.Background(), &backoff.ZeroBackOff{}, func() (*consumer.GlobalNatsConsumer, error) {
			attempts++
			if attempts < 2 {
				return nil, errNatsDown
			}
			return &consumer.GlobalNatsConsumer{}, nil
		}, zap.String("function_id", "fn-1"))
		require.NoError(t, err)
		entries := logs.FilterMessage("failed to set up nats consumer, retrying").All()
		require.Len(t, entries, 1)
		assert.Equal(t, "fn-1", entries[0].ContextMap()["function_id"])
		assert.EqualValues(t, 1, entries[0].ContextMap()["failedAttempts"])
	})

	t.Run("retries unavailable provisioning", func(t *testing.T) {
		attempts := 0
		_, err := newNatsConsumerWithRetry(context.Background(), &backoff.ZeroBackOff{}, func() (*consumer.GlobalNatsConsumer, error) {
			attempts++
			if attempts < 2 {
				return nil, status.Error(codes.Unavailable, "connection refused")
			}
			return &consumer.GlobalNatsConsumer{}, nil
		})
		require.NoError(t, err)
		assert.Equal(t, 2, attempts)
	})

	t.Run("logs recovery after retrying", func(t *testing.T) {
		core, logs := observer.New(zap.InfoLevel)
		t.Cleanup(zap.ReplaceGlobals(zap.New(core)))
		attempts := 0
		_, err := newNatsConsumerWithRetry(context.Background(), &backoff.ZeroBackOff{}, func() (*consumer.GlobalNatsConsumer, error) {
			attempts++
			if attempts < 3 {
				return nil, errNatsDown
			}
			return &consumer.GlobalNatsConsumer{}, nil
		}, zap.String("function_id", "fn-1"))
		require.NoError(t, err)
		entries := logs.FilterMessage("set up nats consumer after retrying").All()
		require.Len(t, entries, 1)
		assert.EqualValues(t, 2, entries[0].ContextMap()["failedAttempts"])
		assert.Equal(t, "fn-1", entries[0].ContextMap()["function_id"])
	})

	t.Run("does not log recovery when the first attempt succeeds", func(t *testing.T) {
		core, logs := observer.New(zap.InfoLevel)
		t.Cleanup(zap.ReplaceGlobals(zap.New(core)))
		_, err := newNatsConsumerWithRetry(context.Background(), &backoff.ZeroBackOff{}, func() (*consumer.GlobalNatsConsumer, error) {
			return &consumer.GlobalNatsConsumer{}, nil
		})
		require.NoError(t, err)
		assert.Empty(t, logs.FilterMessage("set up nats consumer after retrying").All())
	})
}

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

func TestNatsConsumerSetupBackOff(t *testing.T) {
	clock := &fakeClock{now: time.Unix(0, 0)}
	b := newNatsConsumerSetupBackOffWithClock(clock)
	now := clock.now
	advance := func(d time.Duration) {
		now = now.Add(d)
		clock.now = now
	}

	maxFast := natsConsumerFastRetryMaxWait + natsConsumerFastRetryMaxWait/4
	for now.Sub(time.Unix(0, 0)) < natsConsumerFastRetryPeriod {
		wait := b.NextBackOff()
		require.NotEqual(t, backoff.Stop, wait)
		require.LessOrEqual(t, wait, maxFast, "fast phase at %s", now.Sub(time.Unix(0, 0)))
		advance(wait)
	}

	maxSlow := natsConsumerSlowRetryMaxWait + natsConsumerSlowRetryMaxWait/4
	sawSlow := false
	for i := 0; i < 1000; i++ {
		wait := b.NextBackOff()
		require.NotEqual(t, backoff.Stop, wait, "the backoff must never stop on its own")
		require.LessOrEqual(t, wait, maxSlow)
		sawSlow = sawSlow || wait > maxFast
		advance(wait)
	}
	assert.True(t, sawSlow, "waits grow beyond the fast limit after the fast period")
	assert.Greater(t, now.Sub(time.Unix(0, 0)), 6*time.Hour)

}

func TestPhasedBackOffReset(t *testing.T) {
	now := time.Unix(0, 0)
	b := &phasedBackOff{
		fast:    backoff.NewConstantBackOff(time.Millisecond),
		slow:    backoff.NewConstantBackOff(time.Second),
		fastFor: time.Minute,
		now:     func() time.Time { return now },
	}
	assert.Equal(t, time.Millisecond, b.NextBackOff())
	now = now.Add(2 * time.Minute)
	assert.Equal(t, time.Second, b.NextBackOff())

	b.Reset()
	assert.Equal(t, time.Millisecond, b.NextBackOff(), "Reset starts over with fast retries")
	now = now.Add(2 * time.Minute)
	assert.Equal(t, time.Second, b.NextBackOff())
}

func TestCheckNatsConsumerSetup(t *testing.T) {
	w := &NVCFWorker{}
	require.NoError(t, w.checkNatsConsumerSetup(context.Background()))

	w.natsConsumerSetupFailing.Store(true)
	require.Error(t, w.checkNatsConsumerSetup(context.Background()))

	w.natsConsumerSetupFailing.Store(false)
	require.NoError(t, w.checkNatsConsumerSetup(context.Background()))
}
