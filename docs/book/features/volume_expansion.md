# vSphere CSI Driver - Volume Expansion

CSI Volume Expansion was introduced as an alpha feature in Kubernetes 1.14 and it was promoted to beta in Kubernetes 1.16. The vSphere CSI driver supports volume expansion for dynamically/statically created **block** volumes only. Kubernetes supports two modes of volume expansion - offline and online. When the PVC is being used by a Pod i.e it is mounted on a node, the resulting volume expansion operation is termed as an online expansion. In all other cases, it is an offline expansion. Depending upon the kubernetes flavor and the mode of volume expansion required for your use case, refer to the table below to know the minimum version of the vSphere CSI driver to be used.

| vSphere CSI flavor                                              | Vanilla                     |      Supervisor cluster                    | Tanzu Kubernetes Grid Service (TKGS) | 
|-----------------------------------------------------|-----------------------------------|--------------------------------------|----------------------------|
| Offline volume expansion support                                               | vSphere CSI driver v2.0 and above | vSphere version 7.0U2 and above | vSphere version 7.0U1 and above               |
|         Online volume expansion support                                            |    vSphere CSI driver v2.2 and above           |    vSphere version 7.0U2 and above                                  |     vSphere version 7.0U2 and above                       |                       |


**NOTE**: vSphere CSI driver v2.2 is not yet released. 

For more information, check the [supported features](../supported_features_matrix.md) section to verify if your environment conforms to all the required versions and the [known issues](../known_issues.md) section to see if this feature caters to your requirement. 

## Feature Gate

Expand CSI Volumes feature was promoted to beta in kubernetes 1.16, therefore it is enabled by default. For Kubernetes releases before 1.16, ExpandCSIVolumes feature gate needs to be enabled for this feature to support volume expansion in CSI drivers.

## Sidecar Container

An external-resizer sidecar container implements the logic of watching the Kubernetes API for Persistent Volume claim edits, issuing the ControllerExpandVolume RPC call against a CSI endpoint and updating the PersistentVolume object to reflect the new size. This container has already been deployed for you as a part of the vsphere-csi-controller pod.

## Requirements

If you are either on the supervisor cluster or on TKGS, check if your environment adheres to the required kubernetes and vSphere CSI driver versions mentioned above and skip this section to directly proceed to the `Expand PVC` section below to use this feature.

However, in order to try this feature out on the vanilla kubernetes driver, you need to modify the StorageClass definition in your environment as mentioned below.

### StorageClass

Create a new StorageClass or edit the existing StorageClass to set `allowVolumeExpansion` to true.

```yaml
kind: StorageClass
apiVersion: storage.k8s.io/v1
metadata:
  name: example-block-sc
provisioner: csi.vsphere.vmware.com
allowVolumeExpansion: true
```

Proceed to create/edit a PVC by using this storage class.

## Expand PVC

Prior to increasing the size of a PVC make sure that the PVC is in `Bound` state. 

### Online mode

Patch the PVC to increase its requested storage size (in this case, to `2Gi`):

```bash
kubectl patch pvc example-block-pvc -p '{"spec": {"resources": {"requests": {"storage": "2Gi"}}}}'
```



### Offline mode

Patch the PVC to increase its requested storage size (in this case, to `2Gi`):

```bash
kubectl patch pvc example-block-pvc -p '{"spec": {"resources": {"requests": {"storage": "2Gi"}}}}'
```

This will trigger an expansion in the volume associated with the PVC in vSphere Cloud Native Storage which finally gets reflected on the capacity of the corresponding PV object. Note that the capacity of the PVC will not change until the PVC is used by a Pod i.e mounted on a node.

```bash
kubectl get pv
NAME                                       CAPACITY ACCESS MODES RECLAIM POLICY STATUS   CLAIM                       STORAGECLASS           REASON AGE
pvc-9e9a325d-ee1c-11e9-a223-005056ad1fc1   2Gi           RWO         Delete     Bound    default/example-block-pvc   example-block-sc              6m44s

kubectl get pvc
NAME                STATUS VOLUME                                     CAPACITY ACCESS MODES   STORAGECLASS       AGE
example-block-pvc   Bound  pvc-9e9a325d-ee1c-11e9-a223-005056ad1fc1   1Gi           RWO       example-block-sc   6m57s
```

As you can see above, the capacity of the PVC is unchanged. You will also notice a `FilesystemResizePending` condition applied on the PVC when you `describe` it.

Now create a pod to use the PVC:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: example-block-pod
spec:
  containers:
  - name: test-container
    image: gcr.io/google_containers/busybox:1.24
    command: ["/bin/sh", "-c", "echo 'hello' > /mnt/volume1/index.html  && chmod o+rX /mnt /mnt/volume1/index.html && while true ; do sleep 2 ; done"]
    volumeMounts:
    - name: test-volume
      mountPath: /mnt/volume1
  restartPolicy: Never
  volumes:
  - name: test-volume
    persistentVolumeClaim:
      claimName: example-block-pvc
```

```bash
kubectl create -f example-pod.yaml
pod/example-block-pod created
```

The Kubelet on the node will trigger the filesystem expansion on the volume when the PVC is attached to the Pod.

```bash
kubectl get pod
NAME               READY STATUS  RESTARTS AGE
example-block-pod   1/1  Running 0        65s
```

```bash
kubectl get pvc
NAME                STATUS VOLUME                                    CAPACITY ACCESS MODES STORAGECLASS     AGE
example-block-pvc   Bound  pvc-24114458-9753-428e-9c90-9f568cb25788   2Gi         RWO      example-block-sc 2m12s

kubectl get pv
NAME                                       CAPACITY ACCESS MODES RECLAIM POLICY STATUS   CLAIM                     STORAGECLASS           REASON AGE
pvc-24114458-9753-428e-9c90-9f568cb25788   2Gi           RWO        Delete      Bound    default/example-block-pvc example-block-sc              2m3s
```

You will notice that the capacity of PVC has been modified and the `FilesystemResizePending` condition has been removed from the PVC. Offline volume expansion is complete.
