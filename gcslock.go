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

// Package gcslock acquires a forward-looking lock leveraging a file in a Google
// Cloud Storage bucket. Unlike a mutex, the lock is TTL-based instead of "held"
// like a traditional mutex.
//
// Compared to other mutexes, this is intended to be a long-lived lock. The
// minimum granularity is "seconds" and most consumers will acquire a lock for
// "minutes" or "hours". Because of clock skew and network latency, granularity
// below 1s is not a supported use case.
package gcslock

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"cloud.google.com/go/storage"
	"github.com/sethvargo/go-retry"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

const (
	// userAgent is the unqiue identifier for upstream API calls.
	userAgent = "gcslock/1.0 (+https://github.com/sethvargo/go-gcslock)"

	// defaultCacheControl is the default value for the Cache-Control header.
	defaultCacheControl = "private, no-cache, no-store, no-transform, max-age=0"

	// defaultChunkSize is the default chunking size. Files are metadata-only, so
	// we intentionally make this very small.
	defaultChunkSize = 1024

	// notBeforeKey is the metadata key where the not-before timestamp is stored.
	notBeforeKey = "nbf"
)

// Lockable is the interface that defines how to manage a lock with Google Cloud
// Storage.
type Lockable interface {
	Acquire(ctx context.Context, ttl time.Duration) error
	Close(ctx context.Context) error
}

var _ error = (*LockHeldError)(nil)

// LockHeldError is a specific error returned when a lock is alread held.
type LockHeldError struct {
	nbf int64
}

// NewLockHeldError creates an instance of a LockHeldError.
func NewLockHeldError(nbf int64) *LockHeldError {
	return &LockHeldError{
		nbf: nbf,
	}
}

// Error implements the error interface.
func (e *LockHeldError) Error() string {
	return "lock held until " + e.NotBefore().Format(time.RFC3339)
}

// NotBefore returns the UTC Unix timestamp of when the lock expires.
func (e *LockHeldError) NotBefore() time.Time {
	return time.Unix(e.nbf, 0).UTC()
}

// Is implements the error comparison interface.
func (e *LockHeldError) Is(err error) bool {
	var terr *LockHeldError
	return errors.As(err, &terr)
}

// Verify that the Lock implements the interface.
var _ Lockable = (*Lock)(nil)

// Lock represents a remote forward-looking lock in Google Cloud Storage.
type Lock struct {
	client *storage.Client
	bucket string
	object string

	retryPolicy retry.Backoff
}

// New creates a new distributed locking handler on the specific object in
// Google Cloud. It does create the lock until Acquire is called.
func New(ctx context.Context, bucket, object string, opts ...option.ClientOption) (*Lock, error) {
	// Append our user agent, but make it first so that subsequent options can
	// override it.
	opts = append([]option.ClientOption{option.WithUserAgent(userAgent)}, opts...)

	// Create the Google Cloud Storage client.
	client, err := storage.NewClient(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create storage client: %w", err)
	}

	// Set a default retry policy. This is for failed API calls, not for failed
	// lock attempts.
	retryPolicy := retry.WithMaxRetries(5, retry.NewFibonacci(50*time.Millisecond))

	return &Lock{
		client:      client,
		bucket:      bucket,
		object:      object,
		retryPolicy: retryPolicy,
	}, nil
}

// Acquire attempts to acquire the lock. It returns [ErrLockHeld] if the lock is
// already held. Callers can cast the error type to get more specific
// information like the TTL expiration time:
//
//	if err := lock.Acquire(ctx, 5*time.Minute); err != nil {
//	  var lockErr *ErrLockHeld
//	  if errors.As(err, &lockErr) {
//	    log.Printf("lock is held until %s", lockErr.NotBefore())
//	  }
//	}
//
// It automatically retries transient upstream API errors, but returns
// immediately for errors that are irrecoverable.
func (l *Lock) Acquire(ctx context.Context, ttl time.Duration) error {
	now := time.Now().UTC()

	if err := retry.Do(ctx, l.retryPolicy, func(ctx context.Context) error {
		return l.tryAcquire(ctx, now, ttl)
	}); err != nil {
		return fmt.Errorf("failed to acquire lock: %w", err)
	}

	return nil
}

// Close terminates the client connection. It does not delete the lock.
func (l *Lock) Close(_ context.Context) error {
	if err := l.client.Close(); err != nil {
		return fmt.Errorf("failed to close storage client: %w", err)
	}
	return nil
}

// tryAcquire is the internal implementation of [Acquire] that actually creates
// and updates the lock.
func (l *Lock) tryAcquire(ctx context.Context, now time.Time, ttl time.Duration) error {
	now = now.Truncate(time.Second)
	ttl = ttl.Truncate(time.Second)
	objHandle := l.client.Bucket(l.bucket).Object(l.object)

	// Try to get the attributes on the object.
	attrs, err := objHandle.Attrs(ctx)
	if err != nil && !errors.Is(err, storage.ErrObjectNotExist) {
		return fmt.Errorf("failed to get storage object: %w", err)
	}

	// If we found the object, check if the lock is valid and held.
	if attrs != nil && attrs.Metadata != nil {
		nbf, ok := attrs.Metadata[notBeforeKey]
		if !ok {
			nbf = "0"
		}

		nbfUnix, err := strconv.ParseInt(nbf, 10, 64)
		if err != nil {
			return fmt.Errorf("failed to parse nbf as an integer: %w", err)
		}

		if nbfUnix >= now.Unix() {
			return NewLockHeldError(nbfUnix)
		}
	}

	// If we got this far, it means the lock object either does not exist, or it
	// exists but is past the TTL. Therefore we need to create/update the file,
	// taking into account the possibility that competing processes are fighting
	// for the lock.

	// If generation and metageneration are 0, then we should only create the
	// object if it does not exist. Otherwise, we should only perform an update if
	// the metagenerations match.
	var conds storage.Conditions
	if attrs == nil {
		// The object did not exist, so ensure it does not exist when we write.
		conds = storage.Conditions{
			DoesNotExist: true,
		}
	} else {
		// The object exists, so set metadata to ensure we catch a race.
		conds = storage.Conditions{
			GenerationMatch:     attrs.Generation,
			MetagenerationMatch: attrs.Metageneration,
		}
	}

	w := objHandle.If(conds).NewWriter(ctx)
	w.CacheControl = defaultCacheControl
	w.ChunkSize = defaultChunkSize
	w.SendCRC32C = true
	if w.Metadata == nil {
		w.Metadata = make(map[string]string)
	}
	w.Metadata[notBeforeKey] = strconv.FormatInt(now.Add(ttl).Unix(), 10)

	// Write the metadata back to the object.
	if err := w.Close(); err != nil {
		var googleErr *googleapi.Error
		if errors.As(err, &googleErr) {
			switch googleErr.Code {
			case http.StatusNotFound:
				// The object was deleted between when we read attributes and now.
				return retry.RetryableError(err)
			case http.StatusPreconditionFailed:
				// The object was modified between when we read attributes and now.
				return retry.RetryableError(err)
			}
		}

		return fmt.Errorf("failed to update object: %w", err)
	}

	return nil
}
