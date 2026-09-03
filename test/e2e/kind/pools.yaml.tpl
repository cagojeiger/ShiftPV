apiVersion: shiftpv.io/v1alpha1
kind: ShiftPVPool
metadata:
  name: worker-a
spec:
  nodeName: __WORKER_A_NODE__
  mountPath: /mnt/shiftpv
---
apiVersion: shiftpv.io/v1alpha1
kind: ShiftPVPool
metadata:
  name: worker-b
spec:
  nodeName: __WORKER_B_NODE__
  mountPath: /srv/shiftpv-b
