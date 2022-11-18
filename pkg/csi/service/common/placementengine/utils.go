package placementengine

import (
	"context"
	"sync"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/mo"

	cnsvsphere "sigs.k8s.io/vsphere-csi-driver/v2/pkg/common/cns-lib/vsphere"
	"sigs.k8s.io/vsphere-csi-driver/v2/pkg/csi/service/common"
	"sigs.k8s.io/vsphere-csi-driver/v2/pkg/csi/service/logger"
)

var (
	// TopologyVCMap maintains a cache of topology tags to the vCenter IP/FQDN which holds the tag.
	// Example - {region1: {VC1: struct{}{}, VC2: struct{}{}},
	//            zone1: {VC1: struct{}{}},
	//            zone2: {VC2: struct{}{}}}
	// The vCenter IP/FQDN under each tag are maintained as a map of string with nil values to improve
	// retrieval and deletion performance.
	TopologyVCMap map[string]map[string]struct{}
	// TODO: Multi-VC: make it per VC
	// preferredDatastoresMap is a map of topology domain to list of
	// datastore URLs preferred in that domain.
	// Ex: {vc1: {zone1: [DSURL1, DSURL2], zone2: [DSURL3]}, "vc2:}
	preferredDatastoresMap = make(map[string][]string)
	// preferredDatastoresMapInstanceLock guards the preferredDatastoresMap from read-write overlaps.
	preferredDatastoresMapInstanceLock = &sync.RWMutex{}
)

// TODO: Club to Get
func ClubAccessibilityRequirementsByVC(ctx context.Context, topoReq *csi.TopologyRequirement) (
	map[string][]map[string]string, error) {
	log := logger.GetLogger(ctx)
	// NOTE: We are only checking for preferred segments as vSphere CSI driver follows strict topology.
	if topoReq.GetPreferred() == nil {
		return nil, logger.LogNewErrorf(log, "No preferred segments specified in the "+
			"accessibility requirements.")
	}
	vcTopoSegmentsMap := make(map[string][]map[string]string)
	for _, topology := range topoReq.GetPreferred() {
		segments := topology.GetSegments()
		vcHost, err := getVCForTopologySegments(ctx, segments)
		if err != nil {
			return nil, logger.LogNewErrorf(log, "failed to fetch VC associated with topology segments %+v",
				segments)
		}
		vcTopoSegmentsMap[vcHost] = append(vcTopoSegmentsMap[vcHost], segments)
	}
	return vcTopoSegmentsMap, nil
}

// getVCForTopologySegments uses the topologyVCMap to retrieve the
// VC instance for the given topology segments map in a multi-VC environment.
// TODO: VC to vCenter
func getVCForTopologySegments(ctx context.Context, topologySegments map[string]string) (string, error) {
	log := logger.GetLogger(ctx)
	// vcCountMap keeps a cumulative count of the occurrences of
	// VCs across all labels in the given topology segment.
	vcCountMap := make(map[string]int)

	// Find the VC which contains all the labels given in the topologySegments.
	// For example, if topologyVCMap looks like
	// {"region-1": {"vc1": struct{}{}, "vc2": struct{}{} },
	// "zone-1": {"vc1": struct{}{} },
	// "zone-2": {"vc2": struct{}{} },}
	// For a given topologySegment
	// {"topology.csi.vmware.com/k8s-region": "region-1",
	// "topology.csi.vmware.com/k8s-zone": "zone-2"}
	// we will end up with a vcCountMap as follows: {"vc1": 1, "vc2": 2}
	// We go over the vcCountMap to check which VC has a count equal to
	// the len(topologySegment), in this case 2 and return that VC.
	for topologyKey, label := range topologySegments {
		if vcList, exists := TopologyVCMap[label]; exists {
			for vc := range vcList {
				vcCountMap[vc] = vcCountMap[vc] + 1
			}
		} else {
			return "", logger.LogNewErrorf(log, "Topology label %q not found in topology to VC mapping.",
				topologyKey+":"+label)
		}
	}
	var commonVCList []string
	numTopoLabels := len(topologySegments)
	for vc, count := range vcCountMap {
		// Add VCs to the commonVCList if they satisfied all the labels in the topology segment.
		if count == numTopoLabels {
			commonVCList = append(commonVCList, vc)
		}
	}
	switch {
	case len(commonVCList) > 1:
		return "", logger.LogNewErrorf(log, "Topology segment(s) %+v belong to more than one VC: %+v",
			topologySegments, commonVCList)
	case len(commonVCList) == 1:
		log.Infof("Topology segment(s) %+v belong to VC: %q", topologySegments, commonVCList[0])
		return commonVCList[0], nil
	}
	return "", logger.LogNewErrorf(log, "failed to find the VC associated with topology segments %+v",
		topologySegments)
}

// RefreshPreferentialDatastores refreshes the preferredDatastoresMap variable
// with latest information on the preferential datastores for each topology domain.
func RefreshPreferentialDatastores(ctx context.Context) error {
	log := logger.GetLogger(ctx)
	cnsCfg, err := common.GetConfig(ctx)
	if err != nil {
		return logger.LogNewErrorf(log, "failed to fetch CNS config. Error: %+v", err)
	}
	vcenterconfigs, err := cnsvsphere.GetVirtualCenterConfigs(ctx, cnsCfg)
	if err != nil {
		return logger.LogNewErrorf(log, "failed to get VirtualCenterConfig from CNS config. Error: %+v", err)
	}
	prefDatastoresMap := make(map[string][]string)
	for _, vcConfig := range vcenterconfigs {
		// Get VC instance.
		vcMgr := cnsvsphere.GetVirtualCenterManager(ctx)
		if err != nil {
			return logger.LogNewErrorf(log, "failed to get vCenter manager instance. Error: %+v", err)
		}
		vc, err := common.GetVCenterFromVCHost(ctx, vcMgr, vcConfig.Host)
		if err != nil {
			return logger.LogNewErrorf(log, "failed to get vCenter instance for host %q. Error: %+v",
				vcConfig.Host, err)
		}
		// Get tag manager instance.
		tagMgr, err := cnsvsphere.GetTagManager(ctx, vc)
		if err != nil {
			return logger.LogNewErrorf(log, "failed to create tag manager. Error: %+v", err)
		}
		defer func() {
			err := tagMgr.Logout(ctx)
			if err != nil {
				log.Errorf("failed to logout tagManager. Error: %v", err)
			}
		}()
		// Get tags for category reserved for preferred datastore tagging.
		tagIds, err := tagMgr.ListTagsForCategory(ctx, common.PreferredDatastoresCategory)
		if err != nil {
			log.Infof("failed to retrieve tags for category %q. Reason: %+v", common.PreferredDatastoresCategory,
				err)
			return nil
		}
		if len(tagIds) == 0 {
			log.Info("No preferred datastores found in environment.")
			return nil
		}
		// Fetch vSphere entities on which the tags have been applied.
		attachedObjs, err := tagMgr.GetAttachedObjectsOnTags(ctx, tagIds)
		if err != nil {
			return logger.LogNewErrorf(log, "failed to retrieve objects with tags %v. Error: %+v", tagIds, err)
		}
		for _, attachedObj := range attachedObjs {
			for _, obj := range attachedObj.ObjectIDs {
				// Preferred datastore tag should only be applied to datastores.
				if obj.Reference().Type != "Datastore" {
					log.Warnf("Preferred datastore tag applied on a non-datastore entity: %+v",
						obj.Reference())
					continue
				}
				// Fetch Datastore URL.
				var dsMo mo.Datastore
				dsObj := object.NewDatastore(vc.Client.Client, obj.Reference())
				err = dsObj.Properties(ctx, obj.Reference(), []string{"summary"}, &dsMo)
				if err != nil {
					return logger.LogNewErrorf(log, "failed to retrieve summary from datastore: %+v. Error: %v",
						obj.Reference(), err)
				}

				log.Infof("Datastore %q with URL %q is preferred in %q", dsMo.Summary.Name, dsMo.Summary.Url,
					attachedObj.Tag.Name)
				// For each topology domain, store the datastore URLs preferred in that domain.
				// TODO: Multi-VC: Storing all preferred DS across VCs in the same map. This means we cannot have same tag across different VCs.
				prefDatastoresMap[attachedObj.Tag.Name] = append(prefDatastoresMap[attachedObj.Tag.Name],
					dsMo.Summary.Url)
			}
		}
	}
	// Finally, write to cache.
	if len(prefDatastoresMap) != 0 {
		preferredDatastoresMapInstanceLock.Lock()
		defer preferredDatastoresMapInstanceLock.Unlock()
		preferredDatastoresMap = prefDatastoresMap
	}
	return nil
}

// GetPreferredDatastoresInSegments fetches preferred datastores in
// given topology segments as a map for faster retrieval.
func GetPreferredDatastoresInSegments(ctx context.Context, segments map[string]string) map[string]struct{} {
	log := logger.GetLogger(ctx)
	allPreferredDSURLs := make(map[string]struct{})

	preferredDatastoresMapInstanceLock.Lock()
	defer preferredDatastoresMapInstanceLock.Unlock()
	if len(preferredDatastoresMap) == 0 {
		return allPreferredDSURLs
	}
	// Arrange applicable preferred datastores as a map.
	for _, tag := range segments {
		preferredDS, ok := preferredDatastoresMap[tag]
		if ok {
			log.Infof("Found preferred datastores %+v for topology domain %q", preferredDS, tag)
			for _, val := range preferredDS {
				allPreferredDSURLs[val] = struct{}{}
			}
		}
	}
	return allPreferredDSURLs
}
