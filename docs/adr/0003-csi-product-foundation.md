# 0003. Kubernetes CSI를 제품 기반으로 사용한다

- 상태: Accepted
- 날짜: 2026-09-01

## Context

ShiftPV는 StorageClass를 통한 동적 provisioning과 kubelet의 표준 volume mount
lifecycle에 참여해야 한다. PVC를 감시해 HostPath PV를 직접 만드는 provisioner는
구현이 단순하지만 kubelet mount lifecycle을 제공하지 않는다.

## Decision

제품 interface는 `csi.shiftpv.io` CSI driver로 고정한다.

- Controller Deployment가 CSI Identity/Controller service를 제공한다.
- 표준 `external-provisioner`가 Controller service를 호출한다.
- Node Plugin DaemonSet가 CSI Identity/Node service와 bind mount를 제공한다.
- 표준 `node-driver-registrar`가 Node service를 kubelet에 등록한다.
- `CSIDriver.spec.attachRequired`는 `false`이며 `external-attacher`는 배포하지 않는다.
- RWO Filesystem, dynamic provisioning, `WaitForFirstConsumer`,
  `NodePublishVolume`과 `NodeUnpublishVolume`만 지원한다.
- 지원하지 않는 CSI capability는 광고하지 않는다.

## Consequences

- 일반 Kubernetes PVC/StorageClass 흐름을 사용한다.
- Controller, Node Plugin, CSI sidecar, Unix socket과 RBAC 배포가 필요하다.
- CSI RPC와 배포 계약을 unit test, Helm 검사와 kind E2E로 검증한다.

## References

- [Container Storage Interface specification](https://github.com/container-storage-interface/spec)
- [Kubernetes CSI sidecar containers](https://kubernetes-csi.github.io/docs/sidecar-containers.html)
- [Kubernetes CSI hostpath sample driver](https://github.com/kubernetes-csi/csi-driver-host-path)
