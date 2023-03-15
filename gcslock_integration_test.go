// Copyright 2023 The Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package gcslock_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"github.com/sethvargo/go-gcslock"
)

func TestGCSLock_Acquire(t *testing.T) {
	t.Parallel()

	testBucket := os.Getenv("TEST_BUCKET")
	if testBucket == "" {
		t.Skip("missing $TEST_BUCKET")
	}
	testObject := "gcslock_test_" + randomString(t)

	ctx := context.Background()

	// Always delete the lock at the end of the suite
	t.Cleanup(func() {
		client, err := storage.NewClient(ctx)
		if err != nil {
			t.Fatal(err)
		}

		if err := client.
			Bucket(testBucket).
			Object(testObject).
			Delete(ctx); err != nil && !errors.Is(err, storage.ErrObjectNotExist) {
			t.Fatal(err)
		}
	})

	lock, err := gcslock.New(ctx, testBucket, testObject)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lock.Close(ctx); err != nil {
			t.Fatal(err)
		}
	})

	// Acquire initial lock.
	if err := lock.Acquire(ctx, 1*time.Second); err != nil {
		t.Fatal(err)
	}

	// Wait for the lock to expire with a buffer.
	time.Sleep(2 * time.Second)

	// Acquiring the lock again should succeed, since it's expired.
	if err := lock.Acquire(ctx, 1*time.Second); err != nil {
		t.Fatal(err)
	}

	// Immediately try to acquire the lock, which should still be held.
	if err := lock.Acquire(ctx, 1*time.Second); err != nil {
		var terr *gcslock.LockHeldError
		if !errors.As(err, &terr) {
			t.Fatalf("expected %s (%T) to be %T", err, err, terr)
		}
	} else {
		t.Errorf("expected error, got nothing")
	}

	// Wait for the lock to expire with a buffer.
	time.Sleep(2 * time.Second)

	// Attempt to acquire the lock in parallel. All but one should fail.
	var wg sync.WaitGroup
	var numSuccess int64
	var numRejected int64
	iters := 3
	errCh := make(chan error, iters)
	for i := 0; i < iters; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			if err := lock.Acquire(ctx, 5*time.Second); err != nil {
				var terr *gcslock.LockHeldError
				if errors.As(err, &terr) {
					atomic.AddInt64(&numRejected, 1)
				} else {
					errCh <- err
				}
			} else {
				atomic.AddInt64(&numSuccess, 1)
			}
		}()
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}

	if got, want := numSuccess, int64(1); got != want {
		t.Errorf("expected %d to be %d", got, want)
	}
	if got, want := numRejected, int64(2); got != want {
		t.Errorf("expected %d to be %d", got, want)
	}
}

func randomString(tb testing.TB) string {
	tb.Helper()

	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		tb.Fatal(err)
	}
	return hex.EncodeToString(b)
}
