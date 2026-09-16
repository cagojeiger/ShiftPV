apiVersion: v1
kind: Namespace
metadata:
  name: shiftpv-mobility-blocked
  labels:
    shiftpv.io/admission: enabled
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: source-only
  namespace: shiftpv-mobility-blocked
spec:
  accessModes:
    - ReadWriteOnce
  volumeMode: Filesystem
  # Preflight scenarios delete and recreate the namespace while retaining the
  # physical volume as a regression fixture, so they must opt into retention.
  storageClassName: shiftpv-retain
  resources:
    requests:
      storage: 16Mi
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: source-only
  namespace: shiftpv-mobility-blocked
spec:
  replicas: 1
  selector:
    matchLabels:
      app: shiftpv-mobility-source-only
  template:
    metadata:
      labels:
        app: shiftpv-mobility-source-only
    spec:
      nodeSelector:
        kubernetes.io/hostname: __SOURCE_NODE__
      terminationGracePeriodSeconds: 1
      containers:
        - name: writer
          image: busybox:1.37
          command:
            - sh
            - -ec
            - |
              if [ ! -f /data/payload ]; then
                printf 'ShiftPV blocked destination\n' > /data/payload
              fi
              sleep 3600
          volumeMounts:
            - name: data
              mountPath: /data
      volumes:
        - name: data
          persistentVolumeClaim:
            claimName: source-only
