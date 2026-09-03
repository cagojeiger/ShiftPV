# 0004. 최소 CSI bootstrap 범위를 고정한다

- 상태: Accepted
- 날짜: 2026-09-01
- 후속 결정: owner 권한과 PV topology는 [ADR 0005](0005-automatic-cordon-volume-mobility.md)가 대체한다.

## Context

첫 제품 상태는 StorageClass → PVC → CSI provision/publish와 Helm lifecycle이
실제 cluster에서 성립하는지 검증할 수 있을 만큼 작아야 한다.

## Decision

- Kubernetes 1.35 이상을 지원한다.
- Controller는 `WaitForFirstConsumer`가 선택한 node에서 helper Pod로
  `volumes/<csi-volume-id>` directory를 생성한다.
- namespace-scoped ConfigMap에 request name, volume ID, owner node와 requested
  capacity를 기록해 재시도와 재설치에 사용한다.
- Node Plugin은 `ShiftPVVolume.status`가 `Ready`이고 current owner가 자신과 같을 때만
  canonical directory를 kubelet target에 bind mount한다.
- Helm chart는 CSI workload, sidecar, RBAC, `CSIDriver`와 StorageClass를 소유한다.
  StorageClass의 cluster default 지정은 명시적으로 활성화한다.
- StorageClass는 `Retain`, `WaitForFirstConsumer`, RWO Filesystem,
  `shiftpv.io/capacity-enforcement: none`을 사용한다.
- isolated kind E2E는 control-plane 하나와 서로 다른 host directory를 가진 worker
  두 개로 실행한다.

## Consequences

- Helm 제거 전에 volume을 사용하는 workload를 중지해야 한다.
- Helm 제거 후에도 PVC, PV, reservation ConfigMap과 데이터 directory는 남는다.
- 같은 namespace와 Pool 등록으로 재설치하면 retained volume을 다시 publish할 수 있다.
- requested capacity는 표기와 idempotency 비교 값이며 실제 write limit가 아니다.
