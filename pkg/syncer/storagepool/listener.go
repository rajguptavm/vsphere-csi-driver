/*
Copyright 2020 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package storagepool

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vim25/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	cnsvsphere "sigs.k8s.io/vsphere-csi-driver/v3/pkg/common/cns-lib/vsphere"
	"sigs.k8s.io/vsphere-csi-driver/v3/pkg/csi/service/logger"
)

var (
	// reconcileAllMutex should be acquired to run ReconcileAllStoragePools so
	// that only one thread runs at a time.
	reconcileAllMutex sync.Mutex
	// Run ReconcileAllStoragePools every `freq` seconds.
	reconcileAllFreq = time.Second * 60
	// Run ReconcileAllStoragePools `n` times.
	reconcileAllIterations = 5
)

func startPropertyCollectorListener(ctx context.Context) {
	exitChannel := make(chan interface{})
	go managePCListenerInstance(ctx, exitChannel)
	initListener(ctx, defaultStoragePoolService.GetScWatch(), defaultStoragePoolService.GetSPController(), exitChannel)
}

// managePCListenerInstance is responsible for making sure that Property
// collector listener is always running. If the listener crashes for some
// reason it restarts the listener after 1 minute. The delay is so that we
// don't overwhelm VC with connection requests.
func managePCListenerInstance(ctx context.Context, exitChannel chan interface{}) {
	<-exitChannel
	log := logger.GetLogger(ctx)
	sleepTime := time.Minute
	log.Infof("Will restart property collector in %v secs", sleepTime.Seconds())
	time.Sleep(time.Minute)
	startPropertyCollectorListener(ctx)
}

// Initialize a PropertyCollector listener that updates the intended state of
// a StoragePool.
func initListener(ctx context.Context, scWatchCntlr *StorageClassWatch,
	spController *SpController, exitChannel chan interface{}) {
	log := logger.GetLogger(ctx)

	filter := new(property.WaitFilter)
	ts := types.TraversalSpec{
		Type: "ClusterComputeResource",
		Path: "host",
		Skip: types.NewBool(false),
		SelectSet: []types.BaseSelectionSpec{
			&types.TraversalSpec{
				Type: "HostSystem",
				Path: "datastore",
				Skip: types.NewBool(false),
			},
		},
	}
	for _, clusterID := range scWatchCntlr.clusterIDs {
		// Initialize a PropertyCollector that watches all objects in the hierarchy
		// of cluster -> hosts in the cluster -> datastores mounted on hosts. One or
		// more StoragePool instances would be created for each Datastore.
		clusterMoref := types.ManagedObjectReference{
			Type:  "ClusterComputeResource",
			Value: clusterID,
		}
		filter.Add(clusterMoref, clusterMoref.Type, []string{"host"}, &ts)
	}
	prodHost := types.PropertySpec{
		Type:    "HostSystem",
		PathSet: []string{"datastore", "runtime.inMaintenanceMode"},
	}
	propDs := types.PropertySpec{
		Type:    "Datastore",
		PathSet: []string{"summary"},
	}
	filter.Spec.PropSet = append(filter.Spec.PropSet, prodHost, propDs)
	go func() {
		defer func() {
			// Signal managePCListenerInstance that PC listener has exited.
			close(exitChannel)
			// If this goroutine panics midway during execution, we recover so as
			// to not crash the container.
			if recoveredErr := recover(); recoveredErr != nil {
				log.Infof("Recovered Panic in initListener: %v", recoveredErr)
			}
		}()

		err := spController.vc.Connect(ctx)
		if err != nil {
			log.Errorf("Not able to establish connection with VC. Restarting property collector.")
			return
		}

		for {
			log.Infof("Starting property collector...")

			pc, pcErr := property.DefaultCollector(spController.vc.Client.Client).Create(ctx)
			if err != nil {
				log.Errorf("Not able to create property collector. Restarting property collector. error: %+v", pcErr)
				return
			}

			err := property.WaitForUpdatesEx(ctx, pc, filter, func(updates []types.ObjectUpdate) bool {
				ctx := logger.NewContextWithLogger(ctx)
				log = logger.GetLogger(ctx)
				log.Debugf("Got %d property collector update(s)", len(updates))
				reconcileAllScheduled := false
				for _, update := range updates {
					propChange := update.ChangeSet
					log.Debugf("Got update for object %v properties %v", update.Obj, propChange)
					if update.Obj.Type == "Datastore" && len(propChange) == 1 &&
						propChange[0].Name == "summary" && propChange[0].Op == types.PropertyChangeOpAssign {
						// Handle changes in a Datastore's summary property (that
						// includes name, type, freeSpace, accessible) by updating
						// only the corresponding single StoragePool instance.
						ds := update.Obj
						summary, ok := propChange[0].Val.(types.DatastoreSummary)
						if !ok {
							log.Errorf("Not able to cast to DatastoreSummary: %v", propChange[0].Val)
							continue
						}
						// Datastore summary property can be updated immediately into the StoragePool.
						log.Debugf("Starting update for single StoragePool %s", ds.Value)
						err := spController.updateIntendedState(ctx, ds.Value, summary, scWatchCntlr)
						if err != nil {
							log.Errorf("Error updating StoragePool for datastore %v. Err: %v", ds, err)
						}
					} else {
						// Handle changes in "hosts in cluster", "hosts
						// inMaintenanceMode state" and "Datastores mounted on hosts"
						// by scheduling a reconcile of all StoragePool instances
						// afresh. Schedule only once for a batch of updates.
						if !reconcileAllScheduled {
							scheduleReconcileAllStoragePools(ctx, reconcileAllFreq,
								reconcileAllIterations, scWatchCntlr, spController)
							reconcileAllScheduled = true
						}
					}
				}
				log.Debugf("Done processing %d property collector update(s)", len(updates))
				return false
			})

			// Attempt to clean up the property collector using a new context to
			// ensure it goes through. This call *might* fail if the session's
			// auth has expired, but it is worth trying.
			_ = pc.Destroy(context.Background())

			if err != nil {
				log.Infof("Property collector session needs to be reestablished due to err: %v", err)
				err = spController.vc.Connect(ctx)
				if err != nil {
					log.Errorf("Not able to reestablish connection with VC. Restarting property collector.")
					return
				}
			}
		}
	}()
}

// XXX: This hack should be removed once we figure out all the properties of a
// StoragePool that gets updates lazily. Notifications for Hosts add/remove from
// a Cluster and Datastores un/mounted on Hosts come in too early before the VC
// is ready. For example, the vSAN Datastore mount is not completed yet for a
// newly added host. The Datastore does not have the vSANDirect tag yet for a
// newly added Datastore in a host. So this function schedules a
// ReconcileAllStoragePools, that computes addition and deletion of StoragePool
// instances, to run n times every f secs. Each time this function is called,
// the counter n is reset, so that ReconcileAllStoragePools can run another n
// times starting now. The values for n and f can be tuned so that
// ReconcileAllStoragePools can be retried enough number of times to discover
// additions and deletions of StoragePool instances in all cases.
// -- See bug 2602864.
func scheduleReconcileAllStoragePools(ctx context.Context, freq time.Duration,
	nTimes int, scWatchCntrl *StorageClassWatch, spController *SpController) {
	log := logger.GetLogger(ctx)
	log.Debugf("Scheduled ReconcileAllStoragePools starting")
	go func() {
		tick := time.NewTicker(freq)
		defer tick.Stop()
		for iteration := 0; iteration < nTimes; iteration++ {
			iterID := "iteration-" + strconv.Itoa(iteration)
			select {
			case <-tick.C:
				log.Debugf("[%s] Starting reconcile for all StoragePool instances", iterID)
				err := ReconcileAllStoragePools(ctx, scWatchCntrl, spController)
				if err != nil {
					log.Errorf("[%s] Error reconciling StoragePool instances in HostMount listener. Err: %+v", iterID, err)
				} else {
					log.Debugf("[%s] Successfully reconciled all StoragePool instances", iterID)
				}
			case <-ctx.Done():
				log.Debugf("[%s] Done reconcile all loop for StoragePools", iterID)
				return
			}
		}
		log.Infof("Scheduled ReconcileAllStoragePools completed")
	}()
	log.Infof("Scheduled ReconcileAllStoragePools started")
}

// ReconcileAllStoragePools creates/updates/deletes StoragePool instances for
// datastores found in this k8s cluster. This should be invoked when there is
// a potential change in the list of datastores in the cluster.
func ReconcileAllStoragePools(ctx context.Context, scWatchCntlr *StorageClassWatch, spCtl *SpController) error {
	log := logger.GetLogger(ctx)

	// Only one thread at a time can execute ReconcileAllStoragePools.
	reconcileAllMutex.Lock()
	defer reconcileAllMutex.Unlock()

	// Shallow copy VC to prevent nil pointer dereference exception caused due
	// to vc.Disconnect func running in parallel.
	vc := *spCtl.vc
	err := vc.Connect(ctx)
	if err != nil {
		log.Errorf("failed to connect to vCenter. Err: %+v", err)
		return err
	}

	// Get datastores from VC.
	clusterIDToDSs := cnsvsphere.GetCandidateDatastoresInClusters(ctx, &vc,
		spCtl.clusterIDs, true)
	validStoragePoolNames := make(map[string]bool)
	// Create StoragePools that are missing and add them to intendedStateMap.
	for clusterID, DSs := range clusterIDToDSs {
		for _, dsInfo := range DSs {
			validStoragePoolNames[makeStoragePoolName(dsInfo.Info.Name)] = true
			intendedState, err := newIntendedState(ctx, clusterID, dsInfo, scWatchCntlr)
			if err != nil {
				log.Errorf("Error reconciling StoragePool for datastore %s. Err: %v", dsInfo.Reference().Value, err)
				continue
			}

			err = spCtl.applyIntendedState(ctx, intendedState)
			if err != nil {
				log.Errorf("Error applying intended state of StoragePool %s. Err: %v", intendedState.spName, err)
				continue
			}

			// Create vsan-sna StoragePools for local vsan datastore.
			if intendedState.dsType == vsanDsType && !intendedState.isRemoteVsan {
				err := spCtl.updateVsanSnaIntendedState(ctx, intendedState, validStoragePoolNames, scWatchCntlr)
				if err != nil {
					log.Errorf("Error updating intended state of vSAN SNA StoragePools. Err: %v", err)
					continue
				}
			}
		}
	}

	// Delete unknown StoragePool instances owned by this driver.
	return deleteStoragePools(ctx, validStoragePoolNames, spCtl)
}

func deleteStoragePools(ctx context.Context, validStoragePoolNames map[string]bool, spCtl *SpController) error {
	log := logger.GetLogger(ctx)
	spClient, spResource, err := getSPClient(ctx)
	if err != nil {
		return err
	}
	// Delete unknown StoragePool instances owned by this driver.
	splist, err := spClient.Resource(*spResource).List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Errorf("Error getting list of StoragePool instances. Err: %v", err)
		return err
	}
	for _, sp := range splist.Items {
		spName := sp.GetName()
		if _, valid := validStoragePoolNames[spName]; !valid {
			err = deleteStoragePool(ctx, spName)
			if err != nil {
				log.Warnf("Error deleting StoragePool %s. Err: %s", spName, err)
			}

			// Also delete entry from intendedStateMap.
			spCtl.deleteIntendedState(ctx, spName)
		}
	}
	return nil
}
