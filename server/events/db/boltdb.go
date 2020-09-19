// Package db handles our database layer.
package db

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/runatlantis/atlantis/server/events/models"
	bolt "go.etcd.io/bbolt"
)

// BoltDB is a database using BoltDB
type BoltDB struct {
	db              *bolt.DB
	locksBucketName []byte
	pullsBucketName []byte
}

const (
	locksBucketName  = "runLocks"
	pullsBucketName  = "pulls"
	pullKeySeparator = "::"
)

// New returns a valid locker. We need to be able to write to dataDir
// since bolt stores its data as a file
func New(dataDir string) (*BoltDB, error) {
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, errors.Wrap(err, "creating data dir")
	}
	db, err := bolt.Open(path.Join(dataDir, "atlantis.db"), 0600, &bolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		if err.Error() == "timeout" {
			return nil, errors.New("starting BoltDB: timeout (a possible cause is another Atlantis instance already running)")
		}
		return nil, errors.Wrap(err, "starting BoltDB")
	}

	// Create the buckets.
	err = db.Update(func(tx *bolt.Tx) error {
		if _, err = tx.CreateBucketIfNotExists([]byte(locksBucketName)); err != nil {
			return errors.Wrapf(err, "creating bucket %q", locksBucketName)
		}
		if _, err = tx.CreateBucketIfNotExists([]byte(pullsBucketName)); err != nil {
			return errors.Wrapf(err, "creating bucket %q", pullsBucketName)
		}
		return nil
	})
	if err != nil {
		return nil, errors.Wrap(err, "starting BoltDB")
	}
	// todo: close BoltDB when server is sigtermed
	return &BoltDB{db: db, locksBucketName: []byte(locksBucketName), pullsBucketName: []byte(pullsBucketName)}, nil
}

// NewWithDB is used for testing.
func NewWithDB(db *bolt.DB, bucket string) (*BoltDB, error) {
	return &BoltDB{db: db, locksBucketName: []byte(bucket), pullsBucketName: []byte(pullsBucketName)}, nil
}

// TryLock attempts to create a new lock. If the lock is
// acquired, it will return true and the lock returned will be newLock.
// If the lock is not acquired, it will return false and the current
// lock that is preventing this lock from being acquired.
func (b *BoltDB) TryLock(newLock models.ProjectLock) (bool, models.ProjectLock, error) {
	var lockAcquired bool
	var currLock models.ProjectLock
	key := b.lockKey(newLock.Project, newLock.Workspace)
	newLockSerialized, _ := json.Marshal(newLock)
	transactionErr := b.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(b.locksBucketName)

		// if there is no run at that key then we're free to create the lock
		currLockSerialized := bucket.Get([]byte(key))
		if currLockSerialized == nil {
			// This will only error on readonly buckets, it's okay to ignore.
			bucket.Put([]byte(key), newLockSerialized) // nolint: errcheck
			lockAcquired = true
			currLock = newLock
			return nil
		}

		// otherwise the lock fails, return to caller the run that's holding the lock
		if err := json.Unmarshal(currLockSerialized, &currLock); err != nil {
			return errors.Wrap(err, "failed to deserialize current lock")
		}
		lockAcquired = false
		return nil
	})

	if transactionErr != nil {
		return false, currLock, errors.Wrap(transactionErr, "DB transaction failed")
	}

	return lockAcquired, currLock, nil
}

// Unlock attempts to unlock the project and workspace.
// If there is no lock, then it will return a nil pointer.
// If there is a lock, then it will delete it, and then return a pointer
// to the deleted lock.
func (b *BoltDB) Unlock(p models.Project, workspace string) (*models.ProjectLock, error) {
	var lock models.ProjectLock
	foundLock := false
	key := b.lockKey(p, workspace)
	err := b.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(b.locksBucketName)
		serialized := bucket.Get([]byte(key))
		if serialized != nil {
			if err := json.Unmarshal(serialized, &lock); err != nil {
				return errors.Wrap(err, "failed to deserialize lock")
			}
			foundLock = true
		}
		return bucket.Delete([]byte(key))
	})
	err = errors.Wrap(err, "DB transaction failed")
	if foundLock {
		return &lock, err
	}
	return nil, err
}

// List lists all current locks.
func (b *BoltDB) List() ([]models.ProjectLock, error) {
	var locks []models.ProjectLock
	var locksBytes [][]byte
	err := b.db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(b.locksBucketName)
		c := bucket.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			locksBytes = append(locksBytes, v)
		}
		return nil
	})
	if err != nil {
		return locks, errors.Wrap(err, "DB transaction failed")
	}

	// deserialize bytes into the proper objects
	for k, v := range locksBytes {
		var lock models.ProjectLock
		if err := json.Unmarshal(v, &lock); err != nil {
			return locks, errors.Wrap(err, fmt.Sprintf("failed to deserialize lock at key %q", string(k)))
		}
		locks = append(locks, lock)
	}

	return locks, nil
}

// UnlockByPull deletes all locks associated with that pull request and returns them.
func (b *BoltDB) UnlockByPull(repoFullName string, pullNum int) ([]models.ProjectLock, error) {
	var locks []models.ProjectLock
	err := b.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(b.locksBucketName).Cursor()

		// we can use the repoFullName as a prefix search since that's the first part of the key
		for k, v := c.Seek([]byte(repoFullName)); k != nil && bytes.HasPrefix(k, []byte(repoFullName)); k, v = c.Next() {
			var lock models.ProjectLock
			if err := json.Unmarshal(v, &lock); err != nil {
				return errors.Wrapf(err, "deserializing lock at key %q", string(k))
			}
			if lock.Pull.Num == pullNum {
				locks = append(locks, lock)
			}
		}
		return nil
	})
	if err != nil {
		return locks, err
	}

	// delete the locks
	for _, lock := range locks {
		if _, err = b.Unlock(lock.Project, lock.Workspace); err != nil {
			return locks, errors.Wrapf(err, "unlocking repo %s, path %s, workspace %s", lock.Project.RepoFullName, lock.Project.Path, lock.Workspace)
		}
	}
	return locks, nil
}

// GetLock returns a pointer to the lock for that project and workspace.
// If there is no lock, it returns a nil pointer.
func (b *BoltDB) GetLock(p models.Project, workspace string) (*models.ProjectLock, error) {
	key := b.lockKey(p, workspace)
	var lockBytes []byte
	err := b.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(b.locksBucketName)
		lockBytes = b.Get([]byte(key))
		return nil
	})
	if err != nil {
		return nil, errors.Wrap(err, "getting lock data")
	}
	// lockBytes will be nil if there was no data at that key
	if lockBytes == nil {
		return nil, nil
	}

	var lock models.ProjectLock
	if err := json.Unmarshal(lockBytes, &lock); err != nil {
		return nil, errors.Wrapf(err, "deserializing lock at key %q", key)
	}

	// need to set it to Local after deserialization due to https://github.com/golang/go/issues/19486
	lock.Time = lock.Time.Local()
	return &lock, nil
}

// UpdatePullWithResults updates pull's status with the latest project results.
// It returns the new PullStatus object.
func (b *BoltDB) UpdatePullWithResults(pull models.PullRequest, newResults []models.ProjectResult) (models.PullStatus, error) {
	key, err := b.pullKey(pull)
	if err != nil {
		return models.PullStatus{}, err
	}

	var newStatus models.PullStatus
	err = b.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(b.pullsBucketName)
		currStatus, err := b.getPullFromBucket(bucket, key)
		if err != nil {
			return err
		}

		// If there is no pull OR if the pull we have is out of date, we
		// just write a new pull.
		if currStatus == nil || currStatus.Pull.HeadCommit != pull.HeadCommit {
			var statuses []models.ProjectStatus
			for _, r := range newResults {
				statuses = append(statuses, b.projectResultToProject(r))
			}
			newStatus = models.PullStatus{
				Pull:     pull,
				Projects: statuses,
			}
		} else {
			// If there's an existing pull at the right commit then we have to
			// merge our project results with the existing ones. We do a merge
			// because it's possible a user is just applying a single project
			// in this command and so we don't want to delete our data about
			// other projects that aren't affected by this command.
			newStatus = *currStatus
			for _, res := range newResults {
				// First, check if we should update any existing projects.
				updatedExisting := false
				for i := range newStatus.Projects {
					// NOTE: We're using a reference here because we are
					// in-place updating its Status field.
					proj := &newStatus.Projects[i]
					if res.Workspace == proj.Workspace &&
						res.RepoRelDir == proj.RepoRelDir &&
						res.ProjectName == proj.ProjectName {

						proj.Status = res.PlanStatus()
						updatedExisting = true
						break
					}
				}

				if !updatedExisting {
					// If we didn't update an existing project, then we need to
					// add this because it's a new one.
					newStatus.Projects = append(newStatus.Projects, b.projectResultToProject(res))
				}
			}
		}

		// Now, we overwrite the key with our new status.
		return b.writePullToBucket(bucket, key, newStatus)
	})
	return newStatus, errors.Wrap(err, "DB transaction failed")
}

// GetPullStatus returns the status for pull.
// If there is no status, returns a nil pointer.
func (b *BoltDB) GetPullStatus(pull models.PullRequest) (*models.PullStatus, error) {
	key, err := b.pullKey(pull)
	if err != nil {
		return nil, err
	}
	var s *models.PullStatus
	err = b.db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(b.pullsBucketName)
		var txErr error
		s, txErr = b.getPullFromBucket(bucket, key)
		return txErr
	})
	return s, errors.Wrap(err, "DB transaction failed")
}

// DeletePullStatus deletes the status for pull.
func (b *BoltDB) DeletePullStatus(pull models.PullRequest) error {
	key, err := b.pullKey(pull)
	if err != nil {
		return err
	}
	err = b.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(b.pullsBucketName)
		return bucket.Delete(key)
	})
	return errors.Wrap(err, "DB transaction failed")
}

// DeleteProjectStatus deletes all project statuses under pull that match
// workspace and repoRelDir.
func (b *BoltDB) DeleteProjectStatus(pull models.PullRequest, workspace string, repoRelDir string) error {
	key, err := b.pullKey(pull)
	if err != nil {
		return err
	}
	err = b.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(b.pullsBucketName)
		currStatusPtr, err := b.getPullFromBucket(bucket, key)
		if err != nil {
			return err
		}
		if currStatusPtr == nil {
			return nil
		}
		currStatus := *currStatusPtr

		// Create a new projectStatuses array without the ones we want to
		// delete.
		var newProjects []models.ProjectStatus
		for _, p := range currStatus.Projects {
			if p.Workspace == workspace && p.RepoRelDir == repoRelDir {
				continue
			}
			newProjects = append(newProjects, p)
		}

		// Overwrite the old pull status.
		currStatus.Projects = newProjects
		return b.writePullToBucket(bucket, key, currStatus)
	})
	return errors.Wrap(err, "DB transaction failed")
}

func (b *BoltDB) pullKey(pull models.PullRequest) ([]byte, error) {
	hostname := pull.BaseRepo.VCSHost.Hostname
	if strings.Contains(hostname, pullKeySeparator) {
		return nil, fmt.Errorf("vcs hostname %q contains illegal string %q", hostname, pullKeySeparator)
	}
	repo := pull.BaseRepo.FullName
	if strings.Contains(repo, pullKeySeparator) {
		return nil, fmt.Errorf("repo name %q contains illegal string %q", hostname, pullKeySeparator)
	}

	return []byte(fmt.Sprintf("%s::%s::%d", hostname, repo, pull.Num)),
		nil
}

func (b *BoltDB) lockKey(p models.Project, workspace string) string {
	return fmt.Sprintf("%s/%s/%s", p.RepoFullName, p.Path, workspace)
}

func (b *BoltDB) getPullFromBucket(bucket *bolt.Bucket, key []byte) (*models.PullStatus, error) {
	serialized := bucket.Get(key)
	if serialized == nil {
		return nil, nil
	}

	var p models.PullStatus
	if err := json.Unmarshal(serialized, &p); err != nil {
		return nil, errors.Wrapf(err, "deserializing pull at %q with contents %q", key, serialized)
	}
	return &p, nil
}

func (b *BoltDB) writePullToBucket(bucket *bolt.Bucket, key []byte, pull models.PullStatus) error {
	serialized, err := json.Marshal(pull)
	if err != nil {
		return errors.Wrap(err, "serializing")
	}
	return bucket.Put(key, serialized)
}

func (b *BoltDB) projectResultToProject(p models.ProjectResult) models.ProjectStatus {
	return models.ProjectStatus{
		Workspace:   p.Workspace,
		RepoRelDir:  p.RepoRelDir,
		ProjectName: p.ProjectName,
		Status:      p.PlanStatus(),
	}
}