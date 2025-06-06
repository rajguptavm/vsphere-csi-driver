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

package vanilla

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/vmware/govmomi/cns"
	cnstypes "github.com/vmware/govmomi/cns/types"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/pbm"
	"github.com/vmware/govmomi/pbm/types"
	"github.com/vmware/govmomi/vim25"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/container-storage-interface/spec/lib/go/csi"
	clientset "k8s.io/client-go/kubernetes"
	testclient "k8s.io/client-go/kubernetes/fake"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/vsphere-csi-driver/v3/pkg/common/cns-lib/node"
	cnsvolume "sigs.k8s.io/vsphere-csi-driver/v3/pkg/common/cns-lib/volume"
	cnsvsphere "sigs.k8s.io/vsphere-csi-driver/v3/pkg/common/cns-lib/vsphere"
	"sigs.k8s.io/vsphere-csi-driver/v3/pkg/common/config"
	"sigs.k8s.io/vsphere-csi-driver/v3/pkg/common/unittestcommon"
	"sigs.k8s.io/vsphere-csi-driver/v3/pkg/csi/service/common"
	"sigs.k8s.io/vsphere-csi-driver/v3/pkg/csi/service/common/commonco"
	"sigs.k8s.io/vsphere-csi-driver/v3/pkg/internalapis/cnsvolumeoperationrequest"
	k8s "sigs.k8s.io/vsphere-csi-driver/v3/pkg/kubernetes"
)

const (
	testVolumeName             = "test-pvc"
	testClusterName            = "test-cluster"
	simulateNoSharedDatastores = "no-shared-datastores"
)

var (
	ctx                    context.Context
	controllerTestInstance *controllerTest
	onceForControllerTest  sync.Once
)

type controllerTest struct {
	controller *controller
	config     *config.Config
	vcenter    *cnsvsphere.VirtualCenter
	// Add a VolumeOperationRequest interface to set up certain test scenario
	operationStore cnsvolumeoperationrequest.VolumeOperationRequest
}

type FakeNodeManager struct {
	cnsNodeManager node.Manager
	k8sClient      clientset.Interface
	vimClient      *vim25.Client
}

type FakeAuthManager struct {
	vcenter *cnsvsphere.VirtualCenter
}

func (f *FakeNodeManager) Initialize(ctx context.Context) error {
	f.cnsNodeManager = node.GetManager(ctx)
	f.cnsNodeManager.SetKubernetesClient(f.k8sClient)
	var t *testing.T

	objVMs, err := find.NewFinder(f.vimClient).VirtualMachineList(ctx, "*")
	if err != nil {
		return err
	}
	var i int
	for _, vm := range objVMs {
		i++
		nodeUUID := vm.UUID(ctx)
		nodeName := "k8s-node-" + strconv.Itoa(i)
		err := f.cnsNodeManager.RegisterNode(ctx, nodeUUID, nodeName)
		if err != nil {
			t.Errorf("Error occurred while registering a node: %s, nodeUUID: %s, err: %v", nodeName, nodeUUID, err)
			return err
		}
	}
	return nil
}

func (f *FakeNodeManager) GetSharedDatastoresInK8SCluster(ctx context.Context) ([]*cnsvsphere.DatastoreInfo, error) {
	// Return error that no shared datastores found for a negative test case
	if v := os.Getenv("VSPHERE_DATASTORE_URL"); v == simulateNoSharedDatastores {
		return nil, errors.New("No shared datastores found in the cluster.")
	}

	var t *testing.T
	nodeVMs, err := f.cnsNodeManager.GetAllNodes(ctx)
	if err != nil {
		t.Errorf("failed to get Nodes from nodeManager with err %v", err)
		return nil, err
	}

	if len(nodeVMs) == 0 {
		errMsg := "empty List of Node VMs received from nodeManager"
		t.Errorf("%s", errMsg)
		return nil, fmt.Errorf("%s", errMsg)
	}

	sharedDatastores, err := cnsvsphere.GetSharedDatastoresForVMs(ctx, nodeVMs)
	if err != nil {
		t.Errorf("failed to get shared datastores for node VMs. Err: %+v", err)
		return nil, err
	}
	fmt.Printf("GetSharedDatastoresInK8SCluster, sharedDatastores= %+v\n", sharedDatastores)
	return sharedDatastores, nil
}

func (f *FakeNodeManager) GetNodeVMByNameAndUpdateCache(ctx context.Context,
	nodeName string) (*cnsvsphere.VirtualMachine, error) {
	return f.cnsNodeManager.GetNodeVMByNameAndUpdateCache(ctx, nodeName)
}

func (f *FakeNodeManager) GetNodeVMByNameOrUUID(
	ctx context.Context, nodeNameOrUUID string) (*cnsvsphere.VirtualMachine, error) {
	return f.cnsNodeManager.GetNodeVMByNameOrUUID(ctx, nodeNameOrUUID)
}

func (f *FakeNodeManager) GetNodeNameByUUID(ctx context.Context, nodeUUID string) (string, error) {
	return f.cnsNodeManager.GetNodeNameByUUID(ctx, nodeUUID)
}

func (f *FakeNodeManager) GetNodeVMByUuid(ctx context.Context, nodeUUID string) (*cnsvsphere.VirtualMachine, error) {
	return f.cnsNodeManager.GetNodeVMByUuid(ctx, nodeUUID)
}

func (f *FakeNodeManager) GetAllNodes(ctx context.Context) ([]*cnsvsphere.VirtualMachine, error) {
	return f.cnsNodeManager.GetAllNodes(ctx)
}

func (f *FakeNodeManager) GetAllNodesByVC(ctx context.Context, vcHost string) ([]*cnsvsphere.VirtualMachine, error) {
	// This function is required only for multi VC env.
	return nil, nil
}

func (f *FakeAuthManager) GetDatastoreMapForBlockVolumes(ctx context.Context) map[string]*cnsvsphere.DatastoreInfo {
	fmt.Print("FakeAuthManager: GetDatastoreMapForBlockVolumes")
	datastoreMapForBlockVolumes, _ := common.GenerateDatastoreMapForBlockVolumes(ctx, f.vcenter)
	return datastoreMapForBlockVolumes
}

func (f *FakeAuthManager) GetFsEnabledClusterToDsMap(ctx context.Context) map[string][]*cnsvsphere.DatastoreInfo {
	fsEnabledClusterToDsMap := make(map[string][]*cnsvsphere.DatastoreInfo)
	fmt.Print("FakeAuthManager: GetClusterToFsEnabledDsMap")
	if v := os.Getenv("VSPHERE_DATACENTER"); v != "" {
		fsEnabledClusterToDsMap, _ := common.GenerateFSEnabledClustersToDsMap(ctx, f.vcenter)
		return fsEnabledClusterToDsMap
	}
	return fsEnabledClusterToDsMap
}

func (f *FakeAuthManager) ResetvCenterInstance(ctx context.Context, vCenter *cnsvsphere.VirtualCenter) {
	f.vcenter = vCenter
}

var vcsimParams = unittestcommon.VcsimParams{
	Datacenters:     1,
	Clusters:        1,
	HostsPerCluster: 2,
	VMsPerCluster:   2,
	StandaloneHosts: 0,
	Datastores:      1,
	Version:         "7.0.3",
	ApiVersion:      "7.0",
}

func getControllerTest(t *testing.T) *controllerTest {
	onceForControllerTest.Do(func() {
		// Create context.
		ctx = context.Background()
		config, _ := unittestcommon.ConfigFromEnvOrVCSim(ctx, vcsimParams, false)

		// CNS based CSI requires a valid cluster name.
		config.Global.ClusterID = testClusterName

		vcenterconfig, err := cnsvsphere.GetVirtualCenterConfig(ctx, config)
		if err != nil {
			t.Fatal(err)
		}
		vcManager := cnsvsphere.GetVirtualCenterManager(ctx)
		vcenter, err := vcManager.RegisterVirtualCenter(ctx, vcenterconfig)
		if err != nil {
			t.Fatal(err)
		}

		err = vcenter.ConnectCns(ctx)
		if err != nil {
			t.Fatal(err)
		}
		fakeOpStore, err := unittestcommon.InitFakeVolumeOperationRequestInterface()
		if err != nil {
			t.Fatal(err)
		}

		volumeManager, err := cnsvolume.GetManager(ctx, vcenter,
			fakeOpStore, true, false,
			false, cnstypes.CnsClusterFlavorVanilla)
		if err != nil {
			t.Fatalf("failed to create an instance of volume manager. err=%v", err)
		}

		manager := &common.Manager{
			VcenterConfig:  vcenterconfig,
			CnsConfig:      config,
			VolumeManager:  volumeManager,
			VcenterManager: cnsvsphere.GetVirtualCenterManager(ctx),
		}

		var k8sClient clientset.Interface
		if k8senv := os.Getenv("KUBECONFIG"); k8senv != "" {
			k8sClient, err = k8s.CreateKubernetesClientFromConfig(k8senv)
			if err != nil {
				t.Fatal(err)
			}
		} else {
			k8sClient = testclient.NewSimpleClientset()
		}

		nodeManager := &FakeNodeManager{
			k8sClient: k8sClient,
			vimClient: vcenter.Client.Client,
		}
		err = nodeManager.Initialize(ctx)
		if err != nil {
			t.Fatalf("Failed to initialize node manager, err = %v", err)
		}

		c := &controller{
			manager: manager,
			nodeMgr: nodeManager,
			authMgr: &FakeAuthManager{
				vcenter: vcenter,
			},
		}

		commonco.ContainerOrchestratorUtility, err =
			unittestcommon.GetFakeContainerOrchestratorInterface(common.Kubernetes)
		if err != nil {
			t.Fatalf("Failed to create co agnostic interface. err=%v", err)
		}
		controllerTestInstance = &controllerTest{
			controller:     c,
			config:         config,
			vcenter:        vcenter,
			operationStore: fakeOpStore,
		}
	})
	return controllerTestInstance
}

func TestCreateVolumeWithStoragePolicy(t *testing.T) {
	// Create context.
	ct := getControllerTest(t)

	// Create.
	params := make(map[string]string)
	if v := os.Getenv("VSPHERE_DATASTORE_URL"); v != "" {
		params[common.AttributeDatastoreURL] = v
	}

	// PBM simulator defaults.
	params[common.AttributeStoragePolicyName] = "vSAN Default Storage Policy"
	if v := os.Getenv("VSPHERE_STORAGE_POLICY_NAME"); v != "" {
		params[common.AttributeStoragePolicyName] = v
	}
	capabilities := []*csi.VolumeCapability{
		{
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
	}

	reqCreate := &csi.CreateVolumeRequest{
		Name: testVolumeName + "-" + uuid.New().String(),
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 1 * common.GbInBytes,
		},
		Parameters:         params,
		VolumeCapabilities: capabilities,
	}

	respCreate, err := ct.controller.CreateVolume(ctx, reqCreate)
	if err != nil {
		t.Fatal(err)
	}
	volID := respCreate.Volume.VolumeId
	// Verify the volume has been created with corresponding storage policy ID.
	pc, err := pbm.NewClient(ctx, ct.vcenter.Client.Client)
	if err != nil {
		t.Fatal(err)
	}

	profileID, err := pc.ProfileIDByName(ctx, params[common.AttributeStoragePolicyName])
	if err != nil {
		t.Fatal(err)
	}

	queryFilter := cnstypes.CnsQueryFilter{
		VolumeIds: []cnstypes.CnsVolumeId{
			{
				Id: volID,
			},
		},
	}
	queryResult, err := ct.vcenter.CnsClient.QueryVolume(ctx, queryFilter)
	if err != nil {
		t.Fatal(err)
	}

	if len(queryResult.Volumes) != 1 && queryResult.Volumes[0].VolumeId.Id != volID {
		t.Fatalf("failed to find the newly created volume with ID: %s", volID)
	}

	if queryResult.Volumes[0].StoragePolicyId != profileID {
		t.Fatalf("failed to match volume policy ID: %s", profileID)
	}

	// QueryAll.
	queryFilter = cnstypes.CnsQueryFilter{
		VolumeIds: []cnstypes.CnsVolumeId{
			{
				Id: volID,
			},
		},
	}
	querySelection := cnstypes.CnsQuerySelection{}
	queryResult, err = ct.vcenter.CnsClient.QueryAllVolume(ctx, queryFilter, querySelection)
	if err != nil {
		t.Fatal(err)
	}

	if len(queryResult.Volumes) != 1 && queryResult.Volumes[0].VolumeId.Id != volID {
		t.Fatalf("failed to find the newly created volume with ID: %s", volID)
	}

	// Delete.
	reqDelete := &csi.DeleteVolumeRequest{
		VolumeId: volID,
	}
	_, err = ct.controller.DeleteVolume(ctx, reqDelete)
	if err != nil {
		t.Fatal(err)
	}

	// Verify the volume has been deleted.
	queryResult, err = ct.vcenter.CnsClient.QueryVolume(ctx, queryFilter)
	if err != nil {
		t.Fatal(err)
	}

	if len(queryResult.Volumes) != 0 {
		t.Fatalf("Volume should not exist after deletion with ID: %s", volID)
	}
}

// Create new Storage Policy and pass this storage policy's name through parameters to CreateVolume
// function call. Verify that volume creation succeeds and it uses newly created storage policy.
func TestCreateVolumeWithNewlyCreatedStoragePolicy(t *testing.T) {
	// Create context.
	ct := getControllerTest(t)

	params := make(map[string]string)

	pc, err := pbm.NewClient(ctx, ct.vcenter.Client.Client)
	if err != nil {
		t.Fatal(err)
	}

	// Create new Storage Policy
	storagePolicyName := "Kubernetes-VSAN-TestPolicy"
	pbmCreateSpecForVSAN := pbm.CapabilityProfileCreateSpec{
		Name:        storagePolicyName,
		Description: "VSAN Test policy create",
		Category:    string(types.PbmProfileCategoryEnumREQUIREMENT),
		CapabilityList: []pbm.Capability{
			{
				ID:        "hostFailuresToTolerate",
				Namespace: "VSAN",
				PropertyList: []pbm.Property{
					{
						ID:       "hostFailuresToTolerate",
						Value:    "2",
						DataType: "int",
					},
				},
			},
			{
				ID:        "stripeWidth",
				Namespace: "VSAN",
				PropertyList: []pbm.Property{
					{
						ID:       "stripeWidth",
						Value:    "1",
						DataType: "int",
					},
				},
			},
		},
	}

	// Create PBM capability spec for the above defined user spec
	createSpecVSAN, err := pbm.CreateCapabilityProfileSpec(pbmCreateSpecForVSAN)
	if err != nil {
		t.Fatal(err)
	}

	// Create SPBM VSAN profile
	vsanProfileID, err := pc.CreateProfile(ctx, *createSpecVSAN)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("VSAN Storage Policy %q successfully created", vsanProfileID.UniqueId)

	defer func() {
		_, err = pc.DeleteProfile(ctx, []types.PbmProfileId{*vsanProfileID})
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("VSAN Storage Policy %+v successfully deleted", vsanProfileID.UniqueId)
	}()

	// Verify if profile created exists by issuing a PbmRetrieveContent request
	_, err = ct.vcenter.PbmRetrieveContent(ctx, []string{vsanProfileID.UniqueId})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Storage Policy %q exists on vCenter", vsanProfileID.UniqueId)

	params[common.AttributeStoragePolicyName] = storagePolicyName
	capabilities := []*csi.VolumeCapability{
		{
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
	}

	reqCreate := &csi.CreateVolumeRequest{
		Name: testVolumeName + "-" + uuid.New().String(),
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 1 * common.GbInBytes,
		},
		Parameters:         params,
		VolumeCapabilities: capabilities,
	}

	// Create volume.
	// As part of create volume PbmCheckCompatibility and GetStoragePolicyIDByName
	// functions will get tested.
	respCreate, err := ct.controller.CreateVolume(ctx, reqCreate)
	if err != nil {
		t.Fatal(err)
	}
	volID := respCreate.Volume.VolumeId

	queryFilter := cnstypes.CnsQueryFilter{
		VolumeIds: []cnstypes.CnsVolumeId{
			{
				Id: volID,
			},
		},
	}
	queryResult, err := ct.vcenter.CnsClient.QueryVolume(ctx, queryFilter)
	if err != nil {
		t.Fatal(err)
	}

	if len(queryResult.Volumes) != 1 && queryResult.Volumes[0].VolumeId.Id != volID {
		t.Fatalf("failed to find the newly created volume with ID: %s", volID)
	}

	// Make sure that volume is using the newly created storage profile
	if queryResult.Volumes[0].StoragePolicyId != vsanProfileID.UniqueId {
		t.Fatalf("failed to match volume policy ID: %s", vsanProfileID.UniqueId)
	}

	// Delete volume
	reqDelete := &csi.DeleteVolumeRequest{
		VolumeId: volID,
	}
	_, err = ct.controller.DeleteVolume(ctx, reqDelete)
	if err != nil {
		t.Fatal(err)
	}

	// Verify the volume has been deleted.
	queryResult, err = ct.vcenter.CnsClient.QueryVolume(ctx, queryFilter)
	if err != nil {
		t.Fatal(err)
	}

	if len(queryResult.Volumes) != 0 {
		t.Fatalf("Volume should not exist after deletion with ID: %s", volID)
	}
}

// When the testbed has multiple shared datastores, but VC user which is used
// to deploy CSI does not have Datastore.FileManagement privilege on all shared
// datastores, the create volume should succeed. This test is to simulate CSI
// on VMC.
func TestCreateVolumeWithMultipleDatastores(t *testing.T) {
	// Create context.
	ct := getControllerTest(t)

	// Create.
	params := make(map[string]string)

	capabilities := []*csi.VolumeCapability{
		{
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
	}

	reqCreate := &csi.CreateVolumeRequest{
		Name: testVolumeName + "-" + uuid.New().String(),
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 1 * common.GbInBytes,
		},
		Parameters:         params,
		VolumeCapabilities: capabilities,
	}

	respCreate, err := ct.controller.CreateVolume(ctx, reqCreate)
	if err != nil {
		t.Fatal(err)
	}
	volID := respCreate.Volume.VolumeId

	queryFilter := cnstypes.CnsQueryFilter{
		VolumeIds: []cnstypes.CnsVolumeId{
			{
				Id: volID,
			},
		},
	}
	queryResult, err := ct.vcenter.CnsClient.QueryVolume(ctx, queryFilter)
	if err != nil {
		t.Fatal(err)
	}

	if len(queryResult.Volumes) != 1 && queryResult.Volumes[0].VolumeId.Id != volID {
		t.Fatalf("failed to find the newly created volume with ID: %s", volID)
	}

	// Delete.
	reqDelete := &csi.DeleteVolumeRequest{
		VolumeId: volID,
	}
	_, err = ct.controller.DeleteVolume(ctx, reqDelete)
	if err != nil {
		t.Fatal(err)
	}

	// Verify the volume has been deleted.
	queryResult, err = ct.vcenter.CnsClient.QueryVolume(ctx, queryFilter)
	if err != nil {
		t.Fatal(err)
	}

	if len(queryResult.Volumes) != 0 {
		t.Fatalf("Volume should not exist after deletion with ID: %s", volID)
	}
}

func TestCreateVolumeWithInvalidAccessibilityRequirements(t *testing.T) {
	// Create context.
	ct := getControllerTest(t)

	// Create.
	params := make(map[string]string)

	capabilities := []*csi.VolumeCapability{
		{
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
	}

	reqCreate := &csi.CreateVolumeRequest{
		Name: testVolumeName + "-" + uuid.New().String(),
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 1 * common.GbInBytes,
		},
		Parameters:         params,
		VolumeCapabilities: capabilities,
		AccessibilityRequirements: &csi.TopologyRequirement{
			Requisite: []*csi.Topology{
				{
					Segments: map[string]string{
						"topology.csi.vmware.com/k8s-zone": "zone-A",
					},
				},
			},
			Preferred: []*csi.Topology{},
		},
	}

	_, err := ct.controller.CreateVolume(ctx, reqCreate)
	if err != nil {
		createErr, ok := status.FromError(err)
		if !ok {
			t.Fatalf("unable to convert the error: %+v into a grpc status error type", err)
		}
		if createErr.Code() == codes.InvalidArgument && strings.Contains(createErr.Message(),
			"topology category names not specified in the vsphere config secret") {
			t.Logf("Received expected error since topology category names are not specified in the vSphere config " +
				"but AccessiblityRequirements were provided in the CreateVolume request")
		} else {
			t.Fatalf("unexpected error received, code: %s, message: %s",
				createErr.Code().String(), createErr.Message())
		}
	} else {
		t.Fatalf("We should get failure while creating a volume, as topology category names are not specified in " +
			"the vSphere config but AccessiblityRequirements were provided in the CreateVolume request")
	}
}

// This is a negative test case. Simulate the case that there are no shared datastores in the K8s cluster
// and make sure that CreateVolume fails with the expected error.
func TestCreateVolumeWithNoSharedDatastores(t *testing.T) {
	// Set environment variable which indicates that there are no shared datastores
	// in K8s cluster
	os.Setenv("VSPHERE_DATASTORE_URL", simulateNoSharedDatastores)
	defer os.Unsetenv("VSPHERE_DATASTORE_URL")
	// Create context.
	ct := getControllerTest(t)

	// Create.
	params := make(map[string]string)

	capabilities := []*csi.VolumeCapability{
		{
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
	}

	reqCreate := &csi.CreateVolumeRequest{
		Name: testVolumeName + "-" + uuid.New().String(),
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 1 * common.GbInBytes,
		},
		Parameters:         params,
		VolumeCapabilities: capabilities,
	}

	_, err := ct.controller.CreateVolume(ctx, reqCreate)
	if err != nil {
		createErr, ok := status.FromError(err)
		if !ok {
			t.Fatalf("unable to convert the error: %+v into a grpc status error type", err)
		}
		if createErr.Code() == codes.Internal && strings.Contains(createErr.Message(),
			"failed to get shared datastores in kubernetes cluster") {
			t.Logf("Received expected error since we are simulating that there are no shared " +
				"datastores in K8s cluster")
		} else {
			t.Fatalf("unexpected error received, code: %s, message: %s",
				createErr.Code().String(), createErr.Message())
		}
	} else {
		t.Fatalf("We should get failure while creating a volume, as we are simulating that there are no shared " +
			"datastores in K8s cluster")
	}
}

func TestExtendVolume(t *testing.T) {
	ct := getControllerTest(t)

	// Create.
	params := make(map[string]string)
	if v := os.Getenv("VSPHERE_DATASTORE_URL"); v != "" {
		params[common.AttributeDatastoreURL] = v
	}
	capabilities := []*csi.VolumeCapability{
		{
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
	}

	reqCreate := &csi.CreateVolumeRequest{
		Name: testVolumeName + "-" + uuid.New().String(),
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 1 * common.GbInBytes,
		},
		Parameters:         params,
		VolumeCapabilities: capabilities,
	}

	respCreate, err := ct.controller.CreateVolume(ctx, reqCreate)
	if err != nil {
		t.Fatal(err)
	}
	volID := respCreate.Volume.VolumeId

	// Verify the volume has been created.
	queryFilter := cnstypes.CnsQueryFilter{
		VolumeIds: []cnstypes.CnsVolumeId{
			{
				Id: volID,
			},
		},
	}
	queryResult, err := ct.vcenter.CnsClient.QueryVolume(ctx, queryFilter)
	if err != nil {
		t.Fatal(err)
	}

	if len(queryResult.Volumes) != 1 && queryResult.Volumes[0].VolumeId.Id != volID {
		t.Fatalf("failed to find the newly created volume with ID: %s", volID)
	}

	// QueryAll.
	queryFilter = cnstypes.CnsQueryFilter{
		VolumeIds: []cnstypes.CnsVolumeId{
			{
				Id: volID,
			},
		},
	}
	querySelection := cnstypes.CnsQuerySelection{}
	queryResult, err = ct.vcenter.CnsClient.QueryAllVolume(ctx, queryFilter, querySelection)
	if err != nil {
		t.Fatal(err)
	}

	if len(queryResult.Volumes) != 1 && queryResult.Volumes[0].VolumeId.Id != volID {
		t.Fatalf("failed to find the newly created volume with ID: %s", volID)
	}

	// Extend Volume.
	newSize := 2 * common.GbInBytes
	reqExpand := &csi.ControllerExpandVolumeRequest{
		VolumeId: volID,
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: newSize,
		},
		VolumeCapability: capabilities[0],
	}
	t.Logf("ControllerExpandVolume will be called with req +%v", *reqExpand)
	respExpand, err := ct.controller.ControllerExpandVolume(ctx, reqExpand)
	if err != nil {
		t.Fatal(err)
	}
	if respExpand.CapacityBytes < newSize {
		t.Fatalf("newly expanded volume size %d is smaller than requested size %d for volume with ID: %s",
			respExpand.CapacityBytes, newSize, volID)
	}
	t.Logf("ControllerExpandVolume succeeded: volume is expanded to requested size %d", newSize)

	// Query volume after expand volume.
	queryFilter = cnstypes.CnsQueryFilter{
		VolumeIds: []cnstypes.CnsVolumeId{
			{
				Id: volID,
			},
		},
	}
	queryResult, err = ct.vcenter.CnsClient.QueryVolume(ctx, queryFilter)
	if err != nil {
		t.Fatal(err)
	}

	if len(queryResult.Volumes) != 1 && queryResult.Volumes[0].VolumeId.Id != volID {
		t.Fatalf("failed to find the expanded volume with ID: %s", volID)
	}

	// Delete.
	reqDelete := &csi.DeleteVolumeRequest{
		VolumeId: volID,
	}
	_, err = ct.controller.DeleteVolume(ctx, reqDelete)
	if err != nil {
		t.Fatal(err)
	}

	// Verify the volume has been deleted.
	queryResult, err = ct.vcenter.CnsClient.QueryVolume(ctx, queryFilter)
	if err != nil {
		t.Fatal(err)
	}

	if len(queryResult.Volumes) != 0 {
		t.Fatalf("Volume should not exist after deletion with ID: %s", volID)
	}
}

// TestMigratedExtendVolume helps test ControllerExpandVolume with VolumeId
// having migrated volume.
func TestMigratedExtendVolume(t *testing.T) {
	ct := getControllerTest(t)
	reqExpand := &csi.ControllerExpandVolumeRequest{
		VolumeId: "[vsanDatastore] 08281a5f-a21d-1eff-62d6-02009d0f19a1/004dbb1694f14e3598abef852b113e3b.vmdk",
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 1024,
		},
	}
	t.Logf("ControllerExpandVolume will be called with req +%v", *reqExpand)
	_, err := ct.controller.ControllerExpandVolume(ctx, reqExpand)
	if err != nil {
		t.Logf("Expected error received. migrated volume with VMDK path can not be expanded")
	} else {
		t.Fatal("Expected error not received when ControllerExpandVolume is called with volume having vmdk path")
	}
}

func TestCompleteControllerFlow(t *testing.T) {
	ct := getControllerTest(t)

	// Create.
	params := make(map[string]string)
	if v := os.Getenv("VSPHERE_DATASTORE_URL"); v != "" {
		params[common.AttributeDatastoreURL] = v
	}
	capabilities := []*csi.VolumeCapability{
		{
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
	}

	reqCreate := &csi.CreateVolumeRequest{
		Name: testVolumeName + "-" + uuid.New().String(),
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 1 * common.GbInBytes,
		},
		Parameters:         params,
		VolumeCapabilities: capabilities,
	}

	respCreate, err := ct.controller.CreateVolume(ctx, reqCreate)
	if err != nil {
		t.Fatal(err)
	}
	volID := respCreate.Volume.VolumeId

	// Verify the volume has been created.
	queryFilter := cnstypes.CnsQueryFilter{
		VolumeIds: []cnstypes.CnsVolumeId{
			{
				Id: volID,
			},
		},
	}
	queryResult, err := ct.vcenter.CnsClient.QueryVolume(ctx, queryFilter)
	if err != nil {
		t.Fatal(err)
	}

	if len(queryResult.Volumes) != 1 && queryResult.Volumes[0].VolumeId.Id != volID {
		t.Fatalf("failed to find the newly created volume with ID: %s", volID)
	}

	// QueryAll.
	queryFilter = cnstypes.CnsQueryFilter{
		VolumeIds: []cnstypes.CnsVolumeId{
			{
				Id: volID,
			},
		},
	}
	querySelection := cnstypes.CnsQuerySelection{}
	queryResult, err = ct.vcenter.CnsClient.QueryAllVolume(ctx, queryFilter, querySelection)
	if err != nil {
		t.Fatal(err)
	}

	if len(queryResult.Volumes) != 1 && queryResult.Volumes[0].VolumeId.Id != volID {
		t.Fatalf("failed to find the newly created volume with ID: %s", volID)
	}

	var NodeID string
	if v := os.Getenv("VSPHERE_K8S_NODE"); v != "" {
		NodeID = v
	} else {
		vms, err := find.NewFinder(ct.vcenter.Client.Client).VirtualMachineList(ctx, "*")
		if err != nil {
			t.Fatal(err)
		}
		NodeID = vms[0].UUID(ctx)
	}

	// Attach.
	reqControllerPublishVolume := &csi.ControllerPublishVolumeRequest{
		VolumeId:         volID,
		NodeId:           NodeID,
		VolumeCapability: capabilities[0],
		Readonly:         false,
	}
	t.Logf("ControllerPublishVolume will be called with req +%v", *reqControllerPublishVolume)
	respControllerPublishVolume, err := ct.controller.ControllerPublishVolume(ctx, reqControllerPublishVolume)
	if err != nil {
		t.Fatal(err)
	}
	diskUUID := respControllerPublishVolume.PublishContext[common.AttributeFirstClassDiskUUID]
	t.Logf("ControllerPublishVolume succeed, diskUUID %s is returned", diskUUID)

	// Detach.
	reqControllerUnpublishVolume := &csi.ControllerUnpublishVolumeRequest{
		VolumeId: volID,
		NodeId:   NodeID,
	}
	t.Logf("ControllerUnpublishVolume will be called with req +%v", *reqControllerUnpublishVolume)
	_, err = ct.controller.ControllerUnpublishVolume(ctx, reqControllerUnpublishVolume)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("ControllerUnpublishVolume succeed")

	// Delete.
	reqDelete := &csi.DeleteVolumeRequest{
		VolumeId: volID,
	}
	_, err = ct.controller.DeleteVolume(ctx, reqDelete)
	if err != nil {
		t.Fatal(err)
	}

	// Verify the volume has been deleted.
	queryResult, err = ct.vcenter.CnsClient.QueryVolume(ctx, queryFilter)
	if err != nil {
		t.Fatal(err)
	}

	if len(queryResult.Volumes) != 0 {
		t.Fatalf("Volume should not exist after deletion with ID: %s", volID)
	}
}

func TestDeleteVolumeWithSnapshots(t *testing.T) {
	ct := getControllerTest(t)

	// Create.
	params := make(map[string]string)
	if v := os.Getenv("VSPHERE_DATASTORE_URL"); v != "" {
		params[common.AttributeDatastoreURL] = v
	}
	capabilities := []*csi.VolumeCapability{
		{
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
	}

	reqCreate := &csi.CreateVolumeRequest{
		Name: testVolumeName + "-" + uuid.New().String(),
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 1 * common.GbInBytes,
		},
		Parameters:         params,
		VolumeCapabilities: capabilities,
	}

	respCreate, err := ct.controller.CreateVolume(ctx, reqCreate)
	if err != nil {
		t.Fatal(err)
	}
	volID := respCreate.Volume.VolumeId

	// Create Snapshot
	snapSpec := cnstypes.CnsSnapshotCreateSpec{
		VolumeId: cnstypes.CnsVolumeId{
			Id: volID,
		},
		Description: "Snap-1",
	}
	snapshotTask, err := ct.vcenter.CnsClient.CreateSnapshots(ctx, []cnstypes.CnsSnapshotCreateSpec{snapSpec})
	if err != nil {
		t.Fatalf("failed to create snapshot for volume: %s", volID)
	}
	snapTaskInfo, err := cns.GetTaskInfo(ctx, snapshotTask)
	if err != nil {
		t.Fatalf("failed to get snapshot taskinfo for volume: %s", volID)
	}
	taskResult, err := cns.GetTaskResult(ctx, snapTaskInfo)
	if err != nil {
		t.Fatalf("failed to get task result for snapshot operation on volume: %s", volID)
	}
	cnsSnapshot := taskResult.(*cnstypes.CnsSnapshotCreateResult)
	snapshotID := cnsSnapshot.Snapshot.SnapshotId.Id
	t.Logf("created cns snapshot: %s for volume: %s", snapshotID, volID)
	defer func() {
		deleteSnapSepc := cnstypes.CnsSnapshotDeleteSpec{
			VolumeId: cnstypes.CnsVolumeId{
				Id: volID,
			},
			SnapshotId: cnstypes.CnsSnapshotId{
				Id: snapshotID,
			},
		}
		_, err = ct.vcenter.CnsClient.DeleteSnapshots(ctx, []cnstypes.CnsSnapshotDeleteSpec{deleteSnapSepc})
		if err != nil {
			t.Errorf("failed to delete snapshot: %s for volume: %s", snapshotID, volID)
		}
	}()

	// Attempt to delete the volume
	reqDelete := &csi.DeleteVolumeRequest{
		VolumeId: volID,
	}
	_, err = ct.controller.DeleteVolume(ctx, reqDelete)
	if err != nil {
		delErr, ok := status.FromError(err)
		if !ok {
			t.Fatalf("unable to convert the error: %+v into a grpc status error type", err)
		}
		if delErr.Code() == codes.FailedPrecondition {
			t.Logf("received error as expected when attempting to delete volume with snapshot, error: %+v", err)
		} else {
			t.Fatalf("unexpected error code received, expected: %s received: %s",
				codes.FailedPrecondition.String(), delErr.Code().String())
		}
	} else {
		t.Fatal("expected error was not received when deleting volume with snapshot")
	}
}

func TestCreateBlockVolumeSnapshotWithIdempotency(t *testing.T) {
	ct := getControllerTest(t)

	// Create.
	params := make(map[string]string)
	if v := os.Getenv("VSPHERE_DATASTORE_URL"); v != "" {
		params[common.AttributeDatastoreURL] = v
	}
	capabilities := []*csi.VolumeCapability{
		{
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
	}

	reqCreate := &csi.CreateVolumeRequest{
		Name: testVolumeName + "-" + uuid.New().String(),
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 1 * common.GbInBytes,
		},
		Parameters:         params,
		VolumeCapabilities: capabilities,
	}

	respCreate, err := ct.controller.CreateVolume(ctx, reqCreate)
	if err != nil {
		t.Fatal(err)
	}
	volID := respCreate.Volume.VolumeId

	// Verify the volume has been created.
	queryFilter := cnstypes.CnsQueryFilter{
		VolumeIds: []cnstypes.CnsVolumeId{
			{
				Id: volID,
			},
		},
	}
	queryResult, err := ct.vcenter.CnsClient.QueryVolume(ctx, queryFilter)
	if err != nil {
		t.Fatal(err)
	}

	if len(queryResult.Volumes) != 1 && queryResult.Volumes[0].VolumeId.Id != volID {
		t.Fatalf("failed to find the newly created volume with ID: %s", volID)
	}

	// QueryAll.
	queryFilter = cnstypes.CnsQueryFilter{
		VolumeIds: []cnstypes.CnsVolumeId{
			{
				Id: volID,
			},
		},
	}
	querySelection := cnstypes.CnsQuerySelection{}
	queryResult, err = ct.vcenter.CnsClient.QueryAllVolume(ctx, queryFilter, querySelection)
	if err != nil {
		t.Fatal(err)
	}

	if len(queryResult.Volumes) != 1 && queryResult.Volumes[0].VolumeId.Id != volID {
		t.Fatalf("failed to find the newly created volume with ID: %s", volID)
	}

	defer func() {
		// Delete volume.
		reqDelete := &csi.DeleteVolumeRequest{
			VolumeId: volID,
		}
		_, err = ct.controller.DeleteVolume(ctx, reqDelete)
		if err != nil {
			t.Fatal(err)
		}

		// Verify the volume has been deleted.
		queryResult, err = ct.vcenter.CnsClient.QueryVolume(ctx, queryFilter)
		if err != nil {
			t.Fatal(err)
		}

		if len(queryResult.Volumes) != 0 {
			t.Fatalf("volume should not exist after deletion with ID: %s", volID)
		}
	}()

	snapshotName := "snapshot-" + uuid.New().String()

	// create a block volume snapshot
	reqCreateSnapshot := &csi.CreateSnapshotRequest{
		SourceVolumeId: volID,
		Name:           snapshotName,
	}

	respCreateSnapshot, err := ct.controller.CreateSnapshot(ctx, reqCreateSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	snapID := respCreateSnapshot.Snapshot.SnapshotId

	defer func() {
		// Delete the snapshot
		reqDeleteSnapshot := &csi.DeleteSnapshotRequest{
			SnapshotId: snapID,
		}

		_, err = ct.controller.DeleteSnapshot(ctx, reqDeleteSnapshot)
		if err != nil {
			t.Fatal(err)
		}
	}()

	// Check number of snapshot
	var numSnapshotsBefore, numSnapshotsAfter int
	var reqListSnapshots *csi.ListSnapshotsRequest
	var respListSnapshots *csi.ListSnapshotsResponse

	reqListSnapshots = &csi.ListSnapshotsRequest{
		SourceVolumeId: volID,
	}

	respListSnapshots, err = ct.controller.ListSnapshots(ctx, reqListSnapshots)
	if err != nil {
		t.Fatal(err)
	}

	numSnapshotsBefore = len(respListSnapshots.Entries)

	// create another block volume snapshot with the same snapshotName
	reqCreateSnapshot = &csi.CreateSnapshotRequest{
		SourceVolumeId: volID,
		Name:           snapshotName,
	}

	respCreateSnapshot, err = ct.controller.CreateSnapshot(ctx, reqCreateSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	snapID = respCreateSnapshot.Snapshot.SnapshotId

	defer func() {
		// Delete the snapshot
		reqDeleteSnapshot := &csi.DeleteSnapshotRequest{
			SnapshotId: snapID,
		}

		_, err = ct.controller.DeleteSnapshot(ctx, reqDeleteSnapshot)
		if err != nil {
			t.Fatal(err)
		}
	}()

	// Check number of snapshot
	reqListSnapshots = &csi.ListSnapshotsRequest{
		SourceVolumeId: volID,
	}

	respListSnapshots, err = ct.controller.ListSnapshots(ctx, reqListSnapshots)
	if err != nil {
		t.Fatal(err)
	}

	numSnapshotsAfter = len(respListSnapshots.Entries)

	// Expect the number of snapshots to be unchanged before and after the idempotent CreateSnapshot operation
	if numSnapshotsBefore != numSnapshotsAfter {
		t.Fatalf("Unexpected snapshot gets created after the idempotent CreateSnapshot operation, "+
			"numSnapshotsBefore = %v, numSnapshotsAfter = %v", numSnapshotsBefore, numSnapshotsAfter)
	} else {
		t.Logf("No unexpected snapshot gets created after the idempotent CreateSnapshot operation, "+
			"numSnapshotsBefore = %v, numSnapshotsAfter = %v", numSnapshotsBefore, numSnapshotsAfter)
	}
}

func TestCreateBlockVolumeSnapshot(t *testing.T) {
	ct := getControllerTest(t)

	// Create.
	params := make(map[string]string)
	if v := os.Getenv("VSPHERE_DATASTORE_URL"); v != "" {
		params[common.AttributeDatastoreURL] = v
	}
	capabilities := []*csi.VolumeCapability{
		{
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
	}

	reqCreate := &csi.CreateVolumeRequest{
		Name: testVolumeName + "-" + uuid.New().String(),
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 1 * common.GbInBytes,
		},
		Parameters:         params,
		VolumeCapabilities: capabilities,
	}

	respCreate, err := ct.controller.CreateVolume(ctx, reqCreate)
	if err != nil {
		t.Fatal(err)
	}
	volID := respCreate.Volume.VolumeId

	// Verify the volume has been created.
	queryFilter := cnstypes.CnsQueryFilter{
		VolumeIds: []cnstypes.CnsVolumeId{
			{
				Id: volID,
			},
		},
	}
	queryResult, err := ct.vcenter.CnsClient.QueryVolume(ctx, queryFilter)
	if err != nil {
		t.Fatal(err)
	}

	if len(queryResult.Volumes) != 1 && queryResult.Volumes[0].VolumeId.Id != volID {
		t.Fatalf("failed to find the newly created volume with ID: %s", volID)
	}

	// QueryAll.
	queryFilter = cnstypes.CnsQueryFilter{
		VolumeIds: []cnstypes.CnsVolumeId{
			{
				Id: volID,
			},
		},
	}
	querySelection := cnstypes.CnsQuerySelection{}
	queryResult, err = ct.vcenter.CnsClient.QueryAllVolume(ctx, queryFilter, querySelection)
	if err != nil {
		t.Fatal(err)
	}

	if len(queryResult.Volumes) != 1 && queryResult.Volumes[0].VolumeId.Id != volID {
		t.Fatalf("failed to find the newly created volume with ID: %s", volID)
	}

	defer func() {
		// Delete volume.
		reqDelete := &csi.DeleteVolumeRequest{
			VolumeId: volID,
		}
		_, err = ct.controller.DeleteVolume(ctx, reqDelete)
		if err != nil {
			t.Fatal(err)
		}

		// Verify the volume has been deleted.
		queryResult, err = ct.vcenter.CnsClient.QueryVolume(ctx, queryFilter)
		if err != nil {
			t.Fatal(err)
		}

		if len(queryResult.Volumes) != 0 {
			t.Fatalf("volume should not exist after deletion with ID: %s", volID)
		}
	}()

	// Create the configured max number of snapshots on the source volume.
	configured_max_snapshot_num := ct.controller.manager.CnsConfig.Snapshot.GlobalMaxSnapshotsPerBlockVolume
	for i := 0; i < configured_max_snapshot_num; i++ {
		reqCreateSnapshot := &csi.CreateSnapshotRequest{
			SourceVolumeId: volID,
			Name:           "snapshot-" + uuid.New().String(),
		}

		respCreateSnapshot, err := ct.controller.CreateSnapshot(ctx, reqCreateSnapshot)
		if err != nil {
			t.Fatal(err)
		}
		snapID := respCreateSnapshot.Snapshot.SnapshotId

		defer func() {
			// Delete the snapshot
			reqDeleteSnapshot := &csi.DeleteSnapshotRequest{
				SnapshotId: snapID,
			}

			_, err = ct.controller.DeleteSnapshot(ctx, reqDeleteSnapshot)
			if err != nil {
				t.Fatal(err)
			}
		}()
	}

	// error expected when create the fourth snapshot
	reqCreateSnapshot := &csi.CreateSnapshotRequest{
		SourceVolumeId: volID,
		Name:           "snapshot-" + uuid.New().String(),
	}
	expectedErr := fmt.Errorf("the number of snapshots on the source volume %s reaches "+
		"the configured maximum (%v)", volID, configured_max_snapshot_num)

	_, err = ct.controller.CreateSnapshot(ctx, reqCreateSnapshot)
	if err != nil {
		delErr, ok := status.FromError(err)
		if !ok {
			t.Fatalf("unable to convert the error: %+v into a grpc status error type.", err)
		}
		if delErr.Code() == codes.FailedPrecondition && delErr.Message() == expectedErr.Error() {
			t.Logf("received error as expected when attempting to create snapshot on volume "+
				"when existing number of snapshots reaches the configured maximum, error: %+v.", err)
		} else {
			t.Fatalf("unexpected error received, expected: %s received: %s.",
				expectedErr.Error(), delErr.Message())
		}
	} else {
		t.Fatal("expected error was not received create snapshot on volume " +
			"with configured maximum number of snapshots.")
	}
}

func TestCreateVolumeFromSnapshot(t *testing.T) {
	ct := getControllerTest(t)

	// Create.
	params := make(map[string]string)
	if v := os.Getenv("VSPHERE_DATASTORE_URL"); v != "" {
		params[common.AttributeDatastoreURL] = v
	}
	capabilities := []*csi.VolumeCapability{
		{
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
	}

	reqCreate := &csi.CreateVolumeRequest{
		Name: testVolumeName + "-" + uuid.New().String(),
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 1 * common.GbInBytes,
		},
		Parameters:         params,
		VolumeCapabilities: capabilities,
	}

	respCreate, err := ct.controller.CreateVolume(ctx, reqCreate)
	if err != nil {
		t.Fatal(err)
	}
	volID := respCreate.Volume.VolumeId

	// Verify the volume has been created.
	queryFilter := cnstypes.CnsQueryFilter{
		VolumeIds: []cnstypes.CnsVolumeId{
			{
				Id: volID,
			},
		},
	}
	queryResult, err := ct.vcenter.CnsClient.QueryVolume(ctx, queryFilter)
	if err != nil {
		t.Fatal(err)
	}

	if len(queryResult.Volumes) != 1 && queryResult.Volumes[0].VolumeId.Id != volID {
		t.Fatalf("failed to find the newly created volume with ID: %s", volID)
	}

	// QueryAll.
	queryFilter = cnstypes.CnsQueryFilter{
		VolumeIds: []cnstypes.CnsVolumeId{
			{
				Id: volID,
			},
		},
	}
	querySelection := cnstypes.CnsQuerySelection{}
	queryResult, err = ct.vcenter.CnsClient.QueryAllVolume(ctx, queryFilter, querySelection)
	if err != nil {
		t.Fatal(err)
	}

	if len(queryResult.Volumes) != 1 && queryResult.Volumes[0].VolumeId.Id != volID {
		t.Fatalf("failed to find the newly created volume with ID: %s", volID)
	}

	defer func() {
		// Delete.
		reqDelete := &csi.DeleteVolumeRequest{
			VolumeId: volID,
		}
		_, err = ct.controller.DeleteVolume(ctx, reqDelete)
		if err != nil {
			t.Fatal(err)
		}

		// Verify the volume has been deleted.
		queryResult, err = ct.vcenter.CnsClient.QueryVolume(ctx, queryFilter)
		if err != nil {
			t.Fatal(err)
		}

		if len(queryResult.Volumes) != 0 {
			t.Fatalf("Volume should not exist after deletion with ID: %s", volID)
		}
	}()

	// Snapshot a volume
	reqCreateSnapshot := &csi.CreateSnapshotRequest{
		SourceVolumeId: volID,
		Name:           "snapshot-" + uuid.New().String(),
	}

	respCreateSnapshot, err := ct.controller.CreateSnapshot(ctx, reqCreateSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	snapID := respCreateSnapshot.Snapshot.SnapshotId

	defer func() {
		// Delete the snapshot
		reqDeleteSnapshot := &csi.DeleteSnapshotRequest{
			SnapshotId: snapID,
		}

		_, err = ct.controller.DeleteSnapshot(ctx, reqDeleteSnapshot)
		if err != nil {
			t.Fatal(err)
		}
	}()

	// Create a new volume from the snapshot with expected request
	reqCreateFromSnapshot := &csi.CreateVolumeRequest{
		Name: testVolumeName + "-" + uuid.New().String(),
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 1 * common.GbInBytes,
		},
		Parameters:         params,
		VolumeCapabilities: capabilities,
		VolumeContentSource: &csi.VolumeContentSource{
			Type: &csi.VolumeContentSource_Snapshot{
				Snapshot: &csi.VolumeContentSource_SnapshotSource{
					SnapshotId: snapID,
				},
			},
		},
	}

	respCreateFromSnapshot, err := ct.controller.CreateVolume(ctx, reqCreateFromSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	restoredVolID := respCreateFromSnapshot.Volume.VolumeId

	// Verify the volume has been created.
	queryFilter = cnstypes.CnsQueryFilter{
		VolumeIds: []cnstypes.CnsVolumeId{
			{
				Id: restoredVolID,
			},
		},
	}
	queryResult, err = ct.vcenter.CnsClient.QueryVolume(ctx, queryFilter)
	if err != nil {
		t.Fatal(err)
	}

	if len(queryResult.Volumes) != 1 && queryResult.Volumes[0].VolumeId.Id != restoredVolID {
		t.Fatalf("failed to find the newly created volume from snapshot with ID: %s", restoredVolID)
	}

	defer func() {
		// Delete the restored volume
		reqDelete := &csi.DeleteVolumeRequest{
			VolumeId: restoredVolID,
		}
		_, err = ct.controller.DeleteVolume(ctx, reqDelete)
		if err != nil {
			t.Fatal(err)
		}

		// Verify the volume has been deleted.
		queryResult, err = ct.vcenter.CnsClient.QueryVolume(ctx, queryFilter)
		if err != nil {
			t.Fatal(err)
		}

		if len(queryResult.Volumes) != 0 {
			t.Fatalf("Volume should not exist after deletion with ID: %s", restoredVolID)
		}
	}()

	// Create a new volume from the snapshot with unexpected request
	reqCreateFromSnapshot = &csi.CreateVolumeRequest{
		Name: testVolumeName + "-" + uuid.New().String(),
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 2 * common.GbInBytes,
		},
		Parameters:         params,
		VolumeCapabilities: capabilities,
		VolumeContentSource: &csi.VolumeContentSource{
			Type: &csi.VolumeContentSource_Snapshot{
				Snapshot: &csi.VolumeContentSource_SnapshotSource{
					SnapshotId: snapID,
				},
			},
		},
	}

	_, err = ct.controller.CreateVolume(ctx, reqCreateFromSnapshot)
	if err != nil {
		statusErr, ok := status.FromError(err)
		if !ok {
			t.Fatalf("unable to convert the error: %+v into a grpc status error type", err)
		}
		if statusErr.Code() == codes.InvalidArgument {
			t.Logf("received error as expected when attempting to create volume from snapshot, error: %+v", err)
		} else {
			t.Fatalf("unexpected error code received, expected: %s received: %s",
				codes.InvalidArgument.String(), statusErr.Code().String())
		}
	} else {
		t.Fatal("expected error was not received when creating volume from snapshot")
	}
}

func TestListSnapshotsOnSpecificVolumeAndSnapshot(t *testing.T) {
	ct := getControllerTest(t)

	// Create.
	params := make(map[string]string)
	if v := os.Getenv("VSPHERE_DATASTORE_URL"); v != "" {
		params[common.AttributeDatastoreURL] = v
	}
	capabilities := []*csi.VolumeCapability{
		{
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
	}

	reqCreate := &csi.CreateVolumeRequest{
		Name: testVolumeName + "-" + uuid.New().String(),
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 1 * common.GbInBytes,
		},
		Parameters:         params,
		VolumeCapabilities: capabilities,
	}

	respCreate, err := ct.controller.CreateVolume(ctx, reqCreate)
	if err != nil {
		t.Fatal(err)
	}
	volID := respCreate.Volume.VolumeId

	// Verify the volume has been created.
	queryFilter := cnstypes.CnsQueryFilter{
		VolumeIds: []cnstypes.CnsVolumeId{
			{
				Id: volID,
			},
		},
	}
	queryResult, err := ct.vcenter.CnsClient.QueryVolume(ctx, queryFilter)
	if err != nil {
		t.Fatal(err)
	}

	if len(queryResult.Volumes) != 1 && queryResult.Volumes[0].VolumeId.Id != volID {
		t.Fatalf("failed to find the newly created volume with ID: %s", volID)
	}

	// Snapshot a volume
	reqCreateSnapshot := &csi.CreateSnapshotRequest{
		SourceVolumeId: volID,
		Name:           "snapshot-" + uuid.New().String(),
	}

	respCreateSnapshot, err := ct.controller.CreateSnapshot(ctx, reqCreateSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	snapID := respCreateSnapshot.Snapshot.SnapshotId

	// Invoke ListSnapshot
	listSnapshotRequest := &csi.ListSnapshotsRequest{
		MaxEntries:     00,
		StartingToken:  "",
		SourceVolumeId: volID,
		SnapshotId:     snapID,
	}

	listSnapshotsRespone, err := ct.controller.ListSnapshots(ctx, listSnapshotRequest)
	if err != nil {
		t.Logf("ListSnapshot invocation failed with err: %+v", err)
		t.Fatal(err)
	}

	if len(listSnapshotsRespone.Entries) == 0 {
		t.Fatalf("ListSnapshot did not return and results for volume-id: %s and snapshot-id: %s", volID, snapID)
	}

	snapshotReturned := listSnapshotsRespone.Entries[0]
	if snapshotReturned.Snapshot.SnapshotId != snapID || snapshotReturned.Snapshot.SourceVolumeId != volID {
		t.Fatalf("failed to returned the specific snapshot for ListSnapshot, received: %+v", snapshotReturned)
	}

	// log the specific snapshot information
	t.Log("==============================================================")
	t.Logf("SourceVolumeId: %s", snapshotReturned.Snapshot.SourceVolumeId)
	t.Logf("SnapshotId: %s", snapshotReturned.Snapshot.SnapshotId)
	t.Logf("CreationTime: %s", snapshotReturned.Snapshot.CreationTime)
	t.Logf("Size: %d", snapshotReturned.Snapshot.SizeBytes)
	t.Logf("ReadyToUse: %t", snapshotReturned.Snapshot.ReadyToUse)
	t.Log("==============================================================")

	// Delete the snapshot
	reqDeleteSnapshot := &csi.DeleteSnapshotRequest{
		SnapshotId: snapID,
	}

	_, err = ct.controller.DeleteSnapshot(ctx, reqDeleteSnapshot)
	if err != nil {
		t.Fatal(err)
	}

	// Delete.
	reqDelete := &csi.DeleteVolumeRequest{
		VolumeId: volID,
	}
	_, err = ct.controller.DeleteVolume(ctx, reqDelete)
	if err != nil {
		t.Fatal(err)
	}

	// Verify the volume has been deleted.
	queryResult, err = ct.vcenter.CnsClient.QueryVolume(ctx, queryFilter)
	if err != nil {
		t.Fatal(err)
	}

	if len(queryResult.Volumes) != 0 {
		t.Fatalf("Volume should not exist after deletion with ID: %s", volID)
	}
}

func TestListSnapshotsOnSpecificVolume(t *testing.T) {
	ct := getControllerTest(t)
	numOfSnapshots := ct.config.Snapshot.GlobalMaxSnapshotsPerBlockVolume
	// Create.
	params := make(map[string]string)
	if v := os.Getenv("VSPHERE_DATASTORE_URL"); v != "" {
		params[common.AttributeDatastoreURL] = v
	}
	capabilities := []*csi.VolumeCapability{
		{
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
	}

	reqCreate := &csi.CreateVolumeRequest{
		Name: testVolumeName + "-" + uuid.New().String(),
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 1 * common.GbInBytes,
		},
		Parameters:         params,
		VolumeCapabilities: capabilities,
	}

	respCreate, err := ct.controller.CreateVolume(ctx, reqCreate)
	if err != nil {
		t.Fatal(err)
	}
	volID := respCreate.Volume.VolumeId

	// Verify the volume has been created.
	queryFilter := cnstypes.CnsQueryFilter{
		VolumeIds: []cnstypes.CnsVolumeId{
			{
				Id: volID,
			},
		},
	}
	queryResult, err := ct.vcenter.CnsClient.QueryVolume(ctx, queryFilter)
	if err != nil {
		t.Fatal(err)
	}

	if len(queryResult.Volumes) != 1 && queryResult.Volumes[0].VolumeId.Id != volID {
		t.Fatalf("failed to find the newly created volume with ID: %s", volID)
	}

	// Map to track all the snapshots created.
	snapshots := make(map[string]string)
	var deleteSnapshotList []string

	for i := 0; i < numOfSnapshots; i++ {
		// Snapshot a volume
		reqCreateSnapshot := &csi.CreateSnapshotRequest{
			SourceVolumeId: volID,
			Name:           "snapshot-" + uuid.New().String(),
		}

		respCreateSnapshot, err := ct.controller.CreateSnapshot(ctx, reqCreateSnapshot)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("Created snapshot-%d snaphot-id: %s", i, respCreateSnapshot.Snapshot.SnapshotId)
		snapshots[respCreateSnapshot.Snapshot.SnapshotId] = ""
		deleteSnapshotList = append(deleteSnapshotList, respCreateSnapshot.Snapshot.SnapshotId)
	}

	// Invoke ListSnapshot
	listSnapshotRequest := &csi.ListSnapshotsRequest{
		MaxEntries:     0,
		StartingToken:  "",
		SourceVolumeId: volID,
	}

	listSnapshotsResponse, err := ct.controller.ListSnapshots(ctx, listSnapshotRequest)
	if err != nil {
		t.Logf("ListSnapshot invocation failed with err: %+v", err)
		t.Fatal(err)
	}

	if len(listSnapshotsResponse.Entries) == 0 {
		t.Fatalf("ListSnapshot did not return and results for volume-id: %s", volID)
	}

	// Iterate through response removing entries from the original map.
	for i, entry := range listSnapshotsResponse.Entries {
		snapshot := entry.Snapshot
		// log the specific snapshot information
		t.Logf("=====================Snapshot-%d===============================", i)
		t.Logf("SourceVolumeId: %s", snapshot.SourceVolumeId)
		t.Logf("SnapshotId: %s", snapshot.SnapshotId)
		t.Logf("CreationTime: %s", snapshot.CreationTime)
		t.Logf("Size: %d", snapshot.SizeBytes)
		t.Logf("ReadyToUse: %t", snapshot.ReadyToUse)
		t.Log("================================================================")
		delete(snapshots, snapshot.SnapshotId)
	}
	// Expect all snapshots to be deleted, the remaining snapshots were not returned in response.
	if len(snapshots) != 0 {
		t.Fatalf("Not all snapshots were returned, missing snapshots: %+v", snapshots)
	}
	// delete snapshots as part of cleanup.
	for i := len(deleteSnapshotList) - 1; i >= 0; i-- {
		// Delete the snapshot
		reqDeleteSnapshot := &csi.DeleteSnapshotRequest{
			SnapshotId: deleteSnapshotList[i],
		}
		_, err = ct.controller.DeleteSnapshot(ctx, reqDeleteSnapshot)
		if err != nil {
			t.Fatal(err)
		}
	}
	// Delete the volume.
	reqDelete := &csi.DeleteVolumeRequest{
		VolumeId: volID,
	}
	_, err = ct.controller.DeleteVolume(ctx, reqDelete)
	if err != nil {
		t.Fatal(err)
	}
}

func TestListSnapshots(t *testing.T) {
	ct := getControllerTest(t)
	numOfSnapshots := ct.config.Snapshot.GlobalMaxSnapshotsPerBlockVolume
	// Create.
	params := make(map[string]string)
	if v := os.Getenv("VSPHERE_DATASTORE_URL"); v != "" {
		params[common.AttributeDatastoreURL] = v
	}
	capabilities := []*csi.VolumeCapability{
		{
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
	}

	reqCreate := &csi.CreateVolumeRequest{
		Name: testVolumeName + "-" + uuid.New().String(),
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 1 * common.GbInBytes,
		},
		Parameters:         params,
		VolumeCapabilities: capabilities,
	}

	respCreate, err := ct.controller.CreateVolume(ctx, reqCreate)
	if err != nil {
		t.Fatal(err)
	}
	volID := respCreate.Volume.VolumeId

	// Verify the volume has been created.
	queryFilter := cnstypes.CnsQueryFilter{
		VolumeIds: []cnstypes.CnsVolumeId{
			{
				Id: volID,
			},
		},
	}
	queryResult, err := ct.vcenter.CnsClient.QueryVolume(ctx, queryFilter)
	if err != nil {
		t.Fatal(err)
	}

	if len(queryResult.Volumes) != 1 && queryResult.Volumes[0].VolumeId.Id != volID {
		t.Fatalf("failed to find the newly created volume with ID: %s", volID)
	}

	// Map to track all the snapshots created.
	snapshots := make(map[string]string)
	var deleteSnapshotList []string

	for i := 0; i < numOfSnapshots; i++ {
		// Snapshot a volume
		reqCreateSnapshot := &csi.CreateSnapshotRequest{
			SourceVolumeId: volID,
			Name:           "snapshot-" + uuid.New().String(),
		}

		respCreateSnapshot, err := ct.controller.CreateSnapshot(ctx, reqCreateSnapshot)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("Created snapshot-%d snaphot-id: %s", i, respCreateSnapshot.Snapshot.SnapshotId)
		snapshots[respCreateSnapshot.Snapshot.SnapshotId] = ""
		deleteSnapshotList = append(deleteSnapshotList, respCreateSnapshot.Snapshot.SnapshotId)
	}

	// Invoke ListSnapshot without specifying vol or snap-id.
	listSnapshotRequest := &csi.ListSnapshotsRequest{
		MaxEntries:    0,
		StartingToken: "",
	}

	listSnapshotsResponse, err := ct.controller.ListSnapshots(ctx, listSnapshotRequest)
	if err != nil {
		t.Logf("ListSnapshot invocation failed with err: %+v", err)
		t.Fatal(err)
	}

	if len(listSnapshotsResponse.Entries) == 0 {
		t.Fatalf("ListSnapshot did not return any results")
	}

	// Iterate through response removing entries from the original map.
	for i, entry := range listSnapshotsResponse.Entries {
		snapshot := entry.Snapshot
		// log the specific snapshot information
		t.Logf("=====================Snapshot-%d===============================", i)
		t.Logf("SourceVolumeId: %s", snapshot.SourceVolumeId)
		t.Logf("SnapshotId: %s", snapshot.SnapshotId)
		t.Logf("CreationTime: %s", snapshot.CreationTime)
		t.Logf("Size: %d", snapshot.SizeBytes)
		t.Logf("ReadyToUse: %t", snapshot.ReadyToUse)
		t.Log("================================================================")
		delete(snapshots, snapshot.SnapshotId)
	}
	// Expect returned snapshots to be deleted from map, the remaining snapshots were not returned in response.
	if len(snapshots) != 0 {
		t.Fatalf("Not all snapshots were returned, missing snapshots: %+v", snapshots)
	}
	// delete snapshots as part of cleanup.
	for i := len(deleteSnapshotList) - 1; i >= 0; i-- {
		// Delete the snapshot
		reqDeleteSnapshot := &csi.DeleteSnapshotRequest{
			SnapshotId: deleteSnapshotList[i],
		}
		_, err = ct.controller.DeleteSnapshot(ctx, reqDeleteSnapshot)
		if err != nil {
			t.Fatal(err)
		}
	}
	// Delete the volume.
	reqDelete := &csi.DeleteVolumeRequest{
		VolumeId: volID,
	}
	_, err = ct.controller.DeleteVolume(ctx, reqDelete)
	if err != nil {
		t.Fatal(err)
	}
}

func TestListSnapshotsWithToken(t *testing.T) {
	ct := getControllerTest(t)
	numOfSnapshots := ct.config.Snapshot.GlobalMaxSnapshotsPerBlockVolume
	// Create.
	params := make(map[string]string)
	if v := os.Getenv("VSPHERE_DATASTORE_URL"); v != "" {
		params[common.AttributeDatastoreURL] = v
	}
	capabilities := []*csi.VolumeCapability{
		{
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
	}

	reqCreate := &csi.CreateVolumeRequest{
		Name: testVolumeName + "-" + uuid.New().String(),
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 1 * common.GbInBytes,
		},
		Parameters:         params,
		VolumeCapabilities: capabilities,
	}

	respCreate, err := ct.controller.CreateVolume(ctx, reqCreate)
	if err != nil {
		t.Fatal(err)
	}
	volID := respCreate.Volume.VolumeId

	// Verify the volume has been created.
	queryFilter := cnstypes.CnsQueryFilter{
		VolumeIds: []cnstypes.CnsVolumeId{
			{
				Id: volID,
			},
		},
	}
	queryResult, err := ct.vcenter.CnsClient.QueryVolume(ctx, queryFilter)
	if err != nil {
		t.Fatal(err)
	}

	if len(queryResult.Volumes) != 1 && queryResult.Volumes[0].VolumeId.Id != volID {
		t.Fatalf("failed to find the newly created volume with ID: %s", volID)
	}

	// Map to track all the snapshots created.
	snapshots := make(map[string]string)
	var deleteSnapshotList []string

	for i := 0; i < numOfSnapshots; i++ {
		// Snapshot a volume
		reqCreateSnapshot := &csi.CreateSnapshotRequest{
			SourceVolumeId: volID,
			Name:           "snapshot-" + uuid.New().String(),
		}

		respCreateSnapshot, err := ct.controller.CreateSnapshot(ctx, reqCreateSnapshot)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("Created snapshot-%d snaphot-id: %s", i, respCreateSnapshot.Snapshot.SnapshotId)
		snapshots[respCreateSnapshot.Snapshot.SnapshotId] = ""
		deleteSnapshotList = append(deleteSnapshotList, respCreateSnapshot.Snapshot.SnapshotId)
	}

	var listSnapshotsResponseEntries []*csi.ListSnapshotsResponse_Entry
	tok := ""
	for {
		// Specify max entries as 1 to trigger paginated results.
		listSnapshotRequest := &csi.ListSnapshotsRequest{
			MaxEntries:    1,
			StartingToken: tok,
		}

		listSnapshotsResponse, err := ct.controller.ListSnapshots(ctx, listSnapshotRequest)
		if err != nil {
			t.Logf("ListSnapshot invocation failed with err: %+v", err)
			t.Fatal(err)
		}
		listSnapshotsResponseEntries = append(listSnapshotsResponseEntries, listSnapshotsResponse.Entries...)
		// Use the next token returned.
		tok = listSnapshotsResponse.NextToken
		if len(tok) == 0 {
			break
		}
	}

	if len(listSnapshotsResponseEntries) == 0 {
		t.Fatalf("ListSnapshot did not return any results")
	}

	// Iterate through response removing entries from the original map.
	for i, entry := range listSnapshotsResponseEntries {
		snapshot := entry.Snapshot
		// log the specific snapshot information
		t.Logf("=====================Snapshot-%d===============================", i)
		t.Logf("SourceVolumeId: %s", snapshot.SourceVolumeId)
		t.Logf("SnapshotId: %s", snapshot.SnapshotId)
		t.Logf("CreationTime: %s", snapshot.CreationTime)
		t.Logf("Size: %d", snapshot.SizeBytes)
		t.Logf("ReadyToUse: %t", snapshot.ReadyToUse)
		t.Log("================================================================")
		delete(snapshots, snapshot.SnapshotId)
	}
	// Expect returned snapshots to be deleted from map, the remaining snapshots were not returned in response.
	if len(snapshots) != 0 {
		t.Fatalf("Not all snapshots were returned, missing snapshots: %+v", snapshots)
	}
	// delete snapshots as part of cleanup.
	for i := len(deleteSnapshotList) - 1; i >= 0; i-- {
		// Delete the snapshot
		reqDeleteSnapshot := &csi.DeleteSnapshotRequest{
			SnapshotId: deleteSnapshotList[i],
		}
		_, err = ct.controller.DeleteSnapshot(ctx, reqDeleteSnapshot)
		if err != nil {
			t.Fatal(err)
		}
	}
	// Delete the volume.
	reqDelete := &csi.DeleteVolumeRequest{
		VolumeId: volID,
	}
	_, err = ct.controller.DeleteVolume(ctx, reqDelete)
	if err != nil {
		t.Fatal(err)
	}
}

func TestExpandVolumeWithSnapshots(t *testing.T) {
	ct := getControllerTest(t)

	// Create.
	params := make(map[string]string)
	if v := os.Getenv("VSPHERE_DATASTORE_URL"); v != "" {
		params[common.AttributeDatastoreURL] = v
	}
	capabilities := []*csi.VolumeCapability{
		{
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
	}

	reqCreate := &csi.CreateVolumeRequest{
		Name: testVolumeName + "-" + uuid.New().String(),
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 1 * common.GbInBytes,
		},
		Parameters:         params,
		VolumeCapabilities: capabilities,
	}

	respCreate, err := ct.controller.CreateVolume(ctx, reqCreate)
	if err != nil {
		t.Fatal(err)
	}
	volID := respCreate.Volume.VolumeId

	// Verify the volume has been created.
	queryFilter := cnstypes.CnsQueryFilter{
		VolumeIds: []cnstypes.CnsVolumeId{
			{
				Id: volID,
			},
		},
	}
	queryResult, err := ct.vcenter.CnsClient.QueryVolume(ctx, queryFilter)
	if err != nil {
		t.Fatal(err)
	}

	if len(queryResult.Volumes) != 1 && queryResult.Volumes[0].VolumeId.Id != volID {
		t.Fatalf("failed to find the newly created volume with ID: %s", volID)
	}

	// Snapshot a volume
	reqCreateSnapshot := &csi.CreateSnapshotRequest{
		SourceVolumeId: volID,
		Name:           "snapshot-" + uuid.New().String(),
	}

	respCreateSnapshot, err := ct.controller.CreateSnapshot(ctx, reqCreateSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	snapID := respCreateSnapshot.Snapshot.SnapshotId

	// Attempt to expand the volume
	reqExpand := &csi.ControllerExpandVolumeRequest{
		VolumeId: volID,
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 2 * common.GbInBytes,
		},
		VolumeCapability: capabilities[0],
	}
	_, err = ct.controller.ControllerExpandVolume(ctx, reqExpand)
	if err != nil {
		expandErr, ok := status.FromError(err)
		if !ok {
			t.Fatalf("unable to convert the error: %+v into a grpc status error type", err)
		}
		if expandErr.Code() == codes.FailedPrecondition {
			t.Logf("received error as expected when attempting to expand volume with snapshot, error: %+v", err)
		} else {
			t.Fatalf("unexpected error code received, expected: %s received: %s",
				codes.FailedPrecondition.String(), expandErr.Code().String())
		}
	} else {
		t.Fatal("expected error was not received when expanding volume with snapshot")
	}
	// Delete the snapshot
	reqDeleteSnapshot := &csi.DeleteSnapshotRequest{
		SnapshotId: snapID,
	}
	_, err = ct.controller.DeleteSnapshot(ctx, reqDeleteSnapshot)
	if err != nil {
		t.Fatal(err)
	}

	// Delete the volume
	reqDelete := &csi.DeleteVolumeRequest{
		VolumeId: volID,
	}
	_, err = ct.controller.DeleteVolume(ctx, reqDelete)
	if err != nil {
		t.Fatal(err)
	}

	// Verify the volume has been deleted.
	queryResult, err = ct.vcenter.CnsClient.QueryVolume(ctx, queryFilter)
	if err != nil {
		t.Fatal(err)
	}

	if len(queryResult.Volumes) != 0 {
		t.Fatalf("Volume should not exist after deletion with ID: %s", volID)
	}
}

func TestDeleteBlockVolumeSnapshotWithManagedObjectNotFound(t *testing.T) {
	ct := getControllerTest(t)

	// Create.
	params := make(map[string]string)
	if v := os.Getenv("VSPHERE_DATASTORE_URL"); v != "" {
		params[common.AttributeDatastoreURL] = v
	}
	capabilities := []*csi.VolumeCapability{
		{
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
	}

	reqCreate := &csi.CreateVolumeRequest{
		Name: testVolumeName + "-" + uuid.New().String(),
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 1 * common.GbInBytes,
		},
		Parameters:         params,
		VolumeCapabilities: capabilities,
	}

	respCreate, err := ct.controller.CreateVolume(ctx, reqCreate)
	if err != nil {
		t.Fatal(err)
	}
	volID := respCreate.Volume.VolumeId

	// Verify the volume has been created.
	queryFilter := cnstypes.CnsQueryFilter{
		VolumeIds: []cnstypes.CnsVolumeId{
			{
				Id: volID,
			},
		},
	}
	queryResult, err := ct.vcenter.CnsClient.QueryVolume(ctx, queryFilter)
	if err != nil {
		t.Fatal(err)
	}

	if len(queryResult.Volumes) != 1 && queryResult.Volumes[0].VolumeId.Id != volID {
		t.Fatalf("failed to find the newly created volume with ID: %s", volID)
	}

	// QueryAll.
	queryFilter = cnstypes.CnsQueryFilter{
		VolumeIds: []cnstypes.CnsVolumeId{
			{
				Id: volID,
			},
		},
	}
	querySelection := cnstypes.CnsQuerySelection{}
	queryResult, err = ct.vcenter.CnsClient.QueryAllVolume(ctx, queryFilter, querySelection)
	if err != nil {
		t.Fatal(err)
	}

	if len(queryResult.Volumes) != 1 && queryResult.Volumes[0].VolumeId.Id != volID {
		t.Fatalf("failed to find the newly created volume with ID: %s", volID)
	}

	defer func() {
		// Delete volume.
		reqDelete := &csi.DeleteVolumeRequest{
			VolumeId: volID,
		}
		_, err = ct.controller.DeleteVolume(ctx, reqDelete)
		if err != nil {
			t.Fatal(err)
		}

		// Verify the volume has been deleted.
		queryResult, err = ct.vcenter.CnsClient.QueryVolume(ctx, queryFilter)
		if err != nil {
			t.Fatal(err)
		}

		if len(queryResult.Volumes) != 0 {
			t.Fatalf("volume should not exist after deletion with ID: %s", volID)
		}
	}()

	// set up a scenario for testing DeleteSnapshot, where the DeleteSnapshot task is removed in vSphere
	snapshotID := uuid.New().String()
	taskID := "non-existent-task-id" // use a task id that must be non-existent
	instanceName := "deletesnapshot-" + volID + "-" + snapshotID
	operationInstance := cnsvolumeoperationrequest.CreateVolumeOperationRequestDetails(
		instanceName, "", "", 0, nil, metav1.Now(),
		taskID, "", "", cnsvolumeoperationrequest.TaskInvocationStatusInProgress, "")
	_ = ct.operationStore.StoreRequestDetails(ctx, operationInstance)

	// logger.SetLoggerLevel(logger.DevelopmentLogLevel) // enable debug level log

	// Delete the snapshot
	reqDeleteSnapshot := &csi.DeleteSnapshotRequest{
		SnapshotId: volID + common.VSphereCSISnapshotIdDelimiter + snapshotID,
	}

	_, err = ct.controller.DeleteSnapshot(ctx, reqDeleteSnapshot)
	if err != nil {
		t.Fatalf("Unexpected error is thrown in DeleteSnapshot with error: %v", err)
	}
}

func TestCreateSnapshotWithManagedObjectNotFound(t *testing.T) {
	ct := getControllerTest(t)

	// Create.
	params := make(map[string]string)
	if v := os.Getenv("VSPHERE_DATASTORE_URL"); v != "" {
		params[common.AttributeDatastoreURL] = v
	}
	capabilities := []*csi.VolumeCapability{
		{
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
	}

	reqCreate := &csi.CreateVolumeRequest{
		Name: testVolumeName + "-" + uuid.New().String(),
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 1 * common.GbInBytes,
		},
		Parameters:         params,
		VolumeCapabilities: capabilities,
	}

	respCreate, err := ct.controller.CreateVolume(ctx, reqCreate)
	if err != nil {
		t.Fatal(err)
	}
	volID := respCreate.Volume.VolumeId

	// Verify the volume has been created.
	queryFilter := cnstypes.CnsQueryFilter{
		VolumeIds: []cnstypes.CnsVolumeId{
			{
				Id: volID,
			},
		},
	}
	queryResult, err := ct.vcenter.CnsClient.QueryVolume(ctx, queryFilter)
	if err != nil {
		t.Fatal(err)
	}

	if len(queryResult.Volumes) != 1 && queryResult.Volumes[0].VolumeId.Id != volID {
		t.Fatalf("failed to find the newly created volume with ID: %s", volID)
	}

	// QueryAll.
	queryFilter = cnstypes.CnsQueryFilter{
		VolumeIds: []cnstypes.CnsVolumeId{
			{
				Id: volID,
			},
		},
	}
	querySelection := cnstypes.CnsQuerySelection{}
	queryResult, err = ct.vcenter.CnsClient.QueryAllVolume(ctx, queryFilter, querySelection)
	if err != nil {
		t.Fatal(err)
	}

	if len(queryResult.Volumes) != 1 && queryResult.Volumes[0].VolumeId.Id != volID {
		t.Fatalf("failed to find the newly created volume with ID: %s", volID)
	}

	defer func() {
		// Delete volume.
		reqDelete := &csi.DeleteVolumeRequest{
			VolumeId: volID,
		}
		_, err = ct.controller.DeleteVolume(ctx, reqDelete)
		if err != nil {
			t.Fatal(err)
		}

		// Verify the volume has been deleted.
		queryResult, err = ct.vcenter.CnsClient.QueryVolume(ctx, queryFilter)
		if err != nil {
			t.Fatal(err)
		}

		if len(queryResult.Volumes) != 0 {
			t.Fatalf("volume should not exist after deletion with ID: %s", volID)
		}
	}()

	snapshotName := "snapshot-" + uuid.New().String()
	reqCreateSnapshot := &csi.CreateSnapshotRequest{
		SourceVolumeId: volID,
		Name:           snapshotName,
	}

	respCreateSnapshot, err := ct.controller.CreateSnapshot(ctx, reqCreateSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	snapID := respCreateSnapshot.Snapshot.SnapshotId

	defer func() {
		// Delete the snapshot
		reqDeleteSnapshot := &csi.DeleteSnapshotRequest{
			SnapshotId: snapID,
		}

		_, err = ct.controller.DeleteSnapshot(ctx, reqDeleteSnapshot)
		if err != nil {
			t.Fatal(err)
		}
	}()

	taskID := "non-existent-task-id" // use a task id that must be non-existent
	instanceName := snapshotName + "-" + volID
	operationInstance := cnsvolumeoperationrequest.CreateVolumeOperationRequestDetails(
		instanceName, volID, "", 0, nil, metav1.Now(),
		taskID, "", "", cnsvolumeoperationrequest.TaskInvocationStatusInProgress, "")
	_ = ct.operationStore.StoreRequestDetails(ctx, operationInstance)
	// Attempt to create snapshot again, but the task-id is non-existent.
	// Since the snapshot already exists, no error is expected.
	_, err = ct.controller.CreateSnapshot(ctx, reqCreateSnapshot)
	if err != nil {
		t.Fatal(err)
	}
}

func TestCreateSnapshotWithCnsSnapshotCreatedFault(t *testing.T) {
	ct := getControllerTest(t)

	// Create volume.
	params := make(map[string]string)
	if v := os.Getenv("VSPHERE_DATASTORE_URL"); v != "" {
		params[common.AttributeDatastoreURL] = v
	}
	capabilities := []*csi.VolumeCapability{
		{
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
	}

	reqCreate := &csi.CreateVolumeRequest{
		Name: testVolumeName + "-" + uuid.New().String(),
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 1 * common.GbInBytes,
		},
		Parameters:         params,
		VolumeCapabilities: capabilities,
	}

	respCreate, err := ct.controller.CreateVolume(ctx, reqCreate)
	if err != nil {
		t.Fatal(err)
	}
	volID := respCreate.Volume.VolumeId

	// Verify the volume has been created.
	queryFilter := cnstypes.CnsQueryFilter{
		VolumeIds: []cnstypes.CnsVolumeId{
			{
				Id: volID,
			},
		},
	}
	queryResult, err := ct.vcenter.CnsClient.QueryVolume(ctx, queryFilter)
	if err != nil {
		t.Fatal(err)
	}

	if len(queryResult.Volumes) != 1 && queryResult.Volumes[0].VolumeId.Id != volID {
		t.Fatalf("failed to find the newly created volume with ID: %s", volID)
	}

	// QueryAll.
	queryFilter = cnstypes.CnsQueryFilter{
		VolumeIds: []cnstypes.CnsVolumeId{
			{
				Id: volID,
			},
		},
	}
	querySelection := cnstypes.CnsQuerySelection{}
	queryResult, err = ct.vcenter.CnsClient.QueryAllVolume(ctx, queryFilter, querySelection)
	if err != nil {
		t.Fatal(err)
	}

	if len(queryResult.Volumes) != 1 && queryResult.Volumes[0].VolumeId.Id != volID {
		t.Fatalf("failed to find the newly created volume with ID: %s", volID)
	}

	defer func() {
		// Delete volume.
		reqDelete := &csi.DeleteVolumeRequest{
			VolumeId: volID,
		}
		_, err = ct.controller.DeleteVolume(ctx, reqDelete)
		if err != nil {
			t.Fatal(err)
		}

		// Verify the volume has been deleted.
		queryResult, err = ct.vcenter.CnsClient.QueryVolume(ctx, queryFilter)
		if err != nil {
			t.Fatal(err)
		}

		if len(queryResult.Volumes) != 0 {
			t.Fatalf("volume should not exist after deletion with ID: %s", volID)
		}
	}()

	// Create snapshot
	snapshotName := "snapshot-" + uuid.New().String()
	reqCreateSnapshot := &csi.CreateSnapshotRequest{
		SourceVolumeId: volID,
		Name:           snapshotName,
	}

	respCreateSnapshot, err := ct.controller.CreateSnapshot(ctx, reqCreateSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	snapID := respCreateSnapshot.Snapshot.SnapshotId

	defer func() {
		// Delete the snapshot
		reqDeleteSnapshot := &csi.DeleteSnapshotRequest{
			SnapshotId: snapID,
		}

		_, err = ct.controller.DeleteSnapshot(ctx, reqDeleteSnapshot)
		if err != nil {
			t.Fatal(err)
		}
	}()

	// Simulate condition where CNS returns CnsSnapshotCreatedFault and task details
	// are stored with PartiallyFailed task status. This fault is returned by CNS in
	// case snapshot creation is successful in CNS, but post-processing fails.
	taskID := "non-existent-task-id" // use a task id that must be non-existent
	instanceName := snapshotName + "-" + volID
	operationInstance := cnsvolumeoperationrequest.CreateVolumeOperationRequestDetails(
		instanceName, volID, snapID, 0, nil, metav1.Now(),
		taskID, "", "", cnsvolumeoperationrequest.TaskInvocationStatusPartiallyFailed, "")
	_ = ct.operationStore.StoreRequestDetails(ctx, operationInstance)

	// Attempt to create snapshot again.
	// Since the snapshot operation details are stored with PartiallyFailed task status,
	// we should get the relevant error.
	expectedErr := fmt.Errorf("Snapshot with name \"%s\" and id \"%s\" on volume \"%s\" is created on CNS, "+
		"but post-processing failed.", instanceName, snapID, volID)
	_, err = ct.controller.CreateSnapshot(ctx, reqCreateSnapshot)
	if err != nil {
		delErr, ok := status.FromError(err)
		if !ok {
			t.Fatalf("unable to convert the error: %+v into a grpc status error type.", err)
		}
		if delErr.Code() == codes.Internal && strings.Contains(delErr.Message(), expectedErr.Error()) {
			t.Logf("received error as expected when attempting to create snapshot when CNS "+
				"returned CnsSnapshotCreatedFault, error: %+v.", err)
		} else {
			t.Fatalf("unexpected error received, expected: %s, received: %s.",
				expectedErr.Error(), delErr.Message())
		}
	} else {
		t.Fatal("expected error was not received for create snapshot operation.")
	}
}
