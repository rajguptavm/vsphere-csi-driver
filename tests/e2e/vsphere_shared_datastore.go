/*
Copyright 2019 The Kubernetes Authors.

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

package e2e

import (
	"context"
	"fmt"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	fnodes "k8s.io/kubernetes/test/e2e/framework/node"
	fpod "k8s.io/kubernetes/test/e2e/framework/pod"
	fpv "k8s.io/kubernetes/test/e2e/framework/pv"
	admissionapi "k8s.io/pod-security-admission/api"
)

// Test to verify provisioning is dependant on type of datastore
// (shared/non-shared), when no storage policy is offered.
//
// Steps
// 1. Create StorageClass with shared/non-shared datastore.
// 2. Create PVC which uses the StorageClass created in step 1.
// 3. Expect:
//    3a. Volume provisioning to fail if non-shared datastore.
//    3b. Volume provisioning to pass if shared datastore.
//
// This test reads env
// 1. SHARED_VSPHERE_DATASTORE_URL (set to shared datastore URL).
// 2. NONSHARED_VSPHERE_DATASTORE_URL (set to non-shared datastor URL).

var _ = ginkgo.Describe("[csi-block-vanilla] [csi-block-vanilla-parallelized] "+
	"Datastore Based Volume Provisioning With No Storage Policy", func() {

	f := framework.NewDefaultFramework("e2e-vsphere-volume-provisioning-no-storage-policy")
	f.NamespacePodSecurityEnforceLevel = admissionapi.LevelPrivileged
	var (
		client                clientset.Interface
		namespace             string
		scParameters          map[string]string
		sharedDatastoreURL    string
		nonSharedDatastoreURL string
		storagePolicyName     string
	)
	ginkgo.BeforeEach(func() {
		client = f.ClientSet
		namespace = f.Namespace.Name
		bootstrap()
		scParameters = make(map[string]string)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		nodeList, err := fnodes.GetReadySchedulableNodes(ctx, f.ClientSet)
		storagePolicyName = GetAndExpectStringEnvVar(envStoragePolicyNameForSharedDatastores)
		framework.ExpectNoError(err, "Unable to find ready and schedulable Node")
		if !(len(nodeList.Items) > 0) {
			framework.Failf("Unable to find ready and schedulable Node")
		}
	})
	ginkgo.AfterEach(func() {
		if supervisorCluster {
			dumpSvcNsEventsOnTestFailure(client, namespace)
		}
		if guestCluster {
			svcClient, svNamespace := getSvcClientAndNamespace()
			dumpSvcNsEventsOnTestFailure(svcClient, svNamespace)
		}
	})

	// Shared datastore should be provisioned successfully.
	ginkgo.It("Verify dynamic provisioning of PV passes with user specified shared datastore and "+
		"no storage policy specified in the storage class", ginkgo.Label(p0, block, vanilla, vc70), func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ginkgo.By("Invoking Test for user specified Shared Datastore in Storage class for volume provisioning")
		sharedDatastoreURL = GetAndExpectStringEnvVar(envSharedDatastoreURL)
		scParameters[scParamDatastoreURL] = sharedDatastoreURL
		storageclass, pvclaim, err := createPVCAndStorageClass(ctx,
			client, namespace, nil, scParameters, "", nil, "", false, "")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		defer func() {
			err = client.StorageV1().StorageClasses().Delete(ctx, storageclass.Name, *metav1.NewDeleteOptions(0))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = fpv.DeletePersistentVolumeClaim(ctx, client, pvclaim.Name, namespace)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}()
		ginkgo.By("Expect claim to pass provisioning volume as shared datastore")
		err = fpv.WaitForPersistentVolumeClaimPhase(ctx,
			v1.ClaimBound, client, pvclaim.Namespace, pvclaim.Name, framework.Poll, framework.ClaimProvisionTimeout)
		gomega.Expect(err).NotTo(gomega.HaveOccurred(),
			fmt.Sprintf("Failed to provision volume on shared datastore with err: %v", err))
	})

	// Setting non-shared datastore in the storage class should fail dynamic
	// volume provisioning.
	ginkgo.It("Verify dynamic provisioning of PV fails with user specified non-shared datastore and "+
		"no storage policy specified in the storage class", ginkgo.Label(p0, block, vanilla, vc70), func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ginkgo.By("Invoking Test for user specified non-shared Datastore in storage class for volume provisioning")
		nonSharedDatastoreURL = GetAndExpectStringEnvVar(envNonSharedStorageClassDatastoreURL)
		scParameters[scParamDatastoreURL] = nonSharedDatastoreURL
		storageclass, pvclaim, err := createPVCAndStorageClass(ctx,
			client, namespace, nil, scParameters, "", nil, "", false, "")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		defer func() {
			err = client.StorageV1().StorageClasses().Delete(ctx, storageclass.Name, *metav1.NewDeleteOptions(0))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = fpv.DeletePersistentVolumeClaim(ctx, client, pvclaim.Name, namespace)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}()
		ginkgo.By("Expect claim to fail provisioning volume on non shared datastore")
		err = fpv.WaitForPersistentVolumeClaimPhase(ctx,
			v1.ClaimBound, client, pvclaim.Namespace, pvclaim.Name, framework.Poll, time.Minute/2)
		gomega.Expect(err).To(gomega.HaveOccurred())
		// eventList contains the events related to pvc.
		expectedErrMsg := "failed to provision volume with StorageClass \"" + storageclass.Name + "\""
		framework.Logf("Expected failure message: %+q", expectedErrMsg)
		errorOccurred := checkEventsforError(client, pvclaim.Namespace,
			metav1.ListOptions{FieldSelector: fmt.Sprintf("involvedObject.name=%s", pvclaim.Name)}, expectedErrMsg)
		gomega.Expect(errorOccurred).To(gomega.BeTrue())

	})

	// Verify impact on existing pv pvc when sc recreated with different binding mode
	// Steps
	// 1. Create a Storage Class with Immediate Binding Mode.
	// 2. Create a PVC using above SC.
	// 3. Wait for PVC to be in Bound phase.
	// 4. Create standalone pod
	// 5. Delete Storgae Class created above
	// 6. Recreate Storage Class with same name as above but with Binding mode WaitForFirstConsumer
	// 7. Check PVC status after recreating storage class
	// 8. Check pod status after recreating storage class
	// 9. Delete Pod
	// 10. Delete PVC and SC

	ginkgo.It("[csi-block-vanilla] [csi-guest] [csi-supervisor] "+
		"Verify impact on existing pv pvc when sc recreated with different binding mode", ginkgo.Label(p0,
		block, wcp, tkg, vanilla, vc70), func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ginkgo.By("Invoking Test to verify impact on existing pv pvc when sc recreated with different binding mode")
		var storageclass *storagev1.StorageClass
		var pvclaim *v1.PersistentVolumeClaim
		var err error
		if !vanillaCluster {
			storagePolicyName = GetAndExpectStringEnvVar(envStoragePolicyNameForSharedDatastores)
		}
		// Create Storage class and PVC
		ginkgo.By("Creating Storage Class and PVC")
		if guestCluster {
			scParameters[svStorageClassName] = storagePolicyName
			storageclass, pvclaim, err = createPVCAndStorageClass(ctx, client,
				namespace, nil, scParameters, diskSize, nil, "", false, "")
		} else if supervisorCluster {
			namespace = getNamespaceToRunTests(f)
			profileID := e2eVSphere.GetSpbmPolicyID(storagePolicyName)
			scParameters[scParamStoragePolicyID] = profileID
			storageclass, pvclaim, err = createPVCAndStorageClass(ctx, client,
				namespace, nil, scParameters, diskSize, nil, "", true, "", storagePolicyName)
		} else if vanillaCluster {
			scParameters[scParamFsType] = ext4FSType
			storageclass, pvclaim, err = createPVCAndStorageClass(ctx, client,
				namespace, nil, scParameters, "", nil, "", false, "")
		}
		framework.Logf("storageclass name :%s", storageclass.GetName())
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		defer func() {
			err := client.StorageV1().StorageClasses().Delete(ctx, storageclass.Name, *metav1.NewDeleteOptions(0))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}()
		defer func() {
			err := fpv.DeletePersistentVolumeClaim(ctx, client, pvclaim.Name, namespace)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			if supervisorCluster {
				ginkgo.By("Delete Resource quota")
				deleteResourceQuota(client, namespace)
			}
		}()

		// Waiting for PVC to be bound
		var pvclaims []*v1.PersistentVolumeClaim
		pvclaims = append(pvclaims, pvclaim)
		ginkgo.By("Waiting for all claims to be in bound state")
		persistentvolumes, err := fpv.WaitForPVClaimBoundPhase(ctx, client, pvclaims, framework.ClaimProvisionTimeout)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		pv := persistentvolumes[0]
		volHandle := pv.Spec.CSI.VolumeHandle
		if guestCluster {
			volHandle = getVolumeIDFromSupervisorCluster(volHandle)
			gomega.Expect(volHandle).NotTo(gomega.BeEmpty())
		}

		// Create a Pod to use this PVC, and verify volume has been attached
		ginkgo.By("Creating pod to attach PV to the node")
		pod, err := createPod(ctx, client, namespace, nil, []*v1.PersistentVolumeClaim{pvclaim}, false, "")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		var vmUUID string
		var exists bool
		ginkgo.By(fmt.Sprintf("Verify volume: %s is attached to the node: %s", volHandle, pod.Spec.NodeName))
		if vanillaCluster {
			vmUUID = getNodeUUID(ctx, client, pod.Spec.NodeName)
		} else if guestCluster {
			vmUUID, err = getVMUUIDFromNodeName(pod.Spec.NodeName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		} else {
			annotations := pod.Annotations
			vmUUID, exists = annotations[vmUUIDLabel]
			gomega.Expect(exists).To(gomega.BeTrue(), fmt.Sprintf("Pod doesn't have %s annotation", vmUUIDLabel))
		}
		framework.Logf("VMUUID : %s", vmUUID)

		isDiskAttached, err := e2eVSphere.isVolumeAttachedToVM(client, volHandle, vmUUID)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(isDiskAttached).To(gomega.BeTrue(), "Volume is not attached to the node")
		defer func() {
			ginkgo.By("Deleting the pod")
			err = fpod.DeletePodWithWait(ctx, client, pod)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			ginkgo.By("Verify volume is detached from the node")
			if supervisorCluster {
				ginkgo.By(fmt.Sprintf("Verify volume: %s is detached from PodVM with vmUUID: %s",
					pv.Spec.CSI.VolumeHandle, vmUUID))
				_, err := e2eVSphere.getVMByUUIDWithWait(ctx, vmUUID, supervisorClusterOperationsTimeout)
				gomega.Expect(err).To(gomega.HaveOccurred(),
					fmt.Sprintf("PodVM with vmUUID: %s still exists. So volume: %s is not detached from the PodVM",
						vmUUID, pv.Spec.CSI.VolumeHandle))
			} else {
				isDiskDetached, err := e2eVSphere.waitForVolumeDetachedFromNode(
					client, pv.Spec.CSI.VolumeHandle, pod.Spec.NodeName)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(isDiskDetached).To(gomega.BeTrue(),
					fmt.Sprintf("Volume %q is not detached from the node %q", pv.Spec.CSI.VolumeHandle, pod.Spec.NodeName))
			}
		}()

		// Delete SC with Immediate Binding Mode
		ginkgo.By("Delete SC created with Immediate Binding Mode")
		err = client.StorageV1().StorageClasses().Delete(ctx, storageclass.Name, *metav1.NewDeleteOptions(0))
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// Create SC with same name but with WaitForFirstConusmer Binding Mode
		ginkgo.By("Recreate SC with same name but with WaitForFirstConusmer Binding Mode")
		storageclass, err = createStorageClass(client, scParameters, nil, "",
			storagev1.VolumeBindingWaitForFirstConsumer, false, storageclass.Name)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		framework.Logf("storageclass name :%s", storageclass.GetName())
		defer func() {
			if supervisorCluster {
				/* Cannot Update SC binding mode from WaitForFirstConsumer to Immediate
				because it is an immutable field */
				// If Supervisor Cluster, delete SC and recreate again with Immediate Binding Mode
				err := client.StorageV1().StorageClasses().Delete(ctx, storageclass.Name, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				_, err = createStorageClass(client, scParameters, nil, "",
					storagev1.VolumeBindingImmediate, false, storageclass.Name)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			}
		}()

		// Check Pod status after recreating Storage class with different binding mode
		ginkgo.By("Verify Pod status after recreating Storage class with different binding mode")
		pod_status := fpod.VerifyPodsRunning(ctx, client, namespace, pod.Name,
			labels.SelectorFromSet(map[string]string{"name": pod.Name}), true, 0)
		if pod.Status.Phase == v1.PodRunning {
			framework.Logf("Pod is in Running state after recreating Storage Class")
		}
		gomega.Expect(pod_status).NotTo(gomega.HaveOccurred())

		// Check PVC status after recreating Storage class with different binding mode
		ginkgo.By("Verify PVC status after recreating Storage class with different binding mode")
		pvclaim, err = checkPvcHasGivenStatusCondition(client,
			namespace, pvclaim.Name, false, "")
		if v1.PersistentVolumePhase(pvclaim.Status.Phase) == v1.VolumeBound {
			framework.Logf("PVC is in Bound Phase after recreating Storage Class")
		}
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(pvclaim).NotTo(gomega.BeNil())
	})
})
