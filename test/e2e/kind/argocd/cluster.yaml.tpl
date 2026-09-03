kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
  - role: worker
    labels:
      shiftpv.io/storage-node: "true"
    extraMounts:
      - hostPath: __WORKER_POOL__
        containerPath: /mnt/shiftpv
