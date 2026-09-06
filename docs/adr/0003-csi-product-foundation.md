# 0003. Kubernetes CSI를 제품 인터페이스로 사용한다

- 상태: Accepted
- 날짜: 2026-09-01
- 상세 계약: [CSI driver](../spec/csi-driver.md)
- 참고: [Container Storage Interface specification](https://github.com/container-storage-interface/spec)

## Context

ShiftPV는 StorageClass를 통한 동적 provisioning과 kubelet의 표준 volume lifecycle에
참여해야 한다. PVC를 감시해 HostPath PV를 직접 만드는 방식은 단순하지만 kubelet mount
lifecycle을 제공하지 않는다.

## Decision

ShiftPV의 Kubernetes storage interface는 CSI로 고정한다. 표준 CSI sidecar와 kubelet
registration을 사용하며, 구현하지 않는 capability는 광고하지 않는다.

## Alternatives considered

- 외부 provisioner가 HostPath PV를 직접 만들면 CSI Node lifecycle과 명확한 mount 권한 경계가 없다.
- 별도 scheduler나 in-tree volume plugin은 Kubernetes 표준 확장 경로와 맞지 않는다.

## Consequences

일반 PVC와 StorageClass 흐름을 사용할 수 있지만 Controller, Node Plugin, CSI sidecar,
Unix socket과 RBAC 배포가 필요하다.
