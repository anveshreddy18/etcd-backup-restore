// Copyright 2015 The etcd Authors
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

package fileutil

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"
)

func PurgeFile(lg *zap.Logger, dirname string, suffix string, max uint, interval time.Duration, stop <-chan struct{}) <-chan error {
	return purgeFile(lg, dirname, suffix, max, interval, stop, nil, nil, true)
}

func PurgeFileWithDoneNotify(lg *zap.Logger, dirname string, suffix string, max uint, interval time.Duration, stop <-chan struct{}) (<-chan struct{}, <-chan error) {
	doneC := make(chan struct{})
	errC := purgeFile(lg, dirname, suffix, max, interval, stop, nil, doneC, true)
	return doneC, errC
}

func PurgeFileWithoutFlock(lg *zap.Logger, dirname string, suffix string, max uint, interval time.Duration, stop <-chan struct{}) (<-chan struct{}, <-chan error) {
	doneC := make(chan struct{})
	errC := purgeFile(lg, dirname, suffix, max, interval, stop, nil, doneC, false)
	return doneC, errC
}

// purgeFile is the internal implementation for PurgeFile which can post purged files to purgec if non-nil.
// if donec is non-nil, the function closes it to notify its exit.
func purgeFile(lg *zap.Logger, dirname string, suffix string, max uint, interval time.Duration, stop <-chan struct{}, purgec chan<- string, donec chan<- struct{}, flock bool) <-chan error {
	errC := make(chan error, 1)
	if lg != nil {
		lg.Info("started to purge file",
			zap.String("dir", dirname),
			zap.String("suffix", suffix),
			zap.Uint("max", max),
			zap.Duration("interval", interval))
	} else {
		plog.Infof("started to purge file, dir: %s, suffix: %s, max: %d, interval: %v", dirname, suffix, max, interval)
	}

	go func() {
		if donec != nil {
			defer close(donec)
		}
		for {
			fnames, err := ReadDir(dirname)
			if err != nil {
				if lg != nil {
					lg.Warn("failed to read files", zap.String("dir", dirname), zap.Error(err))
				} else {
					plog.Warningf("failed to read files, dir: %s, error: %v", dirname, err)
				}
				errC <- err
				return
			}
			newfnames := make([]string, 0)
			for _, fname := range fnames {
				if strings.HasSuffix(fname, suffix) {
					newfnames = append(newfnames, fname)
				}
			}
			sort.Strings(newfnames)
			fnames = newfnames
			for len(newfnames) > int(max) {
				f := filepath.Join(dirname, newfnames[0])
				var l *LockedFile
				if flock {
					l, err = TryLockFile(f, os.O_WRONLY, PrivateFileMode)
					if err != nil {
						if lg != nil {
							lg.Warn("failed to lock file", zap.String("path", f), zap.Error(err))
						} else {
							plog.Warningf("failed to lock file, path: %s, error: %v", f, err)
						}
						break
					}
				}
				if err = os.Remove(f); err != nil {
					if lg != nil {
						lg.Error("failed to remove file", zap.String("path", f), zap.Error(err))
					} else {
						plog.Errorf("failed to remove file, path: %s, error: %v", f, err)
					}
					errC <- err
					return
				}
				if flock {
					if err = l.Close(); err != nil {
						if lg != nil {
							lg.Error("failed to unlock/close", zap.String("path", l.Name()), zap.Error(err))
						} else {
							plog.Errorf("error unlocking %s when purging file (%v)", l.Name(), err)
						}
						errC <- err
						return
					}
				}
				if lg != nil {
					lg.Info("purged", zap.String("path", f))
				} else {
					plog.Infof("purged file %s successfully", f)
				}
				newfnames = newfnames[1:]
			}
			if purgec != nil {
				for i := 0; i < len(fnames)-len(newfnames); i++ {
					purgec <- fnames[i]
				}
			}
			select {
			case <-time.After(interval):
			case <-stop:
				return
			}
		}
	}()
	return errC
}
