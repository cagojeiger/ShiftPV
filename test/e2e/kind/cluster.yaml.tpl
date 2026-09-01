kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
  - role: worker
    kubeadmConfigPatches:
      - |
        kind: JoinConfiguration
        nodeRegistration:
          kubeletExtraArgs:
            node-labels: "shiftpv.io/storage-node=true"
    extraMounts:
      - hostPath: "__WORKER_A_POOL__"
        containerPath: /mnt/shiftpv
  - role: worker
    kubeadmConfigPatches:
      - |
        kind: JoinConfiguration
        nodeRegistration:
          kubeletExtraArgs:
            node-labels: "shiftpv.io/storage-node=true"
    extraMounts:
      - hostPath: "__WORKER_B_POOL__"
        containerPath: /mnt/shiftpv
