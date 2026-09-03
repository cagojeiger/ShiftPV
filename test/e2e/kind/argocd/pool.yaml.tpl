apiVersion: shiftpv.io/v1alpha1
kind: ShiftPVPool
metadata:
  name: worker
spec:
  nodeName: __WORKER_NODE__
  mountPath: /mnt/shiftpv
