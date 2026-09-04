apiVersion: v1
kind: Namespace
metadata:
  name: __NAMESPACE__
  labels:
    shiftpv.io/admission: enabled
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: data
  namespace: __NAMESPACE__
spec:
  accessModes:
    - ReadWriteOnce
  volumeMode: Filesystem
  storageClassName: shiftpv
  resources:
    requests:
      storage: 16Mi
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: writer
  namespace: __NAMESPACE__
spec:
  replicas: 1
  selector:
    matchLabels:
      app: __NAMESPACE__
  template:
    metadata:
      labels:
        app: __NAMESPACE__
    spec:
      containers:
        - name: writer
          image: busybox:1.37
          command:
            - sh
            - -ec
            - |
              if [ ! -f /data/payload ]; then
                printf '%s\n' '__PAYLOAD__' > /data/payload
              fi
              sleep 3600
          volumeMounts:
            - name: data
              mountPath: /data
      volumes:
        - name: data
          persistentVolumeClaim:
            claimName: data
