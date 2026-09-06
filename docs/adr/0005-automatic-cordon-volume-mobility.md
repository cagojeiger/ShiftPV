# 0005. 정상 cordon 이동은 Kubernetes 배치와 협력한다

- 상태: Accepted
- 날짜: 2026-09-02
- 후속 결정: 이동 전 중단 방지는 [ADR 0009](0009-nondisruptive-mobility-preflight.md)가 보완한다.
- 상세 계약: [volume mobility](../spec/volume-mobility.md)

## Context

한 node에만 authoritative data가 있는 ShiftPV volume을 계획 정비 중 다른 Pool로 옮기려면
PVC/PV identity와 workload controller의 책임을 유지해야 한다. CSI에는 MoveVolume RPC가
없고 일반 drain은 storage migration 완료를 기다리지 않는다.

## Decision

ShiftPV는 정상 source node의 cordon을 planned cold migration trigger로 사용한다. 실제
workload는 scheduling gate로 대기시키고, 별도 placement Pod를 kube-scheduler에 제출해
destination을 선택한다. ShiftPV는 workload controller나 scheduler를 대체하지 않으며 PV
node affinity를 변경하지 않는다.

이동은 영속 journal을 가진 단일 reconciler가 조정한다. Commit 전에는 source, commit 후에는
destination만 authoritative하며 불명확한 상태에서는 진행하지 않는다.

## Alternatives considered

- MutablePVNodeAffinity는 alpha 기능에 의존하고 장기 지원 경계가 불명확하다.
- Custom scheduler/plugin은 cluster-wide 구성과 결합도를 늘린다.
- Workload controller의 replica나 template을 직접 수정하면 Argo CD와 다른 operator 책임을 침범한다.

## Consequences

자동 이동은 지원 가능한 단일-consumer workload와 정상 source에 한정된다. Cordon은 이동
시작 신호일 뿐 shutdown 허가가 아니며, 이동 완료를 확인한 뒤 drain해야 한다.
