// Copyright 2018 jsonnet-bundler authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package pkg

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/fatih/color"
	"github.com/pkg/errors"

	"github.com/jsonnet-bundler/jsonnet-bundler/pkg/jsonnetfile"
	v1 "github.com/jsonnet-bundler/jsonnet-bundler/spec/v1"
	"github.com/jsonnet-bundler/jsonnet-bundler/spec/v1/deps"
)

var (
	VersionMismatch = errors.New("multiple colliding versions specified")
)

// Ensure receives all direct packages, the directory to vendor into and all known locks.
// It then makes sure all direct and nested dependencies are present in vendor at the correct version:
//
// If the package is locked and the files in vendor match the sha256 checksum,
// nothing needs to be done. Otherwise, the package is retrieved from the
// upstream source and added into vendor. If previously locked, the sums are
// checked as well.
// In case a (nested) package is already present in the lock,
// the one from the lock takes precedence. This allows the user to set the
// desired version in case by `jb install`ing it.
//
// Finally, all unknown files and directories are removed from vendor/
// The full list of locked depedencies is returned
func Ensure(direct v1.JsonnetFile, vendorDir string, oldLocks *deps.Ordered) (*deps.Ordered, error) {
	// ensure all required files are in vendor
	// This is the actual installation
	locks, err := downloadAndLink(direct, vendorDir, oldLocks)
	if err != nil {
		return nil, err
	}

	// remove unchanged legacyNames
	CleanLegacyName(locks)

	// find unknown dirs in vendor/
	names := []string{}
	err = filepath.Walk(vendorDir, func(path string, i os.FileInfo, err error) error {
		if path == vendorDir {
			return nil
		}
		if strings.HasPrefix(path, filepath.Join(vendorDir, ".cache")) {
			return nil
		}
		if !i.IsDir() {
			return nil
		}

		names = append(names, path)
		return nil
	})
	if err != nil {
		return nil, err
	}

	// remove them
	for _, dir := range names {
		name, err := filepath.Rel(vendorDir, dir)
		if err != nil {
			return nil, err
		}
		if !known(locks, name) {
			if err := os.RemoveAll(dir); err != nil {
				return nil, err
			}
			color.Magenta("CLEAN %s", dir)
		}
	}

	// remove all symlinks, optionally adding known ones back later if wished
	if err := cleanLegacySymlinks(vendorDir, locks); err != nil {
		return nil, err
	}
	if !direct.LegacyImports {
		return locks, nil
	}
	if err := linkLegacy(vendorDir, locks); err != nil {
		return nil, err
	}

	// return the final lockfile contents
	return locks, nil
}

func CleanLegacyName(list *deps.Ordered) {
	for _, k := range list.Keys() {
		d, _ := list.Get(k)
		// unset if not changed by user
		if d.LegacyNameCompat == d.Source.LegacyName() {
			dep, _ := list.Get(k)
			dep.LegacyNameCompat = ""
			list.Set(k, dep)
		}
	}
}

func cleanLegacySymlinks(vendorDir string, locks *deps.Ordered) error {
	// local packages need to be ignored
	known := map[string]struct{}{}
	for _, k := range locks.Keys() {
		d, _ := locks.Get(k)
		// Name contains the absolute path to the package, we only want to remove the relative ones
		known[filepath.Join(vendorDir, d.Name())] = struct{}{}
	}

	// remove all unknown symlinks first
	return filepath.Walk(vendorDir, func(path string, i os.FileInfo, err error) error {
		if strings.HasPrefix(path, filepath.Join(vendorDir, ".cache")) {
			return nil
		}
		if _, found := known[path]; found {
			return nil
		}

		if i.Mode()&os.ModeSymlink != 0 {
			if err := os.Remove(path); err != nil {
				return err
			}
		}
		return nil
	})
}

func linkLegacy(vendorDir string, locks *deps.Ordered) error {
	// create only the ones we want
	for _, k := range locks.Keys() {
		d, _ := locks.Get(k)
		// localSource still uses the relative style
		if d.Source.LocalSource != nil {
			continue
		}

		legacyName := filepath.Join(vendorDir, d.LegacyName())
		pkgName := d.Name()

		taken, err := checkLegacyNameTaken(legacyName, pkgName)
		if err != nil {
			fmt.Println(err)
			continue
		}
		if taken {
			continue
		}

		// create the symlink
		if err := os.Symlink(
			filepath.Join(pkgName),
			filepath.Join(legacyName),
		); err != nil {
			return err
		}
	}
	return nil
}

func checkLegacyNameTaken(legacyName string, pkgName string) (bool, error) {
	fi, err := os.Lstat(legacyName)
	if err != nil {
		// does not exist: not taken
		if os.IsNotExist(err) {
			return false, nil
		}
		// a real error
		return false, err
	}

	// is it a symlink?
	if fi.Mode()&os.ModeSymlink != 0 {
		s, err := os.Readlink(legacyName)
		if err != nil {
			return false, err
		}
		color.Yellow("WARN: cannot link '%s' to '%s', because package '%s' already uses that name. The absolute import still works\n", pkgName, legacyName, s)
		return true, nil
	}

	// sth else
	color.Yellow("WARN: cannot link '%s' to '%s', because the file/directory already exists. The absolute import still works.\n", pkgName, legacyName)
	return true, nil
}

func known(deps *deps.Ordered, p string) bool {
	p = filepath.ToSlash(p)
	for _, kd := range deps.Keys() {
		d, _ := deps.Get(kd)
		k := filepath.ToSlash(d.Name())
		if strings.HasPrefix(p, k) || strings.HasPrefix(k, p) {
			return true
		}
	}
	return false
}

// download retrieves a package from a remote upstream. The checksum of the
// files is generated afterwards.
func download(d deps.Dependency, vendorDir, pathToParentModule string) (*deps.Dependency, error) {
	var p Interface
	switch {
	case d.Source.GitSource != nil:
		p = NewGitPackage(d.Source.GitSource)
	case d.Source.LocalSource != nil:
		wd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("failed to get current working directory: %w", err)
		}

		// Resolve the relative path to the parent module. When a local
		// dependency tree is resolved recursively, nested local dependencies
		// with relative paths must be evaluated relative to their referencing
		// jsonnetfile, rather than relative to the top-level jsonnetfile.
		modulePath, err := filepath.Rel(wd, filepath.Join(pathToParentModule, d.Source.LocalSource.Directory))
		if err != nil {
			modulePath = d.Source.LocalSource.Directory
		}

		p = NewLocalPackage(&deps.Local{Directory: modulePath})
	}

	if p == nil {
		return nil, errors.New("either git or local source is required")
	}

	version, err := p.Install(context.TODO(), d.Name(), vendorDir, d.Version)
	if err != nil {
		return nil, err
	}

	var sum string
	if d.Source.LocalSource == nil {
		sum, err = hashDir(filepath.Join(vendorDir, d.Name()))
		if err != nil {
			return nil, err
		}
	}

	d.Version = version
	d.Sum = sum
	return &d, nil
}

// check returns whether the files present at the vendor/ folder match the
// sha256 sum of the package. local-directory dependencies are not checked as
// their purpose is to change during development where integrity checking would
// be a hindrance.
func check(d deps.Dependency, vendorDir string) bool {
	// assume a local dependency is intact as long as it exists
	if d.Source.LocalSource != nil {
		x, err := jsonnetfile.Exists(filepath.Join(vendorDir, d.Name()))
		if err != nil {
			return false
		}
		return x
	}

	if d.Sum == "" {
		// no sum available, need to download
		return false
	}

	dir := filepath.Join(vendorDir, d.Name())
	sum, err := hashDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			color.Red("ERROR %s@%s %s", d.Name(), d.Version, err)
		}
		return false
	}
	if d.Sum == sum {
		return true
	}
	color.Yellow("CHECKSUM FAIL %s@%s", d.Name(), d.Version)
	return false
}

// hashDir computes the checksum of a directory by concatenating all files and
// hashing this data using sha256. This can be memory heavy with lots of data,
// but jsonnet files should be fairly small
func hashDir(dir string) (string, error) {
	hasher := sha256.New()

	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// if having the same dependencies with subdir and without subdir
		// there might be symlinks injected
		if info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
			return nil
		}

		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()

		if _, err := io.Copy(hasher, f); err != nil {
			return err
		}

		return nil
	})

	return base64.StdEncoding.EncodeToString(hasher.Sum(nil)), err
}
