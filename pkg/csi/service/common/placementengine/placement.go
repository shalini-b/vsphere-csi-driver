package placementengine

import (
	"context"
	"reflect"

	vimtypes "github.com/vmware/govmomi/vim25/types"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"sigs.k8s.io/vsphere-csi-driver/v2/pkg/common/cns-lib/node"
	cnsvsphere "sigs.k8s.io/vsphere-csi-driver/v2/pkg/common/cns-lib/vsphere"
	"sigs.k8s.io/vsphere-csi-driver/v2/pkg/csi/service/common/commonco"
	"sigs.k8s.io/vsphere-csi-driver/v2/pkg/csi/service/logger"
	csinodetopologyv1alpha1 "sigs.k8s.io/vsphere-csi-driver/v2/pkg/internalapis/csinodetopology/v1alpha1"
)

func GetSharedDatastores(ctx context.Context, topologySegmentsList []map[string]string, storagePolicyID string,
	vcenter *cnsvsphere.VirtualCenter, isCSINodeIdFeatureEnabled, isTopologyPreferentialDatastoresFSSEnabled bool) (
	[]*cnsvsphere.DatastoreInfo, error) {
	log := logger.GetLogger(ctx)
	var sharedDatastores []*cnsvsphere.DatastoreInfo
	nodeMgr := node.GetManager(ctx)

	// Iterate through each set of topology segments and find shared datastores for that segment.
	for _, segments := range topologySegmentsList {
		// Fetch nodes compatible with the requested topology segments.
		matchingNodeVMs, completeTopologySegments, err := getTopologySegmentsWithMatchingNodes(ctx,
			segments, nodeMgr, isCSINodeIdFeatureEnabled)
		if err != nil {
			log.Errorf("failed to find nodes in topology segment %+v. Error: %+v", segments, err)
			return nil, err
		}
		if len(matchingNodeVMs) == 0 {
			log.Warnf("No nodes in the cluster matched the topology requirement provided: %+v",
				segments)
			continue
		}
		log.Infof("Obtained list of nodeVMs %+v", matchingNodeVMs)

		// Fetch shared datastores for the matching nodeVMs.
		sharedDatastoresInTopology, err := cnsvsphere.GetSharedDatastoresForVMs(ctx, matchingNodeVMs)
		if err != nil {
			log.Errorf("failed to get shared datastores for nodes: %+v in topology segment %+v."+
				" Error: %+v", matchingNodeVMs, segments, err)
			return nil, err
		}
		log.Infof("Obtained list of shared datastores as %+v", sharedDatastoresInTopology)

		// Check storage policy compatibility, if given.
		if storagePolicyID != "" {
			var sharedDSMoRef []vimtypes.ManagedObjectReference
			for _, ds := range sharedDatastoresInTopology {
				sharedDSMoRef = append(sharedDSMoRef, ds.Reference())
			}
			compat, err := vcenter.PbmCheckCompatibility(ctx, sharedDSMoRef, storagePolicyID)
			if err != nil {
				return nil, logger.LogNewErrorf(log, "failed to find datastore compatibility "+
					"with storage policy ID %q. Error: %+v", storagePolicyID, err)
			}
			compatibleDsMoids := make(map[string]struct{})
			for _, ds := range compat.CompatibleDatastores() {
				compatibleDsMoids[ds.HubId] = struct{}{}
			}
			log.Infof("Datastores compatible with storage policy %q are %+v", storagePolicyID,
				compatibleDsMoids)

			// Filter compatible datastores from shared datastores list.
			var compatibleDatastores []*cnsvsphere.DatastoreInfo
			for _, ds := range sharedDatastoresInTopology {
				if _, exists := compatibleDsMoids[ds.Reference().Value]; exists {
					compatibleDatastores = append(compatibleDatastores, ds)
				}
			}
			if len(compatibleDatastores) == 0 {
				// TODO: Final error
				return nil, logger.LogNewErrorf(log, "No compatible shared datastores found "+
					"for storage policy %q", storagePolicyID)
			}
			sharedDatastoresInTopology = compatibleDatastores
		}
		// Further, filter the compatible datastores with preferential datastores, if any.
		if isTopologyPreferentialDatastoresFSSEnabled && len(preferredDatastoresMap) != 0 {
			// Fetch all preferred datastore URLs for the matching topology segments.
			allPreferredDSURLs := make(map[string]struct{})
			for _, topoSegs := range completeTopologySegments {
				// TODO: modify this
				prefDS := GetPreferredDatastoresInSegments(ctx, topoSegs)
				for key, val := range prefDS {
					allPreferredDSURLs[key] = val
				}
			}
			if len(allPreferredDSURLs) != 0 {
				// If there are preferred datastores among the compatible
				// datastores, choose the preferred datastores, otherwise
				// choose the compatible datastores.
				var preferredDS []*cnsvsphere.DatastoreInfo
				for _, dsInfo := range sharedDatastoresInTopology {
					if _, ok := allPreferredDSURLs[dsInfo.Info.Url]; ok {
						preferredDS = append(preferredDS, dsInfo)
					}
				}
				if len(preferredDS) != 0 {
					sharedDatastoresInTopology = preferredDS
					log.Infof("Using preferred datastores: %+v", preferredDS)
				}
			}
		}

		// Update sharedDatastores with the list of datastores received.
		// Duplicates will not be added.
		for _, ds := range sharedDatastoresInTopology {
			var found bool
			for _, sharedDS := range sharedDatastores {
				if sharedDS.Info.Url == ds.Info.Url {
					found = true
					break
				}
			}
			if !found {
				sharedDatastores = append(sharedDatastores, ds)
			}
		}
	}
	log.Infof("Obtained shared datastores: %+v", sharedDatastores)
	return sharedDatastores, nil
}

func getTopologySegmentsWithMatchingNodes(ctx context.Context, requestedSegments map[string]string,
	nodeMgr node.Manager, isCSINodeIdFeatureEnabled bool) ([]*cnsvsphere.VirtualMachine, []map[string]string, error) {
	log := logger.GetLogger(ctx)

	var (
		vcHost                   string
		matchingNodeVMs          []*cnsvsphere.VirtualMachine
		completeTopologySegments []map[string]string
	)
	// Fetch node topology information from informer cache.
	for _, val := range commonco.ContainerOrchestratorUtility.GetCSINodeTopologyInstancesList() {
		var nodeTopologyInstance csinodetopologyv1alpha1.CSINodeTopology
		// Validate the object received.
		err := runtime.DefaultUnstructuredConverter.FromUnstructured(val.(*unstructured.Unstructured).Object,
			&nodeTopologyInstance)
		if err != nil {
			return nil, nil, logger.LogNewErrorf(log, "failed to convert unstructured object %+v to "+
				"CSINodeTopology instance. Error: %+v", val, err)
		}

		// Check CSINodeTopology instance `Status` field for success.
		if nodeTopologyInstance.Status.Status != csinodetopologyv1alpha1.CSINodeTopologySuccess {
			return nil, nil, logger.LogNewErrorf(log, "node %q not yet ready. Found CSINodeTopology instance "+
				"status: %q with error message: %q", nodeTopologyInstance.Name, nodeTopologyInstance.Status.Status,
				nodeTopologyInstance.Status.ErrorMessage)
		}
		// Convert array of labels to map.
		topoLabelsMap := make(map[string]string)
		for _, topoLabel := range nodeTopologyInstance.Status.TopologyLabels {
			topoLabelsMap[topoLabel.Key] = topoLabel.Value
		}
		// Check for a match of labels in every segment.
		isMatch := true
		for key, value := range requestedSegments {
			if topoLabelsMap[key] != value {
				log.Debugf("Node %q with topology %+v did not match the topology requirement - %q: %q ",
					nodeTopologyInstance.Name, topoLabelsMap, key, value)
				isMatch = false
				break
			}
		}
		// If there is a match, fetch the nodeVM object and add it to matchingNodeVMs.
		if isMatch {
			var nodeVM *cnsvsphere.VirtualMachine
			if isCSINodeIdFeatureEnabled {
				nodeVM, err = nodeMgr.GetNode(ctx, nodeTopologyInstance.Spec.NodeUUID, nil)
			} else {
				nodeVM, err = nodeMgr.GetNodeByName(ctx, nodeTopologyInstance.Spec.NodeID)
			}
			if err != nil {
				return nil, nil, logger.LogNewErrorf(log,
					"failed to retrieve NodeVM %q. Error - %+v", nodeTopologyInstance.Spec.NodeID, err)
			}
			// Check if each compatible NodeVM belongs to the same VC. If not,
			// error out as we do not support cross-zonal volume provisioning.
			if vcHost == "" {
				vcHost = nodeVM.VirtualCenterHost
			} else if vcHost != nodeVM.VirtualCenterHost {
				// TODO: expected VC vs newly found VC
				return nil, nil, logger.LogNewErrorf(log,
					"found compatible NodeVMs belonging to two different VCs: %q, %q", vcHost,
					nodeVM.VirtualCenterHost)
			}
			matchingNodeVMs = append(matchingNodeVMs, nodeVM)

			// Store the complete hierarchy of topology requestedSegments for future use.
			var exists bool
			for _, segs := range completeTopologySegments {
				if reflect.DeepEqual(segs, topoLabelsMap) {
					exists = true
					break
				}
			}
			if !exists {
				completeTopologySegments = append(completeTopologySegments, topoLabelsMap)
			}
		}
	}
	return matchingNodeVMs, completeTopologySegments, nil
}
