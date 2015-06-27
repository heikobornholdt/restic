package restic

import (
	"errors"
	"os"
	"os/signal"
	"os/user"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/restic/restic/backend"
	"github.com/restic/restic/debug"
	"github.com/restic/restic/repository"
)

// Lock represents a process locking the repository for an operation.
//
// There are two types of locks: exclusive and non-exclusive. There may be many
// different non-exclusive locks, but at most one exclusive lock, which can
// only be acquired while no non-exclusive lock is held.
type Lock struct {
	Time      time.Time `json:"time"`
	Exclusive bool      `json:"exclusive"`
	Hostname  string    `json:"hostname"`
	Username  string    `json:"username"`
	PID       int       `json:"pid"`
	UID       uint32    `json:"uid,omitempty"`
	GID       uint32    `json:"gid,omitempty"`

	repo   *repository.Repository
	lockID backend.ID
}

var (
	ErrAlreadyLocked  = errors.New("already locked")
	ErrStaleLockFound = errors.New("stale lock found")
)

// NewLock returns a new, non-exclusive lock for the repository. If an
// exclusive lock is already held by another process, ErrAlreadyLocked is
// returned.
func NewLock(repo *repository.Repository) (*Lock, error) {
	return newLock(repo, false)
}

// NewExclusiveLock returns a new, exclusive lock for the repository. If
// another lock (normal and exclusive) is already held by another process,
// ErrAlreadyLocked is returned.
func NewExclusiveLock(repo *repository.Repository) (*Lock, error) {
	return newLock(repo, true)
}

const waitBeforeLockCheck = 200 * time.Millisecond

func newLock(repo *repository.Repository, excl bool) (*Lock, error) {
	lock := &Lock{
		Time:      time.Now(),
		PID:       os.Getpid(),
		Exclusive: excl,
		repo:      repo,
	}

	hn, err := os.Hostname()
	if err == nil {
		lock.Hostname = hn
	}

	if err = lock.fillUserInfo(); err != nil {
		return nil, err
	}

	if err = lock.checkForOtherLocks(); err != nil {
		return nil, err
	}

	err = lock.createLock()
	if err != nil {
		return nil, err
	}

	time.Sleep(waitBeforeLockCheck)

	if err = lock.checkForOtherLocks(); err != nil {
		lock.Unlock()
		return nil, ErrAlreadyLocked
	}

	return lock, nil
}

func (l *Lock) fillUserInfo() error {
	usr, err := user.Current()
	if err != nil {
		return nil
	}
	l.Username = usr.Username

	uid, err := strconv.ParseInt(usr.Uid, 10, 32)
	if err != nil {
		return err
	}
	l.UID = uint32(uid)

	gid, err := strconv.ParseInt(usr.Gid, 10, 32)
	if err != nil {
		return err
	}
	l.GID = uint32(gid)

	return nil
}

// checkForOtherLocks looks for other locks that currently exist in the repository.
//
// If an exclusive lock is to be created, checkForOtherLocks returns an error
// if there are any other locks, regardless if exclusive or not. If a
// non-exclusive lock is to be created, an error is only returned when an
// exclusive lock is found.
func (l *Lock) checkForOtherLocks() error {
	return eachLock(l.repo, func(id backend.ID, lock *Lock, err error) error {
		if id.Equal(l.lockID) {
			return nil
		}

		// ignore locks that cannot be loaded
		if err != nil {
			return nil
		}

		if l.Exclusive {
			return ErrAlreadyLocked
		}

		if !l.Exclusive && lock.Exclusive {
			return ErrAlreadyLocked
		}

		return nil
	})
}

func eachLock(repo *repository.Repository, f func(backend.ID, *Lock, error) error) error {
	done := make(chan struct{})
	defer close(done)

	for id := range repo.List(backend.Lock, done) {
		lock, err := LoadLock(repo, id)
		err = f(id, lock, err)
		if err != nil {
			return err
		}
	}

	return nil
}

// createLock acquires the lock by creating a file in the repository.
func (l *Lock) createLock() error {
	id, err := l.repo.SaveJSONUnpacked(backend.Lock, l)
	if err != nil {
		return err
	}

	l.lockID = id
	return nil
}

// Unlock removes the lock from the repository.
func (l *Lock) Unlock() error {
	if l == nil || l.lockID == nil {
		return nil
	}

	return l.repo.Backend().Remove(backend.Lock, l.lockID.String())
}

var staleTimeout = 30 * time.Minute

// Stale returns true if the lock is stale. A lock is stale if the timestamp is
// older than 30 minutes or if it was created on the current machine and the
// process isn't alive any more.
func (l *Lock) Stale() bool {
	debug.Log("Lock.Stale", "testing if lock %v for process %d is stale", l.lockID.Str(), l.PID)
	if time.Now().Sub(l.Time) > staleTimeout {
		debug.Log("Lock.Stale", "lock is stale, timestamp is too old: %v\n", l.Time)
		return true
	}

	proc, err := os.FindProcess(l.PID)
	defer proc.Release()
	if err != nil {
		debug.Log("Lock.Stale", "error searching for process %d: %v\n", l.PID, err)
		return true
	}

	debug.Log("Lock.Stale", "sending SIGHUP to process %d\n", l.PID)
	err = proc.Signal(syscall.SIGHUP)
	if err != nil {
		debug.Log("Lock.Stale", "signal error: %v, lock is probably stale\n", err)
		return true
	}

	debug.Log("Lock.Stale", "lock not stale\n")
	return false
}

// listen for incoming SIGHUP and ignore
var ignoreSIGHUP sync.Once

func init() {
	ignoreSIGHUP.Do(func() {
		go func() {
			c := make(chan os.Signal)
			signal.Notify(c, syscall.SIGHUP)
			for s := range c {
				debug.Log("lock.ignoreSIGHUP", "Signal received: %v\n", s)
			}
		}()
	})
}

// LoadLock loads and unserializes a lock from a repository.
func LoadLock(repo *repository.Repository, id backend.ID) (*Lock, error) {
	lock := &Lock{}
	if err := repo.LoadJSONUnpacked(backend.Lock, id, lock); err != nil {
		return nil, err
	}
	lock.lockID = id

	return lock, nil
}

// RemoveStaleLocks deletes all locks detected as stale from the repository.
func RemoveStaleLocks(repo *repository.Repository) error {
	return eachLock(repo, func(id backend.ID, lock *Lock, err error) error {
		// ignore locks that cannot be loaded
		if err != nil {
			return nil
		}

		if lock.Stale() {
			return repo.Backend().Remove(backend.Lock, id.String())
		}

		return nil
	})
}
