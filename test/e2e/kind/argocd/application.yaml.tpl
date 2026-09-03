apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: shiftpv
  namespace: argocd
  finalizers:
    - resources-finalizer.argocd.argoproj.io/foreground
spec:
  project: default
  source:
    repoURL: http://shiftpv-chart-repository.shiftpv-chart-repository.svc.cluster.local
    chart: shiftpv
    targetRevision: __CHART_VERSION__
    helm:
      releaseName: shiftpv
      valuesObject:
        controller:
          image:
            repository: __IMAGE_REPOSITORY__
            tag: __IMAGE_TAG__
            pullPolicy: Never
        mobility:
          enabled: false
        lifecycle:
          uninstallMode: argocd
        node:
          image:
            repository: __IMAGE_REPOSITORY__
            tag: __IMAGE_TAG__
            pullPolicy: Never
          nodeSelector:
            shiftpv.io/storage-node: "true"
        storageClass:
          defaultClass: false
  destination:
    server: https://kubernetes.default.svc
    namespace: shiftpv-system
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
    syncOptions:
      - CreateNamespace=true
