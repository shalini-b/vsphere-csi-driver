/*
Copyright 2021 The Kubernetes Authors.

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

package csinodetopology

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	cnstypes "github.com/vmware/govmomi/cns/types"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"sigs.k8s.io/vsphere-csi-driver/v2/pkg/common/cns-lib/node"
	volumes "sigs.k8s.io/vsphere-csi-driver/v2/pkg/common/cns-lib/volume"
	cnsvsphere "sigs.k8s.io/vsphere-csi-driver/v2/pkg/common/cns-lib/vsphere"
	cnsconfig "sigs.k8s.io/vsphere-csi-driver/v2/pkg/common/config"
	"sigs.k8s.io/vsphere-csi-driver/v2/pkg/csi/service/common"
	"sigs.k8s.io/vsphere-csi-driver/v2/pkg/csi/service/common/commonco"
	"sigs.k8s.io/vsphere-csi-driver/v2/pkg/csi/service/logger"
	csitypes "sigs.k8s.io/vsphere-csi-driver/v2/pkg/csi/types"
	csinodetopologyv1alpha1 "sigs.k8s.io/vsphere-csi-driver/v2/pkg/internalapis/csinodetopology/v1alpha1"
	k8s "sigs.k8s.io/vsphere-csi-driver/v2/pkg/kubernetes"
	"sigs.k8s.io/vsphere-csi-driver/v2/pkg/syncer"
)

const defaultMaxWorkerThreadsForCSINodeTopology = 1

// backOffDuration is a map of csinodetopology instance name to the time after
// which a request for this instance will be requeued. Initialized to 1 second
// for new instances and for instances whose latest reconcile operation
// succeeded. If the reconcile fails, backoff is incremented exponentially.
var (
	backOffDuration         map[string]time.Duration
	backOffDurationMapMutex = sync.Mutex{}
)

// Add creates a new CSINodeTopology Controller and adds it to the Manager,
// ConfigurationInfo and VirtualCenterTypes. The Manager will set fields on the
// Controller and start it when the Manager is started.
func Add(mgr manager.Manager, clusterFlavor cnstypes.CnsClusterFlavor,
	configInfo *cnsconfig.ConfigurationInfo, volumeManager volumes.Manager) error {
	ctx, log := logger.GetNewContextWithLogger()
	if clusterFlavor != cnstypes.CnsClusterFlavorVanilla {
		log.Debug("Not initializing the CSINodetopology Controller as it is not a Vanilla CSI deployment")
		return nil
	}

	coCommonInterface, err := commonco.GetContainerOrchestratorInterface(ctx,
		common.Kubernetes, clusterFlavor, &syncer.COInitParams)
	if err != nil {
		log.Errorf("failed to create CO agnostic interface. Err: %v", err)
		return err
	}
	if !coCommonInterface.IsFSSEnabled(ctx, common.ImprovedVolumeTopology) {
		log.Infof("Not initializing the CSINodetopology Controller as %s FSS is disabled",
			common.ImprovedVolumeTopology)
		return nil
	}

	useNodeUuid := coCommonInterface.IsFSSEnabled(ctx, common.UseCSINodeId)
	// Initialize kubernetes client.
	k8sclient, err := k8s.NewClient(ctx)
	if err != nil {
		log.Errorf("creating Kubernetes client failed. Err: %v", err)
		return err
	}
	// eventBroadcaster broadcasts events on csinodetopology instances to the
	// event sink.
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartRecordingToSink(
		&typedcorev1.EventSinkImpl{
			Interface: k8sclient.CoreV1().Events(""),
		},
	)
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme,
		corev1.EventSource{Component: csinodetopologyv1alpha1.GroupName})
	return add(mgr, newReconciler(mgr, configInfo, recorder, useNodeUuid))
}

// newReconciler returns a new `reconcile.Reconciler`.
func newReconciler(mgr manager.Manager, configInfo *cnsconfig.ConfigurationInfo,
	recorder record.EventRecorder, useNodeUuid bool) reconcile.Reconciler {
	return &ReconcileCSINodeTopology{client: mgr.GetClient(), scheme: mgr.GetScheme(),
		configInfo: configInfo, recorder: recorder, useNodeUuid: useNodeUuid}
}

// add adds a new Controller to mgr with r as the `reconcile.Reconciler`.
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	log := logger.GetLoggerWithNoContext()

	// Create a new controller.
	c, err := controller.New("csinodetopology-controller", mgr, controller.Options{Reconciler: r,
		MaxConcurrentReconciles: defaultMaxWorkerThreadsForCSINodeTopology})
	if err != nil {
		log.Errorf("failed to create new CSINodetopology controller with error: %+v", err)
		return err
	}

	// Initialize backoff duration map.
	backOffDuration = make(map[string]time.Duration)

	// Predicates are used to determine under which conditions the reconcile
	// callback will be made for an instance.
	pred := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return true
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			// The CO calls NodeGetInfo API just once during the node registration,
			// therefore we do not support updates to the spec after the CR has
			// been reconciled.
			log.Debug("Ignoring CSINodeTopology reconciliation on update event")
			return false
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			// Instances are deleted by the garbage collector automatically after
			// the corresponding NodeVM is deleted. No reconcile operations are
			// required.
			log.Debug("Ignoring CSINodeTopology reconciliation on delete event")
			return false
		},
	}

	// Watch for changes to primary resource CSINodeTopology.
	err = c.Watch(&source.Kind{Type: &csinodetopologyv1alpha1.CSINodeTopology{}},
		&handler.EnqueueRequestForObject{}, pred)
	if err != nil {
		log.Errorf("Failed to watch for changes to CSINodeTopology resource with error: %+v", err)
		return err
	}
	log.Info("Started watching on CSINodeTopology resources")
	return nil
}

// blank assignment to verify that ReconcileCSINodeTopology implements
// `reconcile.Reconciler`.
var _ reconcile.Reconciler = &ReconcileCSINodeTopology{}

// ReconcileCSINodeTopology reconciles a CSINodeTopology object.
type ReconcileCSINodeTopology struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver.
	client      client.Client
	scheme      *runtime.Scheme
	configInfo  *cnsconfig.ConfigurationInfo
	recorder    record.EventRecorder
	useNodeUuid bool
}

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// Note: The Controller will requeue the Request to be processed again if the
// returned error is non-nil or Result.Requeue is true, otherwise upon
// completion it will remove the work from the queue.
func (r *ReconcileCSINodeTopology) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	log := logger.GetLogger(ctx)

	// Fetch the CSINodeTopology instance.
	instance := &csinodetopologyv1alpha1.CSINodeTopology{}
	err := r.client.Get(ctx, request.NamespacedName, instance)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Infof("CSINodeTopology resource with name %q not found. Ignoring since object must have "+
				"been deleted.", request.Name)
			return reconcile.Result{}, nil
		}
		log.Errorf("Failed to fetch the CSINodeTopology instance with name: %q. Error: %+v", request.Name, err)
		// Error reading the object - return with err.
		return reconcile.Result{}, err
	}

	// Initialize backOffDuration for the instance, if required.
	backOffDurationMapMutex.Lock()
	var timeout time.Duration
	if _, exists := backOffDuration[instance.Name]; !exists {
		backOffDuration[instance.Name] = time.Second
	}
	timeout = backOffDuration[instance.Name]
	backOffDurationMapMutex.Unlock()

	// Get NodeVM instance.
	nodeID := instance.Spec.NodeID
	nodeManager := node.GetManager(ctx)
	var nodeVM *cnsvsphere.VirtualMachine
	if r.useNodeUuid {
		nodeUuid := nodeID
		nodeVM, err = nodeManager.GetNode(ctx, nodeUuid, nil)
	} else {
		nodeVM, err = nodeManager.GetNodeByName(ctx, nodeID)
	}
	if err != nil {
		if err == node.ErrNodeNotFound {
			log.Warnf("Node %q is not yet registered in the node manager. Error: %+v", nodeID, err)
			return reconcile.Result{}, err
		}
		// If nodeVM not found, ignore reconcile call.
		msg := fmt.Sprintf("failed to retrieve nodeVM %q using the node manager. Error: %+v", nodeID, err)
		log.Error(msg)
		_ = updateCRStatus(ctx, r, instance, csinodetopologyv1alpha1.CSINodeTopologyError, msg)
		return reconcile.Result{RequeueAfter: timeout}, nil
	}

	// Retrieve topology labels for nodeVM.
	if r.configInfo.Cfg.Labels.TopologyCategories == "" &&
		r.configInfo.Cfg.Labels.Zone == "" && r.configInfo.Cfg.Labels.Region == "" {
		// Not a topology aware setup.
		// Set the Status to Success and return.
		instance.Status.TopologyLabels = make([]csinodetopologyv1alpha1.TopologyLabel, 0)
		err = updateCRStatus(ctx, r, instance, csinodetopologyv1alpha1.CSINodeTopologySuccess,
			"Not a topology aware cluster.")
		if err != nil {
			return reconcile.Result{RequeueAfter: timeout}, nil
		}
	} else if r.configInfo.Cfg.Labels.TopologyCategories != "" ||
		(r.configInfo.Cfg.Labels.Zone != "" && r.configInfo.Cfg.Labels.Region != "") {
		log.Infof("Detected a topology aware cluster")

		// Fetch topology labels for nodeVM.
		topologyLabels, err := getNodeTopologyInfo(ctx, nodeVM, r.configInfo.Cfg)
		if err != nil {
			msg := fmt.Sprintf("failed to fetch topology information for the nodeVM %q. Error: %v",
				instance.Name, err)
			log.Error(msg)
			_ = updateCRStatus(ctx, r, instance, csinodetopologyv1alpha1.CSINodeTopologyError, msg)
			return reconcile.Result{RequeueAfter: timeout}, nil
		}

		// Update CSINodeTopology instance.
		instance.Status.TopologyLabels = topologyLabels
		err = updateCRStatus(ctx, r, instance, csinodetopologyv1alpha1.CSINodeTopologySuccess,
			fmt.Sprintf("Topology labels successfully updated for nodeVM %q", instance.Name))
		if err != nil {
			return reconcile.Result{RequeueAfter: timeout}, nil
		}
	} else {
		msg := "missing zone or region information in vSphere CSI config `Labels` section"
		log.Error(msg)
		_ = updateCRStatus(ctx, r, instance, csinodetopologyv1alpha1.CSINodeTopologyError, msg)
		return reconcile.Result{RequeueAfter: timeout}, nil
	}

	// On successful event, remove instance from backOffDuration.
	backOffDurationMapMutex.Lock()
	delete(backOffDuration, instance.Name)
	backOffDurationMapMutex.Unlock()
	log.Infof("Successfully updated topology labels for nodeVM %q", instance.Name, nodeID)
	return reconcile.Result{}, nil
}

func updateCRStatus(ctx context.Context, r *ReconcileCSINodeTopology, instance *csinodetopologyv1alpha1.CSINodeTopology,
	status csinodetopologyv1alpha1.CRDStatus, eventMessage string) error {
	log := logger.GetLogger(ctx)

	instance.Status.Status = status
	switch status {
	case csinodetopologyv1alpha1.CSINodeTopologySuccess:
		// Reset error message if previously populated.
		instance.Status.ErrorMessage = ""
		// Record an event on the CR.
		r.recorder.Event(instance, corev1.EventTypeNormal, "TopologyRetrievalSucceeded", eventMessage)
	case csinodetopologyv1alpha1.CSINodeTopologyError:
		// Increase backoff duration for the instance.
		backOffDurationMapMutex.Lock()
		backOffDuration[instance.Name] = backOffDuration[instance.Name] * 2
		backOffDurationMapMutex.Unlock()

		// Record an event on the CR.
		instance.Status.ErrorMessage = eventMessage
		r.recorder.Event(instance, corev1.EventTypeWarning, "TopologyRetrievalFailed", eventMessage)
	}

	// Update CSINodeTopology instance.
	err := r.client.Update(ctx, instance)
	if err != nil {
		msg := fmt.Sprintf("Failed to update the CSINodeTopology instance with name %q. Error: %+v",
			instance.Name, err)
		r.recorder.Event(instance, corev1.EventTypeWarning, "CSINodeTopologyUpdateFailed", msg)
		return logger.LogNewError(log, msg)
	}
	return nil
}

func getNodeTopologyInfo(ctx context.Context, nodeVM *cnsvsphere.VirtualMachine, cfg *cnsconfig.Config) (
	[]csinodetopologyv1alpha1.TopologyLabel, error) {
	log := logger.GetLogger(ctx)

	// Get VC instance.
	vcenter, err := cnsvsphere.GetVirtualCenterInstance(ctx, &cnsconfig.ConfigurationInfo{Cfg: cfg}, false)
	if err != nil {
		log.Errorf("failed to get virtual center instance with error: %v", err)
		return nil, err
	}

	// Get tag manager instance.
	tagManager, err := cnsvsphere.GetTagManager(ctx, vcenter)
	if err != nil {
		log.Errorf("failed to create tagManager. Error: %v", err)
		return nil, err
	}
	defer func() {
		err := tagManager.Logout(ctx)
		if err != nil {
			log.Errorf("failed to logout tagManager. Error: %v", err)
		}
	}()

	// Create a map of topologyCategories with category as key and value as empty string.
	var isZoneRegion bool
	zoneCat := strings.TrimSpace(cfg.Labels.Zone)
	regionCat := strings.TrimSpace(cfg.Labels.Region)
	topologyCategories := make(map[string]string)
	if strings.TrimSpace(cfg.Labels.TopologyCategories) != "" {
		categories := strings.Split(cfg.Labels.TopologyCategories, ",")
		for _, cat := range categories {
			topologyCategories[strings.TrimSpace(cat)] = ""
		}
	} else if zoneCat != "" && regionCat != "" {
		isZoneRegion = true
		topologyCategories[zoneCat] = ""
		topologyCategories[regionCat] = ""
	}

	// Populate topology labels for NodeVM corresponding to each category in topologyCategories map.
	err = nodeVM.GetTopologyLabels(ctx, tagManager, topologyCategories)
	if err != nil {
		log.Errorf("failed to get accessibleTopology for nodeVM: %v, Error: %v", nodeVM.Reference(), err)
		return nil, err
	}
	log.Infof("NodeVM %q belongs to topology: %+v", nodeVM.Reference(), topologyCategories)
	topologyLabels := make([]csinodetopologyv1alpha1.TopologyLabel, 0)
	if isZoneRegion {
		// Create the kubernetes client.
		k8sClient, err := k8s.NewClient(ctx)
		if err != nil {
			log.Errorf("failed to create K8s client. Error: %v", err)
			return nil, err
		}

		// In order to keep backward compatibility, we will discover existing topology keys
		// from CSINodes in the cluster. Few examples on how the discovery works:
		// CSINode   Topology Key
		// N1        topology.kubernetes.io/XYZ
		// N2        topology.kubernetes.io/XYZ
		// After the cluster is up and running, nodes N1 and N2 will have topology keys of the
		// form `topology.kubernetes.io/XYZ`.
		//
		// CSINode    Topology Key
		// N1         failure-domain.beta.kubernetes.io/XYZ
		// N2         No label
		// After the cluster is up and running, nodes N1 and N2 will have topology keys of the
		// form `failure-domain.beta.kubernetes.io/XYZ`.
		//
		// CSINode    Topology Key
		// N1         failure-domain.beta.kubernetes.io/XYZ
		// N2         topology.kubernetes.io/XYZ
		// Node registration will fail as this is an unsupported configuration for the vSphere
		// CSI driver.
		//
		// CSINode    Topology Key
		// N1         No label
		// N2         No label
		// After the cluster is up and running, nodes N1 and N2 will have topology keys of the
		// form `topology.csi.vmware.com/XYZ`.
		var zoneLabel, regionLabel string
		csiNodeList, err := k8sClient.StorageV1().CSINodes().List(ctx, metav1.ListOptions{})
		if err != nil {
			_, ok := err.(*apiMeta.NoKindMatchError)
			if ok {
				// CSINode not registered. Might be the in-tree provider.
				log.Infof("CSINode CR not registered in environment. " +
					"Defaulting topology labels to custom VMware labels.")
				zoneLabel = common.TopologyLabelsDomain + "/" + zoneCat
				regionLabel = common.TopologyLabelsDomain + "/" + regionCat
			} else {
				return nil, logger.LogNewErrorf(log, "failed to list CSINodes in the cluster. Error: %+v", err)
			}
		} else {
			zoneLabel, regionLabel, err = findExistingTopologyLabels(ctx, csiNodeList.Items)
			if err != nil {
				return nil, logger.LogNewErrorf(log, "failed to discover existing topology keys "+
					"from the CSINodes of the cluster. Error: %+v", err)
			}
			if zoneLabel == "" && regionLabel == "" {
				log.Infof("Did not find any existing standard topology keys in the CSINodes of the " +
					"cluster, using custom VMware CSI topology labels instead.")
				zoneLabel = common.TopologyLabelsDomain + "/" + zoneCat
				regionLabel = common.TopologyLabelsDomain + "/" + regionCat
			}
		}

		for key, val := range topologyCategories {
			switch key {
			case zoneCat:
				topologyLabels = append(topologyLabels,
					csinodetopologyv1alpha1.TopologyLabel{Key: zoneLabel, Value: val})
			case regionCat:
				topologyLabels = append(topologyLabels,
					csinodetopologyv1alpha1.TopologyLabel{Key: regionLabel, Value: val})
			}
		}
	} else {
		// Prefix user-defined topology labels with TopologyLabelsDomain name to distinctly
		// identify the topology labels on the kubernetes node object added by our driver.
		for key, val := range topologyCategories {
			topologyLabels = append(topologyLabels,
				csinodetopologyv1alpha1.TopologyLabel{Key: common.TopologyLabelsDomain + "/" + key, Value: val})
		}
	}
	return topologyLabels, nil
}

// findExistingTopologyLabels figures out if the given list of nodes were already participating
// in a topology setup. If yes, it will discover if the nodes use the standard topology beta
// labels or the GA labels by reading the TopologyKeys parameter in the corresponding CSINodes
// and return those. However, if the cluster uses a mixed set of beta and GA labels, it will
// error out and request the customer to fix the environment before proceeding further. If no
//TopologyKeys were found on all the CSINodes in a topology aware environment, it will return
// empty strings.
func findExistingTopologyLabels(ctx context.Context, csiNodeList []storagev1.CSINode) (string, string, error) {
	log := logger.GetLogger(ctx)
	labelMap := map[string]bool{
		"beta": false,
		"GA":   false,
	}
	for _, csiNode := range csiNodeList {
		for _, driver := range csiNode.Spec.Drivers {
			if driver.Name == csitypes.Name {
				for _, topoKey := range driver.TopologyKeys {
					if topoKey == corev1.LabelTopologyZone || topoKey == corev1.LabelTopologyRegion {
						labelMap["GA"] = true
					}
					if topoKey == corev1.LabelFailureDomainBetaZone || topoKey == corev1.LabelFailureDomainBetaRegion {
						labelMap["beta"] = true
					}
				}
				break
			}
		}
		if labelMap["GA"] && labelMap["beta"] {
			return "", "", logger.LogNewErrorf(log, "found mixed set of topology keys on CSINode(s) in "+
				"the cluster. The parameter `TopologyKeys` in the CSINodes of the cluster should either have the "+
				"standard topology beta labels starting with \"failure-domain.beta.kubernetes.io\" or "+
				"standard topology GA labels starting with \"topology.kubernetes.io\".")
		}
	}
	switch {
	case labelMap["GA"]:
		log.Infof("Found standard topology GA labels on the existing CSINodes in the cluster.")
		return corev1.LabelTopologyZone, corev1.LabelTopologyRegion, nil
	case labelMap["beta"]:
		log.Infof("Found standard topology beta labels on the existing CSINodes in the cluster.")
		return corev1.LabelFailureDomainBetaZone, corev1.LabelFailureDomainBetaRegion, nil
	}
	return "", "", nil
}
