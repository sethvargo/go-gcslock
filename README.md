[![GoDoc](https://img.shields.io/badge/go-documentation-blue.svg?style=flat-square)][godoc]

# go-gcslock

GCS Lock creates a forward-looking lock using Google Cloud Storage. The lock is
forward-looking because it is not actually "held" like a mutex. This is useful
in situations where you want something to occur at most once over a duration,
but multiple independent processes could trigger the event.

"Identity" is tied to the object itself; different locks should use different
object names.


## Usage

```go
lock, err := gcslock.New(ctx, "my-bucket", "my-object")
if err != nil {
  panic(err) // TODO: handle error
}
defer lock.Close(ctx)

// Acquire the lock for 30 minutes. Upon successful return from this method,
// the lock is held.
if err := lock.Acquire(ctx, 30*time.Minute); err != nil {
  var lockErr *ErrLockHeld
  if errors.As(err, &lockErr) {
    // Lock is already held
    log.Printf("lock is held until %s", lockErr.NotBefore())
  }

  panic(err) // TODO: handle error
}
```


[godoc]: https://pkg.go.dev/github.com/sethvargo/go-gcslock
