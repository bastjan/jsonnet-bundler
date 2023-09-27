package pkg

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"

	"github.com/jsonnet-bundler/jsonnet-bundler/pkg/jsonnetfile"
	v1 "github.com/jsonnet-bundler/jsonnet-bundler/spec/v1"
	"github.com/jsonnet-bundler/jsonnet-bundler/spec/v1/deps"
)

func downloadAndLink(direct v1.JsonnetFile, vendorDir string, oldLocks *deps.Ordered) (*deps.Ordered, error) {
	dl := new(parallelDownloader).Ensure(direct.Dependencies, vendorDir, "", oldLocks)
	return oldLocks, linkDownloaded(direct.Dependencies, vendorDir, dl, oldLocks, make(map[string]struct{}))
}

type packageRef struct {
	name    string
	version string
}

type downloadedPackage struct {
	lock deps.Dependency
	jsf  *v1.JsonnetFile

	downloadErr error
}

// parallelDownloader is a downloader that downloads all dependencies in parallel
// The zero parallelDownloader is empty and ready for use. Must not be copied after first use.
// Should not be used after calling Ensure.
type parallelDownloader struct {
	// seen stores the packages that we are already working on
	seen sync.Map
	// stores how many goroutines are still working
	working sync.WaitGroup

	// deps stores all dependencies that we have already downloaded
	locksM sync.Mutex
	locks  map[packageRef]downloadedPackage
}

// Ensure recursively downloads all dependencies of the given direct dependencies.
// If a download already exists it is integrity checked and skipped if it is valid.
// Integrity is checked by comparing the sha256 checksum of the downloaded files with the one in the lock.
// It returns a map of all downloaded packages and their locks.
// If an error occurs, the map might be incomplete.
// If the same package is requested multiple times, it is only downloaded once.
// The download of the package might fail. This function does not return an error but stores them in the returned map.
// The downloadedPackage should be checked for downloadErr before use.
// The parallelDownloader must be discarded after calling Ensure.
func (pd *parallelDownloader) Ensure(direct *deps.Ordered, vendorDir, pathToParentModule string, oldLocks *deps.Ordered) map[packageRef]downloadedPackage {
	pd.ensure(direct, vendorDir, "", oldLocks)
	pd.working.Wait()
	return pd.locks
}

// ensure recursively downloads all dependencies of the given direct dependencies.
// It spawns goroutines for all dependencies and does not wait for the goroutines to finish.
// Callers should call pd.working.Wait() to wait for all goroutines to finish.
// Stores all downloaded packages in pd.locks and all errors in pd.errs.
func (pd *parallelDownloader) ensure(direct *deps.Ordered, vendorDir, pathToParentModule string, oldLocks *deps.Ordered) {
	for _, k := range direct.Keys() {
		pd.working.Add(1)
		go func(k string) {
			defer pd.working.Done()
			d, _ := direct.Get(k)

			ref := packageRef{name: d.Name(), version: d.Version}
			// Skip if we are already working on this package
			_, seen := pd.seen.LoadOrStore(ref, struct{}{})
			if seen {
				return
			}

			cp := cachePath(vendorDir, d)
			needsDownload := true
			expectedSum := ""

			lock, present := oldLocks.Get(d.Name())
			if present {
				// if in lock file and the integrity is intact, no need to download
				if check(lock, cp) {
					needsDownload = false
				}
				// we should use the resolved version from the lock file
				// e.g. master -> 0b2ab31b77f0ede56b660850462ff279eadcd50c
				d.Version = lock.Version
				expectedSum = lock.Sum
			}

			if needsDownload {
				if err := os.RemoveAll(cp); err != nil {
					pd.addErr(ref, err)
					return
				}
				if err := os.MkdirAll(cp, os.ModePerm); err != nil {
					pd.addErr(ref, err)
					return
				}
				l, err := download(d, cp, pathToParentModule)
				if err != nil {
					pd.addErr(ref, err)
					return
				}
				if expectedSum != "" && expectedSum != l.Sum {
					pd.addErr(ref, fmt.Errorf("integrity check failed for %s@%s", d.Name(), d.Version))
					return
				}
				lock = *l
			}

			if d.Single {
				// skip dependencies that explicitely don't want nested ones installed
				pd.addLock(ref, downloadedPackage{lock: lock})
				return
			}

			// load jsonnetfile from the package and recursively download dependencies
			f, err := jsonnetfile.Load(filepath.Join(cp, d.Name(), jsonnetfile.File))
			if err != nil {
				if os.IsNotExist(err) {
					pd.addLock(ref, downloadedPackage{lock: lock})
					return
				}
				pd.addErr(ref, err)
				return
			}
			pd.addLock(ref, downloadedPackage{lock: lock, jsf: &f})

			absolutePath, err := filepath.EvalSymlinks(filepath.Join(cp, d.Name()))
			if err != nil {
				pd.addErr(ref, err)
				return
			}

			pd.ensure(f.Dependencies, vendorDir, absolutePath, oldLocks)
		}(k)
	}
}

func (pd *parallelDownloader) addLock(p packageRef, d downloadedPackage) {
	pd.locksM.Lock()
	defer pd.locksM.Unlock()
	if pd.locks == nil {
		pd.locks = make(map[packageRef]downloadedPackage)
	}
	pd.locks[p] = d
}

func (pd *parallelDownloader) addErr(p packageRef, err error) {
	pd.locksM.Lock()
	defer pd.locksM.Unlock()
	if pd.locks == nil {
		pd.locks = make(map[packageRef]downloadedPackage)
	}
	pd.locks[p] = downloadedPackage{downloadErr: err}
}

func cachePath(vendorDir string, d deps.Dependency) string {
	return filepath.Join(vendorDir, ".cache", url.PathEscape(d.Name()+"-"+d.Version))
}

// linkDownloaded recursively links all downloaded packages into the vendor directory.
// It also deterministically adds the downloaded packages to the locks.
// The first seen packages version is used as the lock version.
func linkDownloaded(direct *deps.Ordered, vendorDir string, downloaded map[packageRef]downloadedPackage, oldLocks *deps.Ordered, seen map[string]struct{}) error {
	for _, k := range direct.Keys() {
		d, _ := direct.Get(k)
		// skip if we already linked and locked this package
		if _, ok := seen[d.Name()]; ok {
			continue
		}
		seen[d.Name()] = struct{}{}

		// check cache if we downloaded this package
		// it should always be present
		dl, ok := downloaded[packageRef{name: d.Name(), version: d.Version}]
		if !ok {
			return fmt.Errorf("could not find downloaded package %s@%s", d.Name(), d.Version)
		}
		if dl.downloadErr != nil {
			return fmt.Errorf("downloaded package %s@%s has error but is required: %w", d.Name(), d.Version, dl.downloadErr)
		}
		oldLocks.Set(d.Name(), dl.lock)

		// link the package into the vendor directory
		dest := filepath.Join(vendorDir, d.Name())
		if err := os.RemoveAll(dest); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dest), os.ModePerm); err != nil {
			return err
		}
		if err := os.Symlink(filepath.Join(cachePath(vendorDir, d), d.Name()), dest); err != nil {
			return err
		}

		if dl.jsf == nil {
			continue
		}

		// if the package has a jsonnetfile, recursively link and lock its dependencies
		linkDownloaded(dl.jsf.Dependencies, vendorDir, downloaded, oldLocks, seen)
	}

	return nil
}
