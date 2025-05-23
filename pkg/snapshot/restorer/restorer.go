// SPDX-FileCopyrightText: 2024 SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package restorer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/gardener/etcd-backup-restore/pkg/compressor"
	"github.com/gardener/etcd-backup-restore/pkg/etcdutil"
	"github.com/gardener/etcd-backup-restore/pkg/etcdutil/client"
	"github.com/gardener/etcd-backup-restore/pkg/member"
	"github.com/gardener/etcd-backup-restore/pkg/miscellaneous"
	brtypes "github.com/gardener/etcd-backup-restore/pkg/types"

	"github.com/sirupsen/logrus"
	"go.etcd.io/etcd/clientv3"
	"go.etcd.io/etcd/clientv3/snapshot"
	"go.etcd.io/etcd/embed"
	"go.etcd.io/etcd/mvcc/mvccpb"
	"go.uber.org/zap"
)

const (
	etcdConnectionTimeout                                 = 30 * time.Second
	etcdCompactTimeout                                    = 2 * time.Minute
	etcdDefragTimeout                                     = 5 * time.Minute
	periodicallyMakeEtcdLeanDeltaSnapshotInterval         = 10
	thresholdPercentageForDBSizeAlarm             float64 = 80.0 / 100.0
)

// Restorer is a struct for etcd data directory restorer
type Restorer struct {
	logger    *logrus.Entry
	zapLogger *zap.Logger
	store     brtypes.SnapStore
}

// NewRestorer returns the restorer object.
func NewRestorer(store brtypes.SnapStore, logger *logrus.Entry) (*Restorer, error) {
	zapLogger, err := zap.NewProduction()
	if err != nil {
		return nil, fmt.Errorf("unable to create the object of zapLogger: %s", err)
	}
	return &Restorer{
		logger:    logger.WithField("actor", "restorer"),
		zapLogger: zapLogger,
		store:     store,
	}, nil
}

// RestoreAndStopEtcd restore the etcd data directory as per specified restore options but doesn't return the ETCD server that it statrted.
func (r *Restorer) RestoreAndStopEtcd(ro brtypes.RestoreOptions, m member.Control) error {
	embeddedEtcd, err := r.Restore(ro, m)
	defer func() {
		if embeddedEtcd != nil {
			embeddedEtcd.Server.Stop()
			embeddedEtcd.Close()
		}
	}()
	return err
}

// Restore restores the etcd data directory as per specified restore options but returns the ETCD server that it statrted.
func (r *Restorer) Restore(ro brtypes.RestoreOptions, m member.Control) (*embed.Etcd, error) {
	r.logger.Infof("Creating temporary directory %s for persisting full and delta snapshots locally.", ro.Config.TempSnapshotsDir)
	err := os.MkdirAll(ro.Config.TempSnapshotsDir, 0700)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := os.RemoveAll(ro.Config.TempSnapshotsDir); err != nil {
			r.logger.Errorf("failed to remove restoration temp directory %s: %v", ro.Config.TempSnapshotsDir, err)
		}
	}()

	if err := r.restoreFromBaseSnapshot(ro); err != nil {
		return nil, fmt.Errorf("failed to restore from the base snapshot: %v", err)
	}

	if len(ro.DeltaSnapList) == 0 {
		r.logger.Infof("No delta snapshots present over base snapshot.")
		return nil, nil
	}

	r.logger.Infof("Attempting to apply %d delta snapshots for restoration.", len(ro.DeltaSnapList))
	r.logger.Infof("Starting an embedded etcd server...")
	e, err := miscellaneous.StartEmbeddedEtcd(r.logger, &ro)
	if err != nil {
		return e, err
	}

	embeddedEtcdEndpoints := []string{e.Clients[0].Addr().String()}

	clientFactory := etcdutil.NewClientFactory(ro.NewClientFactory, brtypes.EtcdConnectionConfig{
		MaxCallSendMsgSize: ro.Config.MaxCallSendMsgSize,
		Endpoints:          embeddedEtcdEndpoints,
		InsecureTransport:  true,
	})

	r.logger.Infof("Applying delta snapshots...")
	if err := r.applyDeltaSnapshots(clientFactory, embeddedEtcdEndpoints, ro); err != nil {
		return e, err
	}

	if m != nil {
		clientCluster, err := clientFactory.NewCluster()
		if err != nil {
			return e, err
		}
		defer func() {
			if err := clientCluster.Close(); err != nil {
				r.logger.Errorf("failed to close etcd cluster client: %v", err)
			}
		}()

		if err := m.UpdateMemberPeerURL(context.TODO(), clientCluster); err != nil {
			return e, err
		}
	}
	return e, nil
}

// restoreFromBaseSnapshot restores the etcd data directory from the base snapshot.
func (r *Restorer) restoreFromBaseSnapshot(ro brtypes.RestoreOptions) error {
	baseSnapshotPath := path.Join(ro.BaseSnapshot.SnapDir, ro.BaseSnapshot.SnapName)
	if baseSnapshotPath == "" {
		r.logger.Warnf("Base snapshot path not provided. Will do nothing.")
		return nil
	}

	r.logger.Infof("Restoring from base snapshot: %s", baseSnapshotPath)
	startTime := time.Now()

	rc, err := r.store.Fetch(*ro.BaseSnapshot)
	if err != nil {
		return fmt.Errorf("failed to fetch the base snapshot from the object store with error: %w", err)
	}
	defer func() {
		if err := rc.Close(); err != nil {
			r.logger.Errorf("failed to close the base snapshot reader: %v", err)
		}
	}()

	// Decompress the snapshot if necessary
	isCompressed, compressionPolicy, err := compressor.IsSnapshotCompressed(ro.BaseSnapshot.CompressionSuffix)
	if err != nil {
		return fmt.Errorf("failed to determine snapshot compression policy: %w", err)
	}
	if isCompressed {
		rc, err = compressor.DecompressSnapshot(rc, compressionPolicy)
		if err != nil {
			return fmt.Errorf("unable to decompress the snapshot: %w", err)
		}
	}

	// Copy the database snapshot to a temporary file on disk which the restore API will use
	db, err := os.CreateTemp(ro.Config.TempSnapshotsDir, "snapshot-*.db")
	if err != nil {
		return fmt.Errorf("failed to create a temporary file for snapshot: %w", err)
	}
	// Clean up the temporary resources required for restoration before exiting to ensure disk is not exhausted
	defer func() {
		if err := os.Remove(db.Name()); err != nil {
			r.logger.Warnf("Failed to clean up temporary resources allocated for restoration of the database, err: %v", err)
		}
	}()

	if _, err := io.Copy(db, rc); err != nil {
		return fmt.Errorf("failed to copy snapshot data into the temporary file on disk needed for restoration with error: %w", err)
	}

	elapsedTime := time.Since(startTime).Seconds()
	r.logger.Infof("Fetched the snapshot from the object store in %v seconds", elapsedTime)
	if isCompressed {
		r.logger.Infof("Successfully fetched and saved data of the base snapshot in %v seconds [CompressionPolicy:%v]", elapsedTime, compressionPolicy)
	} else {
		r.logger.Infof("Successfully fetched and saved data of the base snapshot in %v seconds", elapsedTime)
	}

	// Restore the database
	restoreCfg := snapshot.RestoreConfig{
		SnapshotPath:        db.Name(),
		Name:                ro.Config.Name,
		PeerURLs:            ro.PeerURLs.StringSlice(),
		InitialCluster:      ro.Config.InitialCluster,
		InitialClusterToken: ro.Config.InitialClusterToken,
		OutputDataDir:       ro.Config.DataDir,
		SkipHashCheck:       ro.Config.SkipHashCheck,
	}
	manager := snapshot.NewV3(r.zapLogger)
	if err := manager.Restore(restoreCfg); err != nil {
		return fmt.Errorf("failed to restore the etcd database from the base snapshot with error: %w", err)
	}

	r.logger.Infof("Successfully restored from base snapshot: %s", baseSnapshotPath)
	return nil
}

// applyDeltaSnapshots fetches the events from delta snapshots in parallel and applies them to the embedded etcd sequentially.
func (r *Restorer) applyDeltaSnapshots(clientFactory client.Factory, endPoints []string, ro brtypes.RestoreOptions) error {

	clientKV, err := clientFactory.NewKV()
	if err != nil {
		return err
	}
	defer func() {
		if err := clientKV.Close(); err != nil {
			r.logger.Errorf("failed to close etcd KV client: %v", err)
		}
	}()

	clientMaintenance, err := clientFactory.NewMaintenance()
	if err != nil {
		return err
	}
	defer func() {
		if err := clientMaintenance.Close(); err != nil {
			r.logger.Errorf("failed to close etcd maintenance client: %v", err)
		}
	}()

	snapList := ro.DeltaSnapList
	numMaxFetchers := ro.Config.MaxFetchers

	firstDeltaSnap := snapList[0]

	if err := r.applyFirstDeltaSnapshot(clientKV, firstDeltaSnap); err != nil {
		return err
	}

	embeddedEtcdQuotaBytes := float64(ro.Config.EmbeddedEtcdQuotaBytes)

	if err := verifySnapshotRevision(clientKV, snapList[0]); err != nil {
		return err
	}

	// no more delta snapshots available
	if len(snapList) == 1 {
		return nil
	}

	var (
		remainingSnaps      = snapList[1:]
		numSnaps            = len(remainingSnaps)
		numFetchers         = int(math.Min(float64(numMaxFetchers), float64(numSnaps)))
		snapLocationsCh     = make(chan string, numSnaps)
		errCh               = make(chan error, numFetchers+1)
		fetcherInfoCh       = make(chan brtypes.FetcherInfo, numSnaps)
		applierInfoCh       = make(chan brtypes.ApplierInfo, numSnaps)
		wg                  sync.WaitGroup
		stopCh              = make(chan bool)
		stopHandleAlarmCh   = make(chan bool)
		dbSizeAlarmCh       = make(chan string)
		dbSizeAlarmDisarmCh = make(chan bool)
	)

	go r.applySnaps(clientKV, clientMaintenance, remainingSnaps, dbSizeAlarmCh, dbSizeAlarmDisarmCh, applierInfoCh, errCh, stopCh, &wg, endPoints, embeddedEtcdQuotaBytes)

	for f := 0; f < numFetchers; f++ {
		go r.fetchSnaps(f, fetcherInfoCh, applierInfoCh, snapLocationsCh, errCh, stopCh, &wg, ro.Config.TempSnapshotsDir)
	}

	go r.HandleAlarm(stopHandleAlarmCh, dbSizeAlarmCh, dbSizeAlarmDisarmCh, clientMaintenance)
	defer close(stopHandleAlarmCh)

	for i, remainingSnap := range remainingSnaps {
		fetcherInfo := brtypes.FetcherInfo{
			Snapshot:  *remainingSnap,
			SnapIndex: i,
		}
		fetcherInfoCh <- fetcherInfo
	}
	close(fetcherInfoCh)

	err = <-errCh

	if cleanupErr := r.cleanup(snapLocationsCh, stopCh, &wg); cleanupErr != nil {
		r.logger.Errorf("Cleanup of temporary snapshots failed: %v", cleanupErr)
	}

	if err != nil {
		r.logger.Errorf("Restoration failed.")
		return err
	}

	r.logger.Infof("Restoration complete.")

	return nil
}

// cleanup stops all running goroutines and removes the persisted snapshot files from disk.
func (r *Restorer) cleanup(snapLocationsCh chan string, stopCh chan bool, wg *sync.WaitGroup) error {
	var errs []error

	close(stopCh)
	wg.Wait()
	close(snapLocationsCh)

	for filePath := range snapLocationsCh {
		if _, err := os.Stat(filePath); err != nil {
			if !os.IsNotExist(err) {
				errs = append(errs, fmt.Errorf("unable to stat file %s: %v", filePath, err))
			}
			continue
		}

		if err := os.Remove(filePath); err != nil {
			errs = append(errs, fmt.Errorf("unable to remove file %s: %v", filePath, err))
		}
	}

	if len(errs) != 0 {
		r.logger.Error("Cleanup failed")
		return ErrorArrayToError(errs)
	}

	r.logger.Info("Cleanup complete")
	return nil
}

// fetchSnaps fetches delta snapshots as events and persists them onto disk.
func (r *Restorer) fetchSnaps(fetcherIndex int, fetcherInfoCh <-chan brtypes.FetcherInfo, applierInfoCh chan<- brtypes.ApplierInfo, snapLocationsCh chan<- string, errCh chan<- error, stopCh chan bool, wg *sync.WaitGroup, tempDir string) {
	defer wg.Done()
	wg.Add(1)

	for fetcherInfo := range fetcherInfoCh {
		select {
		case _, more := <-stopCh:
			if !more {
				return
			}
		default:
			r.logger.Infof("Fetcher #%d fetching delta snapshot %s", fetcherIndex+1, path.Join(fetcherInfo.Snapshot.SnapDir, fetcherInfo.Snapshot.SnapName))

			rc, err := r.store.Fetch(fetcherInfo.Snapshot)
			if err != nil {
				errCh <- fmt.Errorf("failed to fetch delta snapshot %s from store : %v", fetcherInfo.Snapshot.SnapName, err)
				applierInfoCh <- brtypes.ApplierInfo{SnapIndex: -1} // cannot use close(ch) as concurrent fetchSnaps routines might try to send on channel, causing a panic
			}

			snapTempFilePath := filepath.Join(tempDir, fetcherInfo.Snapshot.SnapName)
			if err = persistRawDeltaSnapshot(rc, snapTempFilePath); err != nil {
				errCh <- fmt.Errorf("failed to persist delta snapshot %s to temp file path %s : %v", fetcherInfo.Snapshot.SnapName, snapTempFilePath, err)
				applierInfoCh <- brtypes.ApplierInfo{SnapIndex: -1}
			}

			snapLocationsCh <- snapTempFilePath // used for cleanup later

			applierInfo := brtypes.ApplierInfo{
				SnapFilePath: snapTempFilePath,
				SnapIndex:    fetcherInfo.SnapIndex,
			}
			applierInfoCh <- applierInfo
		}
	}
}

// applySnaps applies delta snapshot events to the embedded etcd sequentially, in the right order of snapshots, regardless of the order in which they were fetched.
func (r *Restorer) applySnaps(clientKV client.KVCloser, clientMaintenance client.MaintenanceCloser, remainingSnaps brtypes.SnapList, dbSizeAlarmCh chan string, dbSizeAlarmDisarmCh <-chan bool, applierInfoCh <-chan brtypes.ApplierInfo, errCh chan<- error, stopCh <-chan bool, wg *sync.WaitGroup, endPoints []string, embeddedEtcdQuotaBytes float64) {
	defer wg.Done()
	wg.Add(1)

	// To reduce or to stop the growing size of embedded etcd database during restoration
	// it's important to track number of delta snapshots applied to an embedded etcd
	// to raise an alarm(if required).
	// it is initialize with "1" as backup-restore has already applied first delta snapshot.
	numberOfDeltaSnapApplied := 1

	// A flag to track whether a previous attempt to make embedded etcd lean failed or succeeds.
	// If failed then backup-restore should re-try after applying next delta snapshot.
	prevAttemptToMakeEtcdLeanFailed := false

	pathList := make([]string, len(remainingSnaps))
	nextSnapIndexToApply := 0
	for {
		select {
		case _, more := <-stopCh:
			if !more {
				return
			}
		case applierInfo := <-applierInfoCh:
			if applierInfo.SnapIndex == -1 {
				return
			}

			fetchedSnapIndex := applierInfo.SnapIndex
			pathList[fetchedSnapIndex] = applierInfo.SnapFilePath

			if fetchedSnapIndex < nextSnapIndexToApply {
				errCh <- fmt.Errorf("snap index mismatch for delta snapshot %d; expected snap index to be atleast %d", fetchedSnapIndex, nextSnapIndexToApply)
				return
			}
			if fetchedSnapIndex == nextSnapIndexToApply {
				for currSnapIndex := fetchedSnapIndex; currSnapIndex < len(remainingSnaps); currSnapIndex++ {
					if pathList[currSnapIndex] == "" {
						break
					}

					filePath := pathList[currSnapIndex]
					snapName := remainingSnaps[currSnapIndex].SnapName

					r.logger.Infof("Reading snapshot contents %s from raw snapshot file %s", snapName, filePath)
					eventsData, err := r.readSnapshotContentsFromFile(filePath, remainingSnaps[currSnapIndex])
					if err != nil {
						errCh <- fmt.Errorf("failed to read events data from delta snapshot file %s : %v", filePath, err)
						return
					}

					var events []brtypes.Event
					if err = json.Unmarshal(eventsData, &events); err != nil {
						errCh <- fmt.Errorf("failed to unmarshal events from events data for delta snapshot %s : %v", snapName, err)
						return
					}

					r.logger.Infof("Applying delta snapshot %s [%d/%d]", path.Join(remainingSnaps[currSnapIndex].SnapDir, remainingSnaps[currSnapIndex].SnapName), currSnapIndex+2, len(remainingSnaps)+1)
					if err := applyEventsAndVerify(clientKV, events, remainingSnaps[currSnapIndex]); err != nil {
						errCh <- err
						return
					}

					r.logger.Infof("Removing temporary delta snapshot events file %s for snapshot %s", filePath, snapName)
					if err = os.Remove(filePath); err != nil {
						r.logger.Warnf("Unable to remove file: %s; err: %v", filePath, err)
					}

					nextSnapIndexToApply++
					if nextSnapIndexToApply == len(remainingSnaps) {
						errCh <- nil // restore finished
						return
					}

					numberOfDeltaSnapApplied++

					if numberOfDeltaSnapApplied%periodicallyMakeEtcdLeanDeltaSnapshotInterval == 0 || prevAttemptToMakeEtcdLeanFailed {
						r.logger.Info("making an embedded etcd lean and check for db size alarm")
						if err := r.MakeEtcdLeanAndCheckAlarm(int64(remainingSnaps[currSnapIndex].LastRevision), endPoints, embeddedEtcdQuotaBytes, dbSizeAlarmCh, dbSizeAlarmDisarmCh, clientKV, clientMaintenance); err != nil {
							r.logger.Errorf("unable to make embedded etcd lean: %v", err)
							r.logger.Warn("etcd mvcc: database space might exceeds its quota limit")
							r.logger.Info("backup-restore will try again in next attempt...")
							// setting the flag to true
							// so, backup-restore shouldn't wait for periodically call
							// and it should re-try after applying next delta snapshot.
							prevAttemptToMakeEtcdLeanFailed = true
						} else {
							prevAttemptToMakeEtcdLeanFailed = false
						}
					}
				}
			}
		}
	}
}

// applyEventsAndVerify applies events from one snapshot to the embedded etcd and verifies the correctness of the sequence of snapshot applied.
func applyEventsAndVerify(clientKV client.KVCloser, events []brtypes.Event, snap *brtypes.Snapshot) error {
	if err := applyEventsToEtcd(clientKV, events); err != nil {
		return fmt.Errorf("failed to apply events to etcd for delta snapshot %s : %v", snap.SnapName, err)
	}

	if err := verifySnapshotRevision(clientKV, snap); err != nil {
		return fmt.Errorf("snapshot revision verification failed for delta snapshot %s : %v", snap.SnapName, err)
	}
	return nil
}

// applyFirstDeltaSnapshot applies the events from first delta snapshot to etcd.
func (r *Restorer) applyFirstDeltaSnapshot(clientKV client.KVCloser, snap *brtypes.Snapshot) error {
	r.logger.Infof("Applying first delta snapshot %s", path.Join(snap.SnapDir, snap.SnapName))

	rc, err := r.store.Fetch(*snap)
	if err != nil {
		return fmt.Errorf("failed to fetch delta snapshot %s from store : %v", snap.SnapName, err)
	}

	eventsData, err := r.readSnapshotContentsFromReadCloser(rc, snap)
	if err != nil {
		return fmt.Errorf("failed to read events data from delta snapshot %s : %v", snap.SnapName, err)
	}

	var events []brtypes.Event
	if err = json.Unmarshal(eventsData, &events); err != nil {
		return fmt.Errorf("failed to unmarshal events data from delta snapshot %s : %v", snap.SnapName, err)
	}

	// Note: Since revision in full snapshot file name might be lower than actual revision stored in snapshot.
	// This is because of issue referred below. So, as per workaround used in our logic of taking delta snapshot,
	// the latest revision from full snapshot may overlap with first few revision on first delta snapshot
	// Hence, we have to additionally take care of that.
	// Refer: https://github.com/coreos/etcd/issues/9037
	ctx, cancel := context.WithTimeout(context.TODO(), etcdConnectionTimeout)
	defer cancel()
	resp, err := clientKV.Get(ctx, "", clientv3.WithLastRev()...)
	if err != nil {
		return fmt.Errorf("failed to get etcd latest revision: %v", err)
	}
	lastRevision := resp.Header.Revision

	if lastRevision == snap.LastRevision {
		// there is no need to apply this fist delta snapshot
		// as it's completely overlaps with full snapshot data.
		// please refer: https://github.com/gardener/etcd-backup-restore/issues/844
		r.logger.Infof("First delta snapshot %s found to be completely overlap with full snapshot with db revisions: %d", path.Join(snap.SnapDir, snap.SnapName), lastRevision)
		r.logger.Info("Skipping this delta snapshot...")
		return nil
	}

	var newRevisionIndex int
	for index, event := range events {
		if event.EtcdEvent.Kv.ModRevision > lastRevision {
			newRevisionIndex = index
			break
		}
	}

	r.logger.Infof("Applying first delta snapshot %s", path.Join(snap.SnapDir, snap.SnapName))

	return applyEventsToEtcd(clientKV, events[newRevisionIndex:])
}

func persistRawDeltaSnapshot(rc io.ReadCloser, tempFilePath string) error {
	tempFile, err := os.Create(tempFilePath) // #nosec G304 -- this is a trusted filepath for persisting delta snapshots for restoration.
	if err != nil {
		err = fmt.Errorf("failed to create temp file %s to store raw delta snapshot", tempFilePath)
		return err
	}
	defer func() {
		_ = tempFile.Close()
	}()

	_, err = tempFile.ReadFrom(rc)
	if err != nil {
		return err
	}

	return rc.Close()
}

// applyEventsToEtcd performs operations in events sequentially.
func applyEventsToEtcd(clientKV client.KVCloser, events []brtypes.Event) error {
	var (
		lastRev int64
		ops     = []clientv3.Op{}
		ctx     = context.TODO()
	)

	for _, e := range events {
		ev := e.EtcdEvent
		nextRev := ev.Kv.ModRevision
		if lastRev != 0 && nextRev > lastRev {
			if _, err := clientKV.Txn(ctx).Then(ops...).Commit(); err != nil {
				return err
			}
			ops = []clientv3.Op{}
		}
		lastRev = nextRev
		switch ev.Type {
		case mvccpb.PUT:
			ops = append(ops, clientv3.OpPut(string(ev.Kv.Key), string(ev.Kv.Value))) //, clientv3.WithLease(clientv3.LeaseID(ev.Kv.Lease))))

		case mvccpb.DELETE:
			ops = append(ops, clientv3.OpDelete(string(ev.Kv.Key)))
		default:
			return fmt.Errorf("unexpected event type")
		}
	}
	_, err := clientKV.Txn(ctx).Then(ops...).Commit()
	return err
}

func verifySnapshotRevision(clientKV client.KVCloser, snap *brtypes.Snapshot) error {
	ctx := context.TODO()
	getResponse, err := clientKV.Get(ctx, "foo")
	if err != nil {
		return fmt.Errorf("failed to connect to etcd KV client: %v", err)
	}
	etcdRevision := getResponse.Header.GetRevision()
	if snap.LastRevision != etcdRevision {
		return fmt.Errorf("mismatched event revision while applying delta snapshot, expected %d but applied %d ", snap.LastRevision, etcdRevision)
	}
	return nil
}

// getNormalizedSnapshotReadCloser passes the given ReadCloser through the
// snapshot decompressor if the snapshot is compressed using a compression policy.
// If snapshot is not compressed, it returns the given ReadCloser as is.
// It also returns whether the snapshot was initially compressed or not, as well as
// the compression policy used for compressing the snapshot.
func getNormalizedSnapshotReadCloser(rc io.ReadCloser, snap *brtypes.Snapshot) (io.ReadCloser, bool, string, error) {
	isCompressed, compressionPolicy, err := compressor.IsSnapshotCompressed(snap.CompressionSuffix)
	if err != nil {
		return rc, false, "", err
	}

	if isCompressed {
		// decompress the snapshot
		rc, err = compressor.DecompressSnapshot(rc, compressionPolicy)
		if err != nil {
			return rc, true, compressionPolicy, fmt.Errorf("unable to decompress the snapshot: %v", err)
		}
	}

	return rc, isCompressed, compressionPolicy, nil
}

func (r *Restorer) readSnapshotContentsFromReadCloser(rc io.ReadCloser, snap *brtypes.Snapshot) ([]byte, error) {
	startTime := time.Now()

	rc, wasCompressed, compressionPolicy, err := getNormalizedSnapshotReadCloser(rc, snap)
	if err != nil {
		return nil, fmt.Errorf("failed to decompress delta snapshot %s : %v", snap.SnapName, err)
	}

	buf := new(bytes.Buffer)
	bufSize, err := buf.ReadFrom(rc)
	if err != nil {
		return nil, fmt.Errorf("failed to parse contents from delta snapshot %s : %v", snap.SnapName, err)
	}

	totalTime := time.Since(startTime).Seconds()
	if wasCompressed {
		r.logger.Infof("successfully decompressed data of delta snapshot in %v seconds [CompressionPolicy:%v]", totalTime, compressionPolicy)
	} else {
		r.logger.Infof("successfully read the data of delta snapshot in %v seconds", totalTime)
	}

	if bufSize <= sha256.Size {
		return nil, fmt.Errorf("delta snapshot is missing hash")
	}

	sha := buf.Bytes()
	data := sha[:bufSize-sha256.Size]
	snapHash := sha[bufSize-sha256.Size:]

	// check for match
	h := sha256.New()
	if _, err := h.Write(data); err != nil {
		return nil, fmt.Errorf("unable to check integrity of snapshot %s: %v", snap.SnapName, err)
	}

	computedSha := h.Sum(nil)
	if !reflect.DeepEqual(snapHash, computedSha) {
		return nil, fmt.Errorf("expected sha256 %v, got %v", snapHash, computedSha)
	}

	return data, nil
}

func (r *Restorer) readSnapshotContentsFromFile(filePath string, snap *brtypes.Snapshot) ([]byte, error) {
	file, err := os.Open(filePath) // #nosec G304 -- this is a trusted snapshot file.
	if err != nil {
		return nil, fmt.Errorf("failed to open file %s for delta snapshot %s : %v", filePath, snap.SnapName, err)
	}

	return r.readSnapshotContentsFromReadCloser(file, snap)
}

// ErrorArrayToError takes an array of errors and returns a single concatenated error
func ErrorArrayToError(errs []error) error {
	if len(errs) == 0 {
		return nil
	}

	var errString string

	for _, e := range errs {
		errString = fmt.Sprintf("%s\n%s", errString, e.Error())
	}

	return fmt.Errorf("%s", strings.TrimSpace(errString))
}

// HandleAlarm function handles alarm raised by backup-restore.
func (r *Restorer) HandleAlarm(stopHandleAlarmCh chan bool, dbSizeAlarmCh <-chan string, dbSizeAlarmDisarmCh chan bool, clientMaintenance client.MaintenanceCloser) {
	r.logger.Info("Starting to handle an alarm...")
	for {
		select {
		case <-stopHandleAlarmCh:
			r.logger.Info("Closing handleAlarm...")
			return
		case endPoint := <-dbSizeAlarmCh:
			r.logger.Info("Received a dbsize alarm")
			r.logger.Infof("Calling defrag on endpoint: [%v]", endPoint)
			if err := func() error {
				ctx, cancel := context.WithTimeout(context.Background(), etcdDefragTimeout)
				defer cancel()
				if _, err := clientMaintenance.Defragment(ctx, endPoint); err != nil {
					return err
				}
				return nil
			}(); err != nil {
				r.logger.Errorf("unable to disalarm as defrag call failed: %v", err)
				// failed to disalarm
				dbSizeAlarmDisarmCh <- false
			} else {
				// successfully disalarm
				dbSizeAlarmDisarmCh <- true
			}
		}
	}
}

// MakeEtcdLeanAndCheckAlarm calls etcd compaction on given revision number and raise db size alarm if embedded etcd db size crosses threshold.
func (r *Restorer) MakeEtcdLeanAndCheckAlarm(revision int64, endPoints []string, embeddedEtcdQuotaBytes float64, dbSizeAlarmCh chan string, dbSizeAlarmDisarmCh <-chan bool, clientKV client.KVCloser, clientMaintenance client.MaintenanceCloser) error {

	ctx, cancel := context.WithTimeout(context.Background(), etcdCompactTimeout)
	defer cancel()
	if _, err := clientKV.Compact(ctx, revision, clientv3.WithCompactPhysical()); err != nil {
		return fmt.Errorf("compact API call failed: %w", err)
	}
	r.logger.Infof("Successfully compacted embedded etcd till revision: %v", revision)

	ctx, cancel = context.WithTimeout(context.Background(), etcdConnectionTimeout)
	defer cancel()

	// check database size of embedded etcdServer.
	status, err := clientMaintenance.Status(ctx, endPoints[0])
	if err != nil {
		return fmt.Errorf("unable to check embedded etcd status: %v", err)
	}

	if float64(status.DbSizeInUse) > thresholdPercentageForDBSizeAlarm*embeddedEtcdQuotaBytes ||
		float64(status.DbSize) > thresholdPercentageForDBSizeAlarm*embeddedEtcdQuotaBytes {
		r.logger.Info("Embedded etcd database size crosses the threshold limit")
		r.logger.Info("Raising a dbSize alarm...")

		for _, endPoint := range endPoints {
			// send endpoint to alarm channel to raise an db size alarm
			dbSizeAlarmCh <- endPoint

			if !<-dbSizeAlarmDisarmCh {
				return fmt.Errorf("failed to disalarm the embedded etcd dbSize alarm")
			}

			r.logger.Info("Successfully disalarm the embedded etcd dbSize alarm")
			ctx, cancel := context.WithTimeout(context.Background(), etcdConnectionTimeout)
			defer cancel()
			if afterDefragStatus, err := clientMaintenance.Status(ctx, endPoint); err != nil {
				r.logger.Warnf("failed to get status of embedded etcd with error: %v", err)
			} else {
				dbSizeBeforeDefrag := status.DbSize
				dbSizeAfterDefrag := afterDefragStatus.DbSize
				r.logger.Infof("Probable DB size change for embedded etcd: %dB -> %dB after defragmentation call", dbSizeBeforeDefrag, dbSizeAfterDefrag)
			}
		}
	} else {
		r.logger.Infof("Embedded etcd dbsize: %dB didn't crosses the threshold limit: %fB", status.DbSize, thresholdPercentageForDBSizeAlarm*embeddedEtcdQuotaBytes)
	}
	return nil
}
