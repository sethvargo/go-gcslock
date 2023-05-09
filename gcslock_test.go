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

package gcslock

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fsouza/fake-gcs-server/fakestorage"
)

func TestLockHeldError_Error(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  *LockHeldError
		exp  string
	}{
		{
			name: "zero",
			err:  NewLockHeldError(0),
			exp:  "lock held until 1970-01-01T00:00:00Z",
		},
		{
			name: "timestamp",
			err:  NewLockHeldError(1902902494),
			exp:  "lock held until 2030-04-20T08:01:34Z",
		},
	}

	for _, tc := range cases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got, want := tc.err.Error(), tc.exp; got != want {
				t.Errorf("expected %q to be %q", got, want)
			}
		})
	}
}

func TestLockHeldError_NotBefore(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  *LockHeldError
		exp  string
	}{
		{
			name: "zero",
			err:  NewLockHeldError(0),
			exp:  "1970-01-01T00:00:00Z",
		},
		{
			name: "timestamp",
			err:  NewLockHeldError(1902902494),
			exp:  "2030-04-20T08:01:34Z",
		},
	}

	for _, tc := range cases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got, want := tc.err.NotBefore().Format(time.RFC3339), tc.exp; got != want {
				t.Errorf("expected %q to be %q", got, want)
			}
		})
	}
}

func TestNewGCSLock(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	lock, err := New(ctx, "bucket", "object")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lock.Close(ctx); err != nil {
			t.Fatal(err)
		}
	})

	if got := lock.client; got == nil {
		t.Errorf("exected client to be defined")
	}
	if got, want := lock.bucket, "bucket"; got != want {
		t.Errorf("exected %q to be %q", got, want)
	}
	if got, want := lock.object, "object"; got != want {
		t.Errorf("exected %q to be %q", got, want)
	}
	if got := lock.retryPolicy; got == nil {
		t.Errorf("exected retryPolicy to be defined")
	}
}

func TestGCSLock_Acquire(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	now := time.Unix(1902902494, 0).Truncate(time.Second).UTC()
	ttl := 5 * time.Minute

	cases := []struct {
		name        string
		gcsState    func() *fakestorage.Server
		expectedNbf time.Time
		err         string
	}{
		{
			name: "bucket_no_exist",
			gcsState: func() *fakestorage.Server {
				return fakestorage.NewServer(nil)
			},
			err: "Not Found",
		},
		{
			name: "object_no_exist",
			gcsState: func() *fakestorage.Server {
				srv := fakestorage.NewServer(nil)
				if err := srv.Client().Bucket("my-bucket").Create(ctx, "my-project", nil); err != nil {
					t.Fatal(err)
				}
				return srv
			},
			expectedNbf: now.Add(ttl),
		},
		{
			name: "lock_exists_not_expired",
			gcsState: func() *fakestorage.Server {
				srv := fakestorage.NewServer(nil)
				if err := srv.Client().Bucket("my-bucket").Create(ctx, "my-project", nil); err != nil {
					t.Fatal(err)
				}

				w := srv.Client().Bucket("my-bucket").Object("my-object").NewWriter(ctx)
				w.Metadata = map[string]string{
					notBeforeKey: strconv.FormatInt(now.Add(ttl/2).Unix(), 10),
				}

				if err := w.Close(); err != nil {
					t.Fatal(err)
				}

				return srv
			},
			err: "lock held until 2030-04-20T08:04:04Z",
		},
		{
			name: "lock_exists_expired",
			gcsState: func() *fakestorage.Server {
				srv := fakestorage.NewServer(nil)
				if err := srv.Client().Bucket("my-bucket").Create(ctx, "my-project", nil); err != nil {
					t.Fatal(err)
				}

				w := srv.Client().Bucket("my-bucket").Object("my-object").NewWriter(ctx)
				w.Metadata = map[string]string{
					notBeforeKey: strconv.FormatInt(now.Add(-ttl).Unix(), 10),
				}

				if err := w.Close(); err != nil {
					t.Fatal(err)
				}

				return srv
			},
			expectedNbf: now.Add(ttl),
		},
	}

	for _, tc := range cases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			gcsServer := tc.gcsState()

			lock, err := New(ctx, "my-bucket", "my-object")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := lock.Close(ctx); err != nil {
					t.Fatal(err)
				}
			})

			lock.client = gcsServer.Client()

			if err := lock.tryAcquire(ctx, now, ttl); err != nil {
				if tc.err == "" {
					t.Fatal(err)
				} else {
					if got, want := err.Error(), tc.err; !strings.Contains(got, want) {
						t.Errorf("expected %q to contain %q", got, want)
					}
				}
			} else if tc.err != "" {
				t.Fatalf("expected error %q, got nothing", tc.err)
			}

			if !tc.expectedNbf.IsZero() {
				attrs, err := gcsServer.Client().
					Bucket("my-bucket").
					Object("my-object").
					Attrs(ctx)
				if err != nil {
					t.Fatal(err)
				}

				if got, want := timeFromUnixString(t, attrs.Metadata[notBeforeKey]), tc.expectedNbf; got != want {
					t.Errorf("expected %q to be %q", got, want)
				}
			}
		})
	}
}

func timeFromUnixString(tb testing.TB, s string) time.Time {
	tb.Helper()

	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		tb.Fatal(err)
	}

	return time.Unix(v, 0).UTC()
}
